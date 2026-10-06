# objbox 需求书 · M4（交付形态 + 双语文档 + 开源准备）

> 自包含文档。前置：M0/M1/M3 已完成（账号/CLI/热重载/隔离/S3 协议集齐/预签名/配额/Web 管理页/AUTH_MODE）。
> 本阶段**不加新功能**，只做交付形态与文档，让项目达到"可开源"的完成度。
> 铁律同前：小步 commit、禁 push/分支/rebase、禁 systemctl/kill、只改本仓库、冲突以仓库为准不提问、
> **仓库内不得出现真实域名 / IP / 密钥 / 邮箱**（示例一律 `s3.example.com`、`127.0.0.1`）。

## 1. Docker 交付

1. `Dockerfile`（多阶段）：
   - 阶段一：`node:22-alpine` 构建 `web/`（`npm ci && npm run build`）
   - 阶段二：`golang:1.24-alpine` 把 `web/dist` 拷进来后 `go build`（**先把 dist 放到 embed 路径再编译**，保证前端进二进制）
   - 阶段三：`alpine:3.20` 仅拷二进制 + 证书包，`USER` 用非 root（uid 10001），`VOLUME /data`，`EXPOSE 18930`
   - `ENTRYPOINT`：`/usr/local/bin/objbox serve -addr 0.0.0.0:18930 -data /data`
   - 目标：最终镜像 **< 30 MB**（写进报告实测值 `docker images` 大小）
2. `docker-compose.yml`（最小示例）：单服务、映射端口、挂载 `./data:/data`、声明 `AUTH_MODE=builtin` 与数据目录，注释写清"数据卷必须持久化"。
3. `Makefile` 增加：`docker`（构建镜像）、`docker-run`（前台跑）、`image-size`（打印镜像体积）。
4. `.dockerignore`：排除 `.git`、`node_modules`、`data/`、`objbox`（本地二进制）、`web/dist`。

**验证（写进报告）**：`docker build` 成功 + `docker images` 体积数字 + 起容器后 `curl` 未签名请求得 403 + 容器内 `objbox account add` 能建账号（挂载数据卷）。

## 2. 双语 README（中文主 + 英文副）

- `README.md`（中文）与 `README.en.md`（英文）**结构一一对应**；首行放切换行：`[简体中文](README.md) ｜ [English](README.en.md)`
- 内容要求（两版一致）：
  1. 一句话定位 + "为什么自己做"（对照 MinIO/Garage/SeaweedFS 那类平台型方案，说明 objbox 只补"账号 + AK/SK + 隔离"这一薄层）
  2. 特性清单（账号隔离、AK/SK、只读与停用、热重载、S3 子集、预签名、分片、配额、管理页、单二进制）
  3. 快速开始（源码构建 + Docker 两种）
  4. 支持的 S3 操作（**明确列出不做**的：版本控制/对象锁/生命周期/ACL/Policy/事件通知/跨区复制/STS/S3 Select）
  5. 用法示例：rclone、aws cli、mc（三选二即可，示例里的 endpoint 用 `s3.example.com`）
  6. 配置说明（环境变量表：`AUTH_MODE`、数据目录、监听地址、日志开关等）
  7. 安全模型（隔离规则、403 统一语义、SK 明文落盘的取舍与原因、建议：只在自己机器/内网使用、配合 HTTPS）
  8. MIT 许可与贡献说明
- **风格**：克制、无 emoji、无营销腔、不出现"极致/颠覆/赋能"这类词。
- 徽章只用 shields.io 的 license 徽章（可选）。

## 3. 开源准备

1. `LICENSE` 已有（MIT）——确认版权行为 `Copyright (c) 2026 YLing2024`
2. `CONTRIBUTING.md`（简短）：如何提 issue、如何本地开发（`make build` / `go test`）、提交信息规范（conventional commits）
3. `AGENTS.md`：给后续 AI 协作的说明（项目结构、铁律：协议端点永远 AK/SK、不得引入中间件、隔离必须走 SafeJoin 唯一入口、新增 S3 能力要同步 `docs/API.md`）
4. `docs/API.md` 与 `docs/DECISIONS.md`（记录关键取舍：403 统一语义、SK 明文、bbolt 索引可重建、只读账号与磁盘为准的桶发现）
5. `.env.example`：列出所有支持的环境变量（不含任何真实值）
6. **隐私扫描（必须为 0）**：`grep -rInE "example.com|yling\.site|223\.254|24\.233|203\.0\.113|@gmail\.com" --exclude-dir=.git .`；
   另扫：私钥/令牌样式（`sk-[A-Za-z0-9]{20,}`、`AKIA[0-9A-Z]{16}`、`BEGIN .*PRIVATE KEY`）——报告里贴命中数。
