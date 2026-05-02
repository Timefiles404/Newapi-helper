package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	appName          = "xingkong-agent-helper"
	version          = "0.1.0"
	defaultAddr      = "127.0.0.1:8787"
	defaultMaxOutput = 128 * 1024
	defaultTimeout   = 120 * time.Second
	maxTimeout       = 5 * time.Minute
)

type server struct {
	addr           string
	workspace      string
	allowedOrigins []string
}

type statusResponse struct {
	App       string `json:"app"`
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Addr      string `json:"addr"`
	Workspace string `json:"workspace"`
	Shell     string `json:"shell"`
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

func main() {
	addr := flag.String("addr", defaultAddr, "listen address")
	workspace := flag.String("workspace", ".", "workspace root for commands")
	origins := flag.String("origins", "https://new.xingkongai.online,http://localhost:3000,http://127.0.0.1:3000", "comma separated allowed origins")
	flag.Parse()

	root, err := filepath.Abs(*workspace)
	if err != nil {
		log.Fatalf("resolve workspace: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		log.Fatalf("workspace must be an existing directory: %s", root)
	}

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("invalid addr: %v", err)
	}
	if host != "127.0.0.1" && host != "localhost" {
		log.Printf("warning: helper is listening on %s; 127.0.0.1 is strongly recommended", *addr)
	}

	s := &server{
		addr:           *addr,
		workspace:      root,
		allowedOrigins: splitCSV(*origins),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/exec", s.handleExec)

	log.Printf("%s %s listening on http://%s", appName, version, *addr)
	log.Printf("workspace: %s", root)
	log.Fatal(http.ListenAndServe(*addr, s.withCORS(mux)))
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	writeJSON(w, http.StatusOK, statusResponse{
		App:       appName,
		Version:   version,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Addr:      s.addr,
		Workspace: s.workspace,
		Shell:     shellName(),
	})
}

func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	var req execRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "command_required"})
		return
	}

	cwd, err := s.resolveCWD(req.CWD)
	if err != nil {
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
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.OK = true
	resp.ExitCode = 0
	writeJSON(w, http.StatusOK, resp)
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

func (s *server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.isOriginAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}

		if r.Method == http.MethodOptions {
			if origin == "" || !s.isOriginAllowed(origin) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if origin != "" && !s.isOriginAllowed(origin) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin_not_allowed"})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *server) isOriginAllowed(origin string) bool {
	for _, allowed := range s.allowedOrigins {
		if allowed == origin {
			return true
		}
		if strings.HasSuffix(allowed, ":*") {
			prefix := strings.TrimSuffix(allowed, "*")
			if strings.HasPrefix(origin, prefix) {
				return true
			}
		}
	}
	return false
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

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
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

func truncateOutput(value string, maxBytes int) (string, bool) {
	if len(value) <= maxBytes {
		return value, false
	}
	return value[:maxBytes] + "\n[truncated]", true
}
