# REQUIREMENTS-M5 — 账号即空间：桶自动创建

> 本文件是给编码执行方的**自包含需求书**：不要依赖任何对话上下文，按本文件实现即可。
> 冲突时以仓库现状为准，直接实现、不要提问、不要中断。

## 背景（为什么要做）

objbox 的隔离模型是「**账号 = 独立存储空间**」：一个账号一套 AK/SK，落盘在账号自己的根目录下（走
`internal/backend.SafeJoin` 唯一入口）。但 S3 协议里对象必须放在**桶**里，而真实客户端（思源笔记、
rclone、各类 App）**不会主动建桶**：它会直接 GET/PUT 某个桶里的对象。

桶不存在时，objbox 按 M0 的「反枚举」规则返回 `403 AccessDenied`，于是客户端报：
`同步失败: operation error S3: GetObject ... api error AccessDenied`。
用户被迫先手工建桶才能用 —— 这是要消除的摩擦。

目标体验：**一个应用一个账号；建号即建桶；客户端只填 AK/SK（Bucket 那栏随便填或填账号名）就能用**，
与 WebDAV 的手感一致，用户不需要理解「桶」这个概念。

## 1. 后端：账号模型与自动建桶

### 1.1 账号模型新增字段（`internal/account`）
- `autoCreateBucket`（bool，新建账号默认 `true`）
- `bucket`（string，该账号的默认桶名；缺省 = 账号名）
- **向后兼容硬要求**：读到不含这两个字段的旧 `accounts.json` 时不得报错；缺省语义为
  `autoCreateBucket = true`、`bucket = 账号名`。写回时把这两个字段显式落盘（0600、原子写不变）。
- 现有的账号名校验、AK/SK 生成规则、只读标记、热重载语义（解析失败保留旧表）**一律不变**。

### 1.2 请求即建桶（隐式建桶）
在请求**已通过 SigV4 认证**、按账号分发到该账号 `Backend` 之后，若请求路径里的桶**不存在**：

- 该账号 `autoCreateBucket == true` → **自动创建该桶**（只在该账号自己的根目录下，必须复用
  `SafeJoin`/现有 `CreateBucket` 逻辑，不得新开路径拼接），随后按正常流程继续处理。
- 该账号 `autoCreateBucket == false` → **保持现状**（不建、按 M0 反枚举语义返回逐字节相同的 403）。

触发范围（明确写死，避免歧义）：

| 操作 | 桶不存在时（autoCreateBucket=true） |
|---|---|
| PUT Object | 先建桶，再正常写入 |
| POST 分片（?uploads / ?uploadId / ?complete） | 先建桶，再正常处理 |
| GET / HEAD Object | 先建桶，再按「对象不存在」返回 404 NoSuchKey |
| GET Bucket（ListObjects V1/V2）、HEAD Bucket | 先建桶，再返回空列表 / 200 |
| DELETE Object | **不建桶**；桶不存在 → 保持 NoSuchBucket（404）语义 |
| DELETE Bucket | **不建桶**；桶不存在 → NoSuchBucket（404），不得因为自动建桶而变成 200 |
| CopyObject（源或目标桶） | **不建桶**；桶不存在 → 保持现状语义 |

其它约束：
- 桶名合法性校验**不得放宽**：不符合现有 `ValidateBucket` 规则的桶名照旧拒绝（错误码与文案不变）。
- 自动建桶失败（如磁盘错误）→ 返回 5xx，不得静默当作成功。
- **跨账号隔离语义不得改变**：不同账号根目录不同，自动建桶只发生在当前账号自己的根下；
  M0「跨账号访问与桶不存在返回逐字节相同的 403」这一断言在 `autoCreateBucket=false` 下必须**原样成立**。

### 1.3 建账号时自动建桶
`account add` 与管理 API 创建账号成功后，若 `autoCreateBucket == true`：
- 在该账号根下创建默认桶（`bucket` 字段，默认 = 账号名）；
- 桶已存在 → 视为成功（幂等，不报错）；
- 建桶失败 → **回滚账号创建**（删掉刚写入的账号），返回明确错误，保证「建完就能用」。

## 2. CLI（`cmd/objbox`）

