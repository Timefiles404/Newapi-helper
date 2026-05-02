package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	appName           = "xingkong-agent-helper"
	version           = "0.1.11"
	defaultAddr       = "127.0.0.1:8787"
	defaultMaxOutput  = 128 * 1024
	defaultTimeout    = 120 * time.Second
	maxTimeout        = 5 * time.Minute
	maxReadBytes      = 1024 * 1024
	maxSearchFiles    = 300
	maxSearchResults  = 50
	searchReadBytes   = 512 * 1024
	maxFSRequestBytes = 16 * 1024 * 1024
	agentDataDir      = ".xkagent"
	agentHistoryFile  = "playground-agent-conversations.json"
	helperStateFile   = "helper-state.json"
)

type helperState struct {
	LastURL string `json:"last_url"`
}

type server struct {
	addr          string
	workspace     string
	workspaceWarn string
	pairCode      string
	token         string
	tokenExpires  time.Time
	mu            sync.RWMutex
}

type statusResponse struct {
	App              string `json:"app"`
	Version          string `json:"version"`
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	Addr             string `json:"addr"`
	Workspace        string `json:"workspace"`
	WorkspaceWarning string `json:"workspace_warning,omitempty"`
	Shell            string `json:"shell"`
	Paired           bool   `json:"paired"`
	PairingRequired  bool   `json:"pairing_required"`
}

type execRequest struct {
	Command   string            `json:"command"`
	CWD       string            `json:"cwd"`
	TimeoutMS int               `json:"timeout_ms"`
	Env       map[string]string `json:"env"`
}

