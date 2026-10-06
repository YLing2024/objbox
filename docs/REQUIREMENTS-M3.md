# objbox 需求书 · M3（Web 管理页 + SSO 对接）

> 自包含文档。前置：M0/M1 已完成（账号、CLI、热重载、SigV4 header+query 校验、隔离、Bucket/Object CRUD、
> 列表 V1&V2、Range、分片上传、批量删、CopyObject、预签名、配额）。
> 本阶段**只加管理面**，不动物理数据层；M0/M1 的测试必须继续全绿。
> 目标：像 davbox 那样，"后端自带前端静态服务"，nginx 只做反代。

## 0. 铁律（与前两阶段相同）

1. 每完成一个逻辑单元就 `git commit` 一次（conventional commits）；**禁止** `git push`、`git checkout -b`、`rebase`、`reset --hard`、`merge`；就在 `main`。
2. 禁止 `systemctl` / `service` / `kill` / `pkill` / `killall` / `fuser -k`；禁止改 `/root/proj/objbox` 之外的路径；命令带 `timeout`。
3. 遇到文档与仓库现实冲突以**仓库现实为准**，直接实现，不要提问。
4. 仓库内不得出现真实域名 / IP / 密钥；示例用 `s3.example.com` 占位。
5. **认证模式铁律（沿用 davbox）**：**S3 协议端点永远走 AK/SK，不受 `AUTH_MODE` 影响**；`AUTH_MODE` 只作用于**管理面**。

## 1. 管理面认证（两模式）

环境变量 `AUTH_MODE`：
- `builtin`（默认）：管理页用**自带管理员口令**登录。
  - 首次启动生成随机口令，明文写入 `<data>/admin-password.txt`（0600），口令的 **bcrypt 哈希**存 `<data>/admin.json`
  - 会话 cookie 用 `<data>/secret.key`（首次生成，0600）签名；cookie 名 `objbox_admin`，属性 `HttpOnly; SameSite=Lax; Path=/`（**不要**加 `__Host-` 前缀，避免与 nginx 反代场景打架），TTL 12 小时
  - 登录接口限速：同一 IP 每分钟最多 10 次失败尝试，超出 → 429
- `sso`：管理面**不做任何自带登录**，只信任网关注入的 `X-Auth-User` 头；**缺失该头一律 401**（不能因为"没配 SSO"就放行）
- 两种模式下：`/api/admin/*` 与除登录页以外的管理页面都必须鉴权

## 2. 前端（React + Vite + TS，产物 `//go:embed`）

目录 `web/`，与 davbox 一致：`npm run build` 产物进 `web/dist`，用 `//go:embed all:dist` 打进二进制；
`Makefile` 的 `build` 目标先构建前端再 `go build`。**不要引入 UI 框架**（不要 AntD/MUI），原生 CSS 即可，风格克制、无 emoji、无营销文案。

页面（单页应用，路由用 hash）：
1. **登录页**（`builtin` 模式）：口令输入框 + 登录按钮；错误提示简洁；`sso` 模式下此页不存在（访问即 401）
2. **账号列表**（主页面）：
   - 每行：账号名、AK（完整显示，可一键复制）、SK（默认掩码，点「显示」才拉取明文，可复制）、root 路径、用量、状态（启用/停用/只读）、备注
   - 行操作：**轮换 SK**（二次确认，成功后**弹窗只显示一次**新 SK + 复制按钮 + 明确提示"关闭后不再显示"）、启用/停用、编辑备注与配额、删除（二次确认，提示"数据目录保留"）
3. **新增账号**：输入账号名（校验 `[a-z0-9-]{1,32}`）→ 生成 AK/SK → **弹窗显示一次** AK/SK + 一键复制连接信息（Endpoint 用请求的 Host 推导、Region `us-east-1`、PathStyle=true）
4. **顶部**：当前 `AUTH_MODE` 标识、账号总数、总用量；右上角「退出」（builtin 模式）/ 当前用户（sso 模式）

管理 API（`/api/admin/*`，JSON）：
- `POST /api/admin/login`（builtin）→ 设 cookie；`POST /api/admin/logout`
- `GET /api/admin/accounts` → 列表（SK 默认**掩码**；带 `?reveal=<name>` 才返回该账号明文 SK，且必须鉴权）
- `POST /api/admin/accounts` → 新建（返回一次性 AK/SK）
- `POST /api/admin/accounts/<name>/rotate` → 轮换（返回一次性新 SK）
- `POST /api/admin/accounts/<name>/disable` / `/enable`
- `PATCH /api/admin/accounts/<name>` → 改 note / quotaBytes
- `DELETE /api/admin/accounts/<name>` → 删除账号表条目（**数据目录保留**，返回提示）
- `GET /api/admin/overview` → `{authMode, accounts, totalUsageBytes, version}`
- **所有管理 API 必须写审计日志**（`<data>/admin-audit.log`，0600，一行一条：时间 / 操作 / 账号 / 来源 IP），日志里**不得**出现明文 SK

## 3. 数据与并发

- 管理 API 改 `accounts.json` 必须与 CLI **共用同一套**读写逻辑（原子写：临时文件 + rename），不得各写一份
- 改完后服务端热重载应立即生效（M0 已有机制，复用即可）
- 并发：两个请求同时改账号表要用同一把锁，不能丢更新

## 4. 验收标准（Hermes 会独立复核）

### 4.1 静态
```bash
cd web && timeout 300 npm ci && timeout 600 npm run build   # 前端构建通过
timeout 300 go vet ./...
timeout 600 go test ./... -count=1 -race
timeout 300 make build                                        # 单二进制包含前端
```

### 4.2 必写测试
1. `builtin`：无 cookie 访问 `/api/admin/accounts` → 401；错口令登录 → 401 且失败计数生效（第 11 次 → 429）；正确口令 → 拿到 cookie 后可访问。
2. `sso`：无 `X-Auth-User` → 401；带 `X-Auth-User: tester` → 200（**且此时即使口令正确也不能走 builtin 登录**）。
3. **一次性密钥**：`POST /api/admin/accounts` 与 `rotate` 的响应里含 SK，但随后 `GET /api/admin/accounts` 默认是掩码；`?reveal=` 才出明文。
4. 账号 CRUD 与 CLI 结果一致（同一次改动，CLI `account list` 与服务端 API 看到的是同一份）。
5. 审计日志：做 3 次操作后文件存在且含 3 条记录，且**全文不含**明文 SK（断言）。
6. 静态资源：`/` 返回管理页 HTML（内置），`/assets/*` 能取到 JS/CSS；构建产物确实被 embed（删掉 `web/dist` 后重新 `make build` 会失败或明显报错，说明确实依赖构建产物——写进报告说明即可，不要真的删）。

### 4.3 手工冒烟（写进报告）
`./objbox serve` 起服务 → `curl` 走一遍：登录失败/成功、列表掩码、reveal、退出后 401；把响应片段贴进报告。

## 5. 文档要求
- `README.md`：补"管理页"一节（截图占位不放、纯文字说明；说明 `AUTH_MODE` 两种模式与安全边界；**明确 S3 端点不受 AUTH_MODE 影响**）
- `docs/API.md`：补管理 API 一览
- `docs/REQUIREMENTS-M0.md` 的非目标清单同步（管理页已完成）

## 6. 交付报告
写到 `/tmp/objbox-m3-report.md` 并 `cat`：逐条对照 §1–§3 的完成情况、§4 实测输出、`git log --oneline` 每笔对应改动点、已知限制与安全边界说明。
