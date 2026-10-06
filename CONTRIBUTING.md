# 贡献指南

感谢参与。提 Issue 与 PR 前请先阅读本页。

## 提 Issue

- 请说明：objbox 版本（`objbox --help` 或管理页 overview 的 version）、运行方式（源码 / Docker）、
  复现步骤、期望行为与实际行为。
- 涉及协议问题时，附上客户端（rclone / aws cli / mc）与完整命令；**不要粘贴真实 SK**，
  用 `AKEXAMPLE...` / `SKEXAMPLE...` 代替。
- 报告安全问题时请勿公开细节，先通过私密渠道联系维护者。

## 本地开发

需要 Go 1.24+、Node.js 22+。

```bash
make build      # 构建 web/ 并产出 ./objbox（前端用 //go:embed 打进二进制）
make test       # go test ./... -count=1
go test ./... -count=1 -race   # 提交前建议跑一遍竞态
make vet        # go vet ./...
```

只改后端时可先 `cd web && npm run build` 生成一次 `web/dist`，之后 `go build ./cmd/objbox`。

## 提交规范

使用 [Conventional Commits](https://www.conventionalcommits.org/)：

```
<type>(<scope>): <subject>
```

常用 type：`feat` / `fix` / `docs` / `test` / `build` / `refactor`。scope 可用包名，
如 `server` / `backend` / `auth` / `admin` / `cli`。一次提交只做一件事，能过 `go test`。

## 代码约定

- 协议端点永远走 AK/SK，不受 `AUTH_MODE` 影响。
- 不引入中间件；HTTP 只用标准库。
- 账号隔离必须经过 `backend.SafeJoin` 唯一入口。
- 新增 S3 能力要同步更新 `docs/API.md`。
- 源码中不得出现真实域名、公网 IP、密钥或邮箱；示例一律用 `s3.example.com`、`127.0.0.1`。
