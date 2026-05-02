# Xingkong Agent Helper

本地 Agent Helper 用于配合远端星空网页端 Agent 模式，在用户明确授权后执行本机命令行任务。

## 使用

```bash
go run ./cmd/xingkong-helper --workspace /path/to/project
```

默认监听 `127.0.0.1:8787`，只允许 `https://new.xingkongai.online`、`http://localhost:*`、`http://127.0.0.1:*` 这些来源访问。

## 接口

- `GET /v1/status`: 检查 helper 是否在线。
- `POST /v1/exec`: 执行命令。

`/v1/exec` 请求示例：

```json
{
  "command": "go test ./...",
  "cwd": ".",
  "timeout_ms": 120000
}
```

命令会被限制在 `--workspace` 指定的目录内运行。

## 安全边界

- 默认只绑定 `127.0.0.1`，不对局域网开放。
- 默认只接受允许列表里的 Origin。
- 网页端会在执行命令前弹出工具审批，不静默执行。
- 不建议用管理员/root 权限启动 helper。