```
objbox account add <name> [-note "..."] [-readonly] [-bucket NAME] [-no-bucket] [-data DIR]
```
- 默认：建桶，桶名 = 账号名。
- `-bucket NAME`：指定默认桶名（仍受 `ValidateBucket` 校验）。
- `-no-bucket`：不建桶，且该账号 `autoCreateBucket = false`（完全不自动建桶的账号）。
- `account list` 输出中体现默认桶名与是否自动建桶（**不得因此输出 SK 明文**，维持现状）。
- 用法/帮助文本（`accountAddUsage` 等）同步更新；账号名不合法时的提示文案保持现状。
- 新增账号名的边界测试：`-data`、`-bucket` 等 flag **绝不能**被当成账号名（历史踩坑，需有回归测试）。

## 3. 管理 API（`/api/admin/accounts`）

- `POST` 创建账号请求体新增可选字段：`bucket`（string，默认 = 账号名）、`autoCreateBucket`（bool，默认 `true`）。
- 列表/详情响应新增 `bucket`、`autoCreateBucket` 字段（**除既有 reveal 接口外不得返回 SK**）。
- 桶名非法 → `400` + 明确错误消息；桶已存在 → 成功（幂等）。
- 审计动作 `accounts.create` 记录桶名。
- 现有接口的路径、方法、鉴权（HMAC cookie / `AUTH_MODE`）、限速、错误体形状一律不变。

## 4. Web 管理页（`web/`，React + Vite + TS）

- 新建账号表单新增：开关「自动创建同名桶」（默认开）+ 可选桶名输入（留空 = 账号名）。
- 账号列表每行展示默认桶名（未建桶显示「未建桶」）。
- 文案：中文、克制、**无 emoji**、不写技术说明性文案；样式沿用现有页面（不新增依赖、不引图标库）。
- 前端构建产物仍由 `//go:embed all:dist` 打包；`make build` 顺序保持「先构建前端再 `go build`」。

## 5. 文档

- `docs/API.md`：新增「桶自动创建」章节；写明错误码变化（`autoCreateBucket=true` 时桶不存在不再返回 403）
  与上表的触发范围；补一个「思源笔记 / rclone 接入」的最小配置示例（Endpoint / AK / SK / Bucket 随便填）。
- `README.md`：中英双语各加一句——一个应用一个账号，桶自动创建，填 AK/SK 即用。
- `docs/TECH_DESIGN.md`（或同目录 ADR 段）：记录为什么保留账号维度（隔离 / 配额 / 单独吊销轮换），
  为什么做自动建桶（消除客户端摩擦）。

## 6. 测试（必须能跑、必须真绿）

- 单元：旧 `accounts.json`（无新字段）加载后按默认值生效；新字段落盘与再读取。
- 集成（`httptest` + 真 SigV4 签名，沿用现有测试脚手架）：
  1. `autoCreateBucket=true`：对**不存在**的桶 PUT 对象 → 成功，且桶被建出（ListBuckets 可见）。
  2. 对不存在的桶 GET/HEAD/LIST → 空列表 / 404 NoSuchKey，**不得 403**。
  3. `autoCreateBucket=false`：不存在桶 → 403，且与「跨账号访问」响应**逐字节相同**（保留 M0 断言）。
  4. DELETE Bucket 对不存在的桶 → 仍 NoSuchBucket（不得被自动建桶改写成 200）。
  5. CLI：`account add` 建桶；`-no-bucket` 不建桶；`-bucket NAME` 指定名；flag 不被当账号名。
  6. 管理 API：创建账号带/不带桶；非法桶名 400。
- 收尾自检：`go vet ./...` 0 报错、`go test ./... -count=1 -race` 全绿、`make build` 成功。

## 7. 铁律（必须遵守）

1. 每完成一个逻辑单元就 `git commit` 一次（conventional commits，**小步多次**）。
2. **禁止 `git push`**、禁止建分支、禁止 `rebase` / `reset --hard` / `merge` —— 就在当前 `main` 往前走。
3. **禁止 `systemctl` / `service` / `kill` / `pkill` / `fuser`**；不要启动或停止任何服务。
4. 只读写 `/root/proj/objbox` 内的文件，**不得操作任何其它路径**（`/data`、`/etc`、`~/.config` 一律不碰）。
5. 所有命令带 `timeout`。
6. 仓库内不得出现真实域名、公网 IP、密钥、邮箱；示例一律 `s3.example.com` / `127.0.0.1`。
7. 不引入新的第三方依赖（前端也不新增包）。
8. 完成后输出：改动清单（文件 + 一句话）、提交列表、实测命令与输出摘要、已知限制。