type execResponse struct {
	OK         bool   `json:"ok"`
	Command    string `json:"command"`
	CWD        string `json:"cwd"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type pairRequest struct {
	Code string `json:"code"`
}

type pairResponse struct {
	OK        bool   `json:"ok"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

type fsRequest struct {
	Op         string      `json:"op"`
	Path       string      `json:"path"`
	Content    string      `json:"content"`
	Query      string      `json:"query"`
	Start      int         `json:"start"`
	End        int         `json:"end"`
	MaxBytes   int         `json:"max_bytes"`
	MaxResults int         `json:"max_results"`
	Whole      bool        `json:"whole"`
	Edits      []batchEdit `json:"edits"`
}

type batchEdit struct {
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

type workspaceEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type fsResponse struct {
	OK      bool             `json:"ok"`
	Path    string           `json:"path"`
	Output  string           `json:"output,omitempty"`
	Summary string           `json:"summary,omitempty"`
	Entries []workspaceEntry `json:"entries,omitempty"`
	Error   string           `json:"error,omitempty"`
}

func main() {
	addr := flag.String("addr", defaultAddr, "listen address")
	workspace := flag.String("workspace", "", "workspace root for commands")
	pairCodeFlag := flag.String("pair-code", "", "override the helper-generated one-time pairing code")
	origins := flag.String("origins", "*", "deprecated; CORS now echoes any browser origin and protects exec with pairing")
	installProtocol := flag.Bool("install-protocol", false, "register xingkong-helper:// launcher protocol for the current executable")
	flag.Parse()
	_ = origins

	workspaceValue := strings.TrimSpace(*workspace)
	if workspaceValue == "" {
		workspaceValue = protocolWorkspace(flag.Args())
	}
	if workspaceValue == "" && flag.NArg() > 0 {
		firstArg := flag.Arg(0)
		if !strings.HasPrefix(strings.ToLower(firstArg), "xingkong-helper://") {
			workspaceValue = firstArg
		}
	}
	if workspaceValue == "" {
		workspaceValue = strings.TrimSpace(os.Getenv("XINGKONG_WORKSPACE"))
	}
	if workspaceValue == "" {
		workspaceValue = defaultWorkspace()
	}
	pairCode := strings.TrimSpace(*pairCodeFlag)
	if pairCode == "" {
		pairCode = protocolPairCode(flag.Args())
	}
	if pairCode == "" {
		pairCode = randomDigits(8)
	}

	root, err := filepath.Abs(workspaceValue)
	if err != nil {
		log.Fatalf("resolve workspace: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		log.Fatalf("workspace must be an existing directory: %s", root)
	}
	interactiveMode, activeURL := chooseInteractiveMode(root)

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("invalid addr: %v", err)
	}
	if host != "127.0.0.1" && host != "localhost" {
		log.Printf("warning: helper is listening on %s; 127.0.0.1 is strongly recommended", *addr)
	}
	if *installProtocol {
		if err := installLauncherProtocol(); err != nil {
			log.Printf("warning: failed to install launcher protocol: %v", err)
		}
	}

	s := &server{
		addr:          *addr,
		workspace:     root,
		workspaceWarn: workspaceWarning(root),
		pairCode:      pairCode,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/pair", s.handlePair)
	mux.HandleFunc("/v1/exec", s.handleExec)
	mux.HandleFunc("/v1/fs", s.handleFS)

	if interactiveMode == "active" || interactiveMode == "resume" {
		server := &http.Server{Addr: *addr, Handler: s.withCORS(mux)}
		go func() {
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("helper server failed: %v", err)
			}
		}()
		printStartupInfo(*addr, root, pairCode, s.workspaceWarn)
		if err := saveLastLaunchURL(root, activeURL); err != nil {
			log.Printf("warning: failed to save last NewAPI URL: %v", err)
		}
		target := buildActiveLaunchURL(activeURL, pairCode, interactiveMode == "resume")
		fmt.Printf("\n正在打开 NewAPI：%s\n", target)
		if err := openBrowser(target); err != nil {
			fmt.Printf("自动打开浏览器失败，请手动复制上面的地址打开：%v\n", err)
		}
		if interactiveMode == "resume" {
			fmt.Println("已进入延续对话模式。保持此窗口打开，网页会自动接续该工作目录的上次 Agent 会话。")
		} else {
			fmt.Println("已进入主动启动模式。保持此窗口打开，网页会自动新建 Agent 对话并完成配对。")
		}
		select {}
	}

	printStartupInfo(*addr, root, pairCode, s.workspaceWarn)
	log.Fatal(http.ListenAndServe(*addr, s.withCORS(mux)))
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	writeJSON(w, http.StatusOK, statusResponse{
		App:              appName,
		Version:          version,
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		Addr:             s.addr,
		Workspace:        s.workspace,
		WorkspaceWarning: s.workspaceWarn,
		Shell:            shellName(),
		Paired:           s.hasValidToken(""),
		PairingRequired:  true,
	})
}

func (s *server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	var req pairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	code := strings.TrimSpace(req.Code)
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == "" || s.pairCode == "" || code != s.pairCode {
		log.Printf("pair rejected: invalid code from %s", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_pair_code"})
		return
	}

	token := randomToken(32)
	s.token = token
	s.tokenExpires = time.Now().Add(24 * time.Hour)
	s.pairCode = ""
	log.Printf("pair accepted: token expires at %s", s.tokenExpires.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, pairResponse{
		OK:        true,
		Token:     token,
		ExpiresAt: s.tokenExpires.Format(time.RFC3339),
	})
}

func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if !s.hasValidToken(s.requestToken(r)) {
		log.Printf("exec rejected: helper not paired from %s", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "helper_not_paired"})
		return
	}

	var req execRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&req); err != nil {
		log.Printf("exec rejected: invalid json from %s", r.RemoteAddr)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		log.Printf("exec rejected: empty command from %s cwd=%q", r.RemoteAddr, req.CWD)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "command_required"})
		return
	}

	cwd, err := s.resolveCWD(req.CWD)
	if err != nil {
		log.Printf("exec rejected: invalid cwd from %s cwd=%q error=%s", r.RemoteAddr, req.CWD, err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	timeout := defaultTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}

	started := time.Now()
	log.Printf("exec start: cwd=%s command=%q timeout=%s", cwd, req.Command, timeout)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	cmd := shellCommand(ctx, req.Command)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for key, value := range req.Env {
		if isSafeEnvKey(key) {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	resp := execResponse{
		Command:    req.Command,
		CWD:        cwd,
		DurationMS: time.Since(started).Milliseconds(),
	}
	resp.Stdout, resp.Truncated = truncateOutput(stdout.String(), defaultMaxOutput)
	stderrText, stderrTruncated := truncateOutput(stderr.String(), defaultMaxOutput)
	resp.Stderr = stderrText
	resp.Truncated = resp.Truncated || stderrTruncated

	if ctx.Err() == context.DeadlineExceeded {
		resp.ExitCode = -1
		resp.Error = "command_timeout"
		log.Printf("exec done: exit=%d duration=%dms error=%s command=%q", resp.ExitCode, resp.DurationMS, resp.Error, req.Command)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			resp.ExitCode = exitErr.ExitCode()
			resp.Error = fmt.Sprintf("exit status %d", resp.ExitCode)
		} else {
			resp.ExitCode = -1
			resp.Error = err.Error()
		}
		log.Printf("exec done: exit=%d duration=%dms error=%s command=%q", resp.ExitCode, resp.DurationMS, resp.Error, req.Command)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.OK = true
	resp.ExitCode = 0
	log.Printf("exec done: exit=%d duration=%dms command=%q", resp.ExitCode, resp.DurationMS, req.Command)
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleFS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if !s.hasValidToken(s.requestToken(r)) {
		log.Printf("fs rejected: helper not paired from %s", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "helper_not_paired"})
		return
	}

	var req fsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFSRequestBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	resp, err := s.executeFS(req)
	if err != nil {
		log.Printf("fs failed: op=%s path=%q error=%s", req.Op, req.Path, err)
		writeJSON(w, http.StatusOK, fsResponse{
			OK:    false,
			Path:  cleanDisplayPath(req.Path),
			Error: err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) executeFS(req fsRequest) (fsResponse, error) {
	op := strings.TrimSpace(req.Op)
	path := cleanDisplayPath(req.Path)
	if path == "" {
		path = "."
	}

	switch op {
	case "list_dir":
		fullPath, err := s.resolveWorkspacePath(path, true)
		if err != nil {
			return fsResponse{}, err
		}
		entries, output, err := listDirectory(fullPath, path)
		if err != nil {
			return fsResponse{}, err
		}
		return fsResponse{OK: true, Path: path, Entries: entries, Output: output}, nil
	case "read_file":
		fullPath, err := s.resolveWorkspacePath(path, false)
		if err != nil {
			return fsResponse{}, err
		}
		output, summary, err := readTextFile(fullPath, req)
		if err != nil {
			return fsResponse{}, err
		}
		return fsResponse{OK: true, Path: path, Output: output, Summary: summary}, nil
	case "search_files":
		if strings.TrimSpace(req.Query) == "" {
			return fsResponse{}, errors.New("search_query_required")
		}
		fullPath, err := s.resolveWorkspacePath(path, true)
		if err != nil {
			return fsResponse{}, err
		}
		output, summary, err := searchWorkspaceFiles(fullPath, path, req.Query, req.MaxResults)
		if err != nil {
			return fsResponse{}, err
		}
		return fsResponse{OK: true, Path: path, Output: output, Summary: summary}, nil
	case "write_file":
		fullPath, err := s.resolveWorkspacePath(path, false)
		if err != nil {
			return fsResponse{}, err
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return fsResponse{}, err
		}
		if err := os.WriteFile(fullPath, []byte(req.Content), 0o644); err != nil {
			return fsResponse{}, err
		}
		return fsResponse{OK: true, Path: path, Output: "written", Summary: fmt.Sprintf("%d chars written", len(req.Content))}, nil
	case "append_file":
		fullPath, err := s.resolveWorkspacePath(path, false)
		if err != nil {
			return fsResponse{}, err
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return fsResponse{}, err
		}
		file, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fsResponse{}, err
		}
		defer file.Close()
		if _, err := file.WriteString(req.Content); err != nil {
			return fsResponse{}, err
		}
		return fsResponse{OK: true, Path: path, Output: "appended", Summary: fmt.Sprintf("%d chars appended", len(req.Content))}, nil
	case "batch_edit":
		if len(req.Edits) == 0 {
			return fsResponse{}, errors.New("batch_edit_requires_edits")
		}
		fullPath, err := s.resolveWorkspacePath(path, false)
		if err != nil {
			return fsResponse{}, err
		}
		contentBytes, err := os.ReadFile(fullPath)
		if err != nil {
			return fsResponse{}, err
		}
		if len(contentBytes) > maxReadBytes {
			return fsResponse{}, errors.New("file_too_large_for_batch_edit")
		}
		content := string(contentBytes)
		applied := make([]string, 0, len(req.Edits))
		appliedCount := 0
		for index, edit := range req.Edits {
			if edit.Find == "" || !strings.Contains(content, edit.Find) {
				applied = append(applied, fmt.Sprintf("#%d: not found", index+1))
				continue
			}
			content = strings.Replace(content, edit.Find, edit.Replace, 1)
			applied = append(applied, fmt.Sprintf("#%d: applied", index+1))
			appliedCount++
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			return fsResponse{}, err
		}
		return fsResponse{OK: true, Path: path, Output: strings.Join(applied, "\n"), Summary: fmt.Sprintf("%d/%d edits applied", appliedCount, len(req.Edits))}, nil
	case "create_dir":
		fullPath, err := s.resolveWorkspacePath(path, true)
		if err != nil {
			return fsResponse{}, err
		}
		if err := os.MkdirAll(fullPath, 0o755); err != nil {
			return fsResponse{}, err
		}
		return fsResponse{OK: true, Path: path, Output: "created", Summary: "directory created"}, nil
	case "reveal_path":
		fullPath, err := s.resolveWorkspacePath(path, false)
		if err != nil {
			return fsResponse{}, err
		}
		if err := revealPath(fullPath); err != nil {
			return fsResponse{}, err
		}
		return fsResponse{OK: true, Path: path, Output: "opened", Summary: "opened in file manager"}, nil
	case "agent_history_load":
		content, err := s.readAgentHistory()
		if err != nil {
			return fsResponse{}, err
		}
		return fsResponse{OK: true, Path: agentDataDir + "/" + agentHistoryFile, Output: content, Summary: "agent history loaded"}, nil
	case "agent_history_save":
		if err := s.writeAgentHistory(req.Content); err != nil {
			return fsResponse{}, err
		}
		return fsResponse{OK: true, Path: agentDataDir + "/" + agentHistoryFile, Output: "saved", Summary: "agent history saved"}, nil
	default:
		return fsResponse{}, errors.New("unsupported_fs_op")
	}
}

func (s *server) agentHistoryPath() string {
	return filepath.Join(s.workspace, agentDataDir, agentHistoryFile)
}

func (s *server) readAgentHistory() (string, error) {
	path := s.agentHistoryPath()
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return `{"conversations":[],"activeConversationId":null}`, nil
	}
	if err != nil {
		return "", err
	}
	if !json.Valid(content) {
		return "", errors.New("agent_history_invalid_json")
	}
	return string(content), nil
}

func (s *server) writeAgentHistory(content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		content = `{"conversations":[],"activeConversationId":null}`
	}
	if !json.Valid([]byte(content)) {
		return errors.New("agent_history_invalid_json")
	}
	path := s.agentHistoryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func (s *server) resolveCWD(value string) (string, error) {
	if strings.TrimSpace(value) == "" || value == "." {
		return s.workspace, nil
	}
	if filepath.IsAbs(value) {
		return "", errors.New("absolute_cwd_not_allowed")
	}
	clean := filepath.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("cwd_outside_workspace")
	}
	joined, err := filepath.Abs(filepath.Join(s.workspace, clean))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(s.workspace, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("cwd_outside_workspace")
	}
	if info, err := os.Stat(joined); err != nil || !info.IsDir() {
		return "", errors.New("cwd_not_found")
	}
	return joined, nil
}

func (s *server) resolveWorkspacePath(value string, allowDir bool) (string, error) {
	clean := cleanDisplayPath(value)
	if clean == "" || clean == "." {
		if allowDir {
			return s.workspace, nil
		}
		return "", errors.New("file_path_required")
	}
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "/") {
		return "", errors.New("absolute_path_not_allowed")
	}
	parts := strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' })
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("path_traversal_not_allowed")
		}
	}
	joined, err := filepath.Abs(filepath.Join(append([]string{s.workspace}, parts...)...))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(s.workspace, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path_outside_workspace")
	}
	return joined, nil
}