7. **全历史扫描**：对 `git log --all -p` 同样做一遍上面两种扫描（防止早期提交里残留），命中数写进报告。

## 5. 顺带修的 CLI 缺陷（Hermes 验收时实测踩到）

1. **账号名不校验 → 静默产生垃圾账号**：`./objbox account add -data /tmp/x alpha` 会把 **`-data` 当账号名**建成账号（并存进默认数据目录），而用户的真实意图是传数据目录。要求：
   - 账号名必须校验 `^[a-z0-9][a-z0-9-]{0,31}$`（**不得以 `-` 开头**）；不合法 → 打印明确错误 + 用法提示 + 非零退出码，**绝不创建账号**
   - `account add` 的成功输出里打印**实际使用的数据目录**（避免"以为写到 A、实际写到 B"）
2. **`-data` 位置陷阱**：子命令的 `-data` 只在名字之后才被解析（`flag` 包的既有行为）。要求：在用法文本里写明确切形式 `objbox account add <name> [-note ...] [-readonly] [-data DIR]`，并在账号名不合法时把该用法原样提示出来。
3. **默认数据目录副作用**：任何子命令都不应在未显式指定 `-data` 时于系统路径（如 `/var/lib/objbox`）**静默创建**新目录后写入——要么沿用既有目录，要么在输出里明确提示"正在使用默认数据目录 X"。

以上三条都要补测试（非法名被拒、合法名通过、默认目录提示存在）。

## 5b. 另两处 Hermes 验收发现的缺陷

4. **`logout` 没有服务端失效（安全）**：当前 `adminLogout` 只 `ClearSessionCookie`，而会话令牌是无状态签名 `base64(user|expiry).HMAC`，**登出后旧 cookie 在有效期内仍可通过校验**（Hermes 实测：logout 后带旧 cookie 访问 `/api/admin/overview` 依旧 200）。要求：
   - 在 `<data>/admin.json` 里维护一个 **session epoch/版本号**（登录成功时写入签发时的 epoch，校验时要求与当前 epoch 相等）
   - `logout` 时 epoch +1（原子写），**使所有既有会话立即失效**（builtin 模式单管理员场景这样做是可接受的）
   - 补测试：登录 → 拿到 cookie → logout → 同一 cookie 再访问必须 **401**
5. **数据目录被占用时报错含糊**：同一数据目录起第二个实例，报 `backend: 打开元数据库失败: timeout`（bbolt 文件锁），用户看不出是"目录已被占用"。要求：把该错误归一为可读文案（如"数据目录 X 已被另一个 objbox 进程占用（元数据库被锁定）"），并在退出码上保持非零。
6. **`AUTH_MODE` 注入变量名与前端读取不一致（真 bug）**：前端 `web/src/App.tsx` 读的是 `window.__OBJBOX_AUTH_MODE__`，而服务端注入到 HTML 的却是 `window.builtin = 'builtin'`（键名被写成了模式值）。后果：**sso 模式下界面永远误判为 builtin**，会渲染出登录表单/错误标识。要求：统一为 `window.__OBJBOX_AUTH_MODE__ = '<mode>'`，补一个测试断言注入内容包含正确变量名与取值。
7. **登录 cookie 缺 `Secure` 属性**：口令登录下发的 `objbox_admin` cookie 只有 `HttpOnly`，在 HTTPS 部署下应同时带 `Secure`（拿到请求 scheme 为 https 或 `X-Forwarded-Proto: https` 时设置；本地 http 调试时不设，避免登录后无法保持会话）。补测试覆盖两种情形。
8. **`X-Forwarded-For` 的信任边界**：服务端 `ClientIP` 优先取 XFF（这是为网关反代设计的）。请在 `docs/` 或 README 安全小节**明确写出**："直接暴露公网时必须由前置代理覆盖 XFF（否则登录限速可被伪造绕过）"，并给出 nginx 示例（`proxy_set_header X-Forwarded-For $remote_addr;`）。

## 6. 交付报告
写到 `/tmp/objbox-m4-report.md` 并 `cat`：Docker 构建与体积实测、双语 README 文件大小、隐私扫描与全历史扫描命中数、`git log --oneline` 每笔对应改动点、以及"是否建议转公开"的结论（你自己判断，Hermes 会复核）。

> 注意：**不要**执行 `gh repo edit --visibility public`（转公开由 Hermes 在用户确认后做）。
