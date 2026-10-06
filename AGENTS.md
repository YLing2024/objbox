# AGENTS.md

本文件是本仓库的维护约定。协作者请先看 [`CONTRIBUTING.md`](CONTRIBUTING.md)。

## 项目结构

```
cmd/objbox/          单二进制入口：serve / account / presign 子命令
internal/account/    账号模型、accounts.json 读写、AK→账号索引、热重载
internal/auth/       SigV4 头签名与预签名校验、认证错误与 request-id
internal/backend/    POSIX 文件系统 + bbolt 的 S3 后端（每账号一个 Backend）
internal/server/     HTTP 装配：认证中间件 → 按账号分发；管理 API 与静态页
internal/admin/      管理面认证：AUTH_MODE builtin/sso、bcrypt、签名 cookie、限速、审计
internal/usage/      目录用量统计
internal/randstr/    随机串生成
web/                 React + Vite + TS 管理页；构建产物 web/dist 用 //go:embed 打进二进制
docs/                API 对照、决策记录、需求书
```

## 铁律

1. **协议端点永远走 AK/SK**，不受 `AUTH_MODE` 影响；`AUTH_MODE` 只作用于管理面。
2. **不得引入中间件**；HTTP 层只用标准库 `net/http`。
3. **账号隔离必须走 `backend.SafeJoin` 唯一入口**，不得绕开它拼接对象路径。
4. **新增 S3 能力要同步 `docs/API.md`**（支持清单、明确不支持清单、错误码）。
5. 源码中不得出现真实域名、公网 IP、密钥或邮箱；示例统一 `s3.example.com` / `127.0.0.1`。
6. 小步提交，conventional commits；不 `git push`，不开分支。

## 关键约束

- 元数据（ETag/Content-Type/自定义元数据/最后修改时间）在 bbolt，对象内容直接落盘；
  写入一律「临时文件 → fsync → rename」。
- `accounts.json` 含明文 SK（SigV4 要求），权限必须 0600；热重载解析失败保留旧表。
- 跨账号访问与"桶不存在"必须返回逐字节相同的 403。
- 前端读取 `window.__OBJBOX_AUTH_MODE__`；服务端注入占位符是 `__OBJBOX_AUTH_MODE_VALUE__`
  （变量名本身含 `__OBJBOX_AUTH_MODE__`，不要用同名占位符整体替换）。

## 常用命令

```bash
make build        # 前端 + go build
make test         # go test ./... -count=1
go test ./... -count=1 -race
make vet
make docker       # 构建镜像
```