func (s *server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Xingkong-Helper-Token")
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *server) requestToken(r *http.Request) string {
	token := strings.TrimSpace(r.Header.Get("X-Xingkong-Helper-Token"))
	if token != "" {
		return token
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func (s *server) hasValidToken(value string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.token == "" || time.Now().After(s.tokenExpires) {
		return false
	}
	if value == "" {
		return true
	}
	return value == s.token
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd.exe", "/C", command)
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return exec.CommandContext(ctx, shell, "-lc", command)
}

func shellName() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func isSafeEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

func chooseInteractiveMode(root string) (string, string) {
	if len(os.Args) > 1 || strings.TrimSpace(os.Getenv("XINGKONG_HELPER_NO_MENU")) != "" {
		return "", ""
	}
	lastURL := readLastLaunchURL(root)
	fmt.Println("星空 Agent Helper")
	fmt.Println("当前目录将作为 Agent 工作目录。")
	fmt.Println()
	fmt.Println("请选择启动模式：")
	fmt.Println("1. 配对模式：显示配对 key，打开 NewAPI 后手动输入配对")
	fmt.Println("2. 主动启动模式：粘贴 NewAPI 网址，自动打开页面、新建 Agent 对话并静默配对")
	if lastURL != "" {
		fmt.Printf("3. 延续上一次对话：打开 %s 并接续当前工作目录的历史会话\n", lastURL)
		fmt.Print("请输入 1、2 或 3 后回车（默认 1）：")
	} else {
		fmt.Print("请输入 1 或 2 后回车（默认 1）：")
	}

	reader := bufio.NewReader(os.Stdin)
	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)
	if choice == "3" && lastURL != "" {
		return "resume", lastURL
	}
	if choice != "2" {
		return "pair", ""
	}
	for {
		fmt.Print("请输入 NewAPI 网址后回车：")
		target, _ := reader.ReadString('\n')
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		return "active", target
	}
}

func readLastLaunchURL(root string) string {
	content, err := os.ReadFile(filepath.Join(root, agentDataDir, helperStateFile))
	if err != nil {
		return ""
	}
	var state helperState
	if err := json.Unmarshal(content, &state); err != nil {
		return ""
	}
	return strings.TrimSpace(state.LastURL)
}

func saveLastLaunchURL(root, rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/playground/"
	}
	content, err := json.MarshalIndent(helperState{LastURL: parsed.String()}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(root, agentDataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, helperStateFile), content, 0o600)
}

func printStartupInfo(addr, root, pairCode, warning string) {
	log.Printf("%s %s listening on http://%s", appName, version, addr)
	log.Printf("workspace: %s", root)
	log.Printf("CORS: any browser origin is accepted; /v1/exec and /v1/fs require pairing")
	log.Printf("pairing code: %s", pairCode)
	fmt.Println()
	fmt.Printf("工作目录：%s\n", root)
	fmt.Printf("配对 key：%s\n", pairCode)
	fmt.Println("保持此窗口打开，网页端 Agent 才能调用本地工具。")
	if warning != "" {
		log.Printf("warning: %s", warning)
		log.Printf("restart example: xingkong-helper.exe --workspace \"D:\\your-project\" --pair-code %s", pairCode)
	}
}

func buildActiveLaunchURL(rawURL, pairCode string, resume bool) string {
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/playground/"
	}
	query := parsed.Query()
	query.Set("xingkong_agent_mode", "1")
	query.Set("xingkong_helper_pair_code", pairCode)
	query.Set("xingkong_helper_autostart", "1")
	if resume {
		query.Set("xingkong_helper_resume", "1")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func openBrowser(target string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	case "darwin":
		return exec.Command("open", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

func revealPath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	target := path
	if !info.IsDir() {
		target = filepath.Dir(path)
	}
	switch runtime.GOOS {
	case "windows":
		if !info.IsDir() {
			return exec.Command("explorer.exe", "/select,"+path).Start()
		}
		return exec.Command("explorer.exe", target).Start()
	case "darwin":
		if !info.IsDir() {
			return exec.Command("open", "-R", path).Start()
		}
		return exec.Command("open", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

func cleanDisplayPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return "."
	}
	return strings.TrimPrefix(value, "./")
}

func listDirectory(fullPath, displayPath string) ([]workspaceEntry, string, error) {
	items, err := os.ReadDir(fullPath)
	if err != nil {
		return nil, "", err
	}
	entries := make([]workspaceEntry, 0, len(items))
	lines := make([]string, 0, len(items))
	base := cleanDisplayPath(displayPath)
	if base == "." {
		base = ""
	}
	for _, item := range items {
		if item.Name() == agentDataDir {
			continue
		}
		kind := "file"
		prefix := "file"
		if item.IsDir() {
			kind = "directory"
			prefix = "dir "
		}
		entryPath := strings.TrimPrefix(base+"/"+item.Name(), "/")
		entries = append(entries, workspaceEntry{Name: item.Name(), Path: entryPath, Kind: kind})
		lines = append(lines, fmt.Sprintf("%s\t%s", prefix, item.Name()))
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind == "directory"
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	sort.Strings(lines)
	if len(lines) == 0 {
		return entries, "(empty directory)", nil
	}
	return entries, strings.Join(lines, "\n"), nil
}

func readTextFile(fullPath string, req fsRequest) (string, string, error) {
	info, err := os.Stat(fullPath)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		return "", "", errors.New("path_is_directory")
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 || maxBytes > maxReadBytes {
		maxBytes = maxReadBytes
	}
	contentBytes, err := os.ReadFile(fullPath)
	if err != nil {
		return "", "", err
	}
	truncatedBytes := false
	if len(contentBytes) > maxBytes {
		contentBytes = contentBytes[:maxBytes]
		truncatedBytes = true
	}
	text := string(contentBytes)
	if req.Whole {
		if truncatedBytes {
			text += fmt.Sprintf("\n\n[truncated: %d/%d bytes]", maxBytes, info.Size())
		}
		return text, "whole file read", nil
	}
	lines := strings.Split(text, "\n")
	start := req.Start
	if start <= 0 {
		start = 1
	}
	end := req.End
	if end <= 0 {
		end = start + 99
	}
	if start > len(lines) {
		return "", "0 lines read", nil
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end < start {
		end = start
	}
	out := make([]string, 0, end-start+1)
	for i := start; i <= end; i++ {
		out = append(out, fmt.Sprintf("%d: %s", i, strings.TrimSuffix(lines[i-1], "\r")))
	}
	summary := fmt.Sprintf("lines %d-%d", start, end)
	if truncatedBytes {
		out = append(out, "")
		out = append(out, fmt.Sprintf("[truncated: %d/%d bytes]", maxBytes, info.Size()))
	} else if end < len(lines) {
		out = append(out, "")
		out = append(out, fmt.Sprintf("[truncated: %d/%d lines]", end, len(lines)))
	}
	return strings.Join(out, "\n"), summary, nil
}

func searchWorkspaceFiles(root, displayPath, query string, maxResults int) (string, string, error) {
	if maxResults <= 0 || maxResults > maxSearchResults {
		maxResults = 20
	}
	queryLower := strings.ToLower(query)
	results := make([]string, 0, maxResults)
	scanned := 0
	base := cleanDisplayPath(displayPath)
	if base == "." {
		base = ""
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || len(results) >= maxResults || scanned >= maxSearchFiles {
			return nil
		}
		if d.IsDir() {
			if d.Name() == agentDataDir || d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !isSearchableFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > searchReadBytes {
			return nil
		}
		scanned++
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if base != "" {
			rel = strings.TrimPrefix(base+"/"+rel, "/")
		}
		for index, line := range strings.Split(string(content), "\n") {
			if len(results) >= maxResults {
				break
			}
			if strings.Contains(strings.ToLower(line), queryLower) {
				results = append(results, fmt.Sprintf("%s:%d: %s", rel, index+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if len(results) == 0 {
		return "no matches", fmt.Sprintf("0 matches in %d scanned files", scanned), nil
	}
	return strings.Join(results, "\n"), fmt.Sprintf("%d matches in %d scanned files", len(results), scanned), nil
}

func isSearchableFile(name string) bool {
	lower := strings.ToLower(name)
	exts := []string{".txt", ".md", ".markdown", ".json", ".jsonl", ".csv", ".tsv", ".yaml", ".yml", ".xml", ".html", ".htm", ".css", ".scss", ".js", ".jsx", ".ts", ".tsx", ".py", ".go", ".java", ".c", ".cpp", ".h", ".hpp", ".rs", ".sh", ".sql", ".log", ".toml", ".ini", ".env"}
	for _, ext := range exts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func truncateOutput(value string, maxBytes int) (string, bool) {
	if len(value) <= maxBytes {
		return value, false
	}
	return value[:maxBytes] + "\n[truncated]", true
}

func defaultWorkspace() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

func protocolWorkspace(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(strings.ToLower(arg), "xingkong-helper://") {
			continue
		}
		parsed, err := url.Parse(arg)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(parsed.Query().Get("workspace"))
	}
	return ""
}

func protocolPairCode(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(strings.ToLower(arg), "xingkong-helper://") {
			continue
		}
		parsed, err := url.Parse(arg)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(parsed.Query().Get("pair_code"))
	}
	return ""
}

func randomDigits(length int) string {
	if length <= 0 {
		length = 8
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%0*d", length, time.Now().UnixNano()%100000000)
	}
	for i := range buf {
		buf[i] = '0' + (buf[i] % 10)
	}
	return string(buf)
}

func randomToken(bytesLen int) string {
	if bytesLen <= 0 {
		bytesLen = 32
	}
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func installLauncherProtocol() error {
	if runtime.GOOS != "windows" {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	commands := [][]string{
		{"add", `HKCU\Software\Classes\xingkong-helper`, "/ve", "/d", "URL:Xingkong Agent Helper", "/f"},
		{"add", `HKCU\Software\Classes\xingkong-helper`, "/v", "URL Protocol", "/d", "", "/f"},
		{"add", `HKCU\Software\Classes\xingkong-helper\DefaultIcon`, "/ve", "/d", exe, "/f"},
		{"add", `HKCU\Software\Classes\xingkong-helper\shell\open\command`, "/ve", "/d", `"` + exe + `" "%1"`, "/f"},
	}
	for _, args := range commands {
		if output, err := exec.Command("reg", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("reg %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func workspaceWarning(root string) string {
	lower := strings.ToLower(filepath.Clean(root))
	temp := strings.ToLower(filepath.Clean(os.TempDir()))
	if temp != "" && (lower == temp || strings.HasPrefix(lower, temp+string(filepath.Separator))) {
		return "workspace is under the system temp directory; start helper with --workspace pointing to the project directory"
	}
	return ""
}
