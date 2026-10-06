# objbox 需求书 · M0（可用骨架）

> 这份文档是**自包含**的：执行者看不到任何对话历史，所有需要的信息都在本文里。
> 项目：`github.com/YLing2024/objbox`（Go）。仓库目前**只有 `LICENSE` 和 `docs/`**，代码从零开始。
> 目标一句话：**极简自建 S3 兼容对象存储 —— 一个账号一套 AK/SK、一个账号一个隔离空间、不引入用户系统。**

---

## 0. 实施前必读（铁律）

1. **不要 push**、不要新建/切换分支、不要 `rebase`/`reset --hard`/`merge` —— 就在当前分支（`main`）往前走。
2. **不要碰本机服务**：禁止 `systemctl`/`service`/`kill`/`pkill`/`fuser -k`；禁止操作项目目录之外的任何路径（`/etc`、`/usr/local`、`~/.hermes` 全属禁区）。
3. 每条命令自带 `timeout`；失败不要把同一条命令死循环重试。
4. **遇到文档与仓库现实冲突（某令牌/接口名已存在、命名不同、依赖已存在）→ 以仓库现实为准，直接实现，不要提问**。（非交互模式下提问会导致整轮任务中断。）
5. 代码注释与提交消息用中文；标识符用英文。
6. **仓库内不得出现任何真实私有地址**（真实域名、公网 IP、内网 IP、邮箱、密钥）。示例一律用 `s3.example.com` / `127.0.0.1` / `AKEXAMPLE...` 这类占位。
7. 只改本仓库内的文件。

## 提交纪律（必须遵守）

- **每完成一个逻辑单元就 `git commit` 一次**（小步多次），不要憋到最后一次性提交；多文件同一件事可以一次提交，多件事不要塞进一次提交。
- 提交消息用 conventional commits：`feat:` / `fix:` / `refactor:` / `test:` / `docs:` / `chore:` ＋一行摘要（说清"改了什么、为什么"），必要时补正文。
- **禁止**：`git push`、`git checkout -b`、`git rebase`、`git reset --hard`、`git merge`。
- 交付时**工作区必须干净**（`git status --short` 无未提交改动）。
- 报告里给出 `git log --oneline -n <本次提交数>`，并说明每笔对应哪个改动点。

---

## 1. 已定的技术选型（不可更改）

| 项 | 选定 | 说明 |
|---|---|---|
| 语言 | **Go 1.24** | `go.mod` 写 `go 1.24`（本机工具链 go1.24.4，避免无谓下载） |
| HTTP | 标准库 `net/http` | 不引 web 框架 |
| **S3 协议层** | **`github.com/johannesboyne/gofakes3`**（MIT） | 路由/XML/列表/分片流程都用它；**它不做认证**，认证由我们写 |
| **签名校验** | **`github.com/aws/aws-sdk-go-v2/aws/signer/v4`** | **只用来"重算签名并比对"**，绝不自研密码学 |
| 元数据 | **`go.etcd.io/bbolt`**（MIT，纯 Go 嵌入式 KV） | 单文件、无独立进程、索引可重建 |
| 对象数据 | 本地文件系统 | 直接落盘 |
| 构建 | `Makefile`（`build`/`test`/`vet`/`clean`） | 单二进制 `./objbox` |
| 部署 | systemd 单二进制 | M0 只需能在本机 `./objbox serve` 起来 |

## 2. 目录结构（照 davbox 的分层习惯）

```
cmd/objbox/main.go            # 子命令入口（serve / account ...）
internal/account/             # 账号模型、accounts.json 读写、AK→账号索引、AK/SK 生成
internal/auth/                # SigV4 校验、身份注入 context、时间容差
internal/backend/             # POSIX 后端：桶/对象/元数据(bbolt)/multipart；路径安全
internal/server/              # HTTP 装配：认证中间件 → 按账号装配 gofakes3 handler；admin API
internal/usage/               # 账号用量统计（目录递归大小，含缓存）
internal/randstr/             # 随机串生成（AK/SK、uploadId 用）
accounts.example.json         # 示例配置（不含真实密钥）
Makefile
```

## 3. M0 功能清单（全部完成才算交付）

### 3.1 账号与配置
- `accounts.json` 结构：
```json
{
  "version": 1,
  "accounts": [
    {
      "name": "demo",
      "ak": "AKEXAMPLE7F3KQ2M9XZ4WTVD6RYH1BN8C5PL",
      "sk": "SKEXAMPLE32BYTESRANDOMSTRINGVALUE0001",
      "root": "/var/lib/objbox/roots/demo",
      "readonly": false,
      "disabled": false,
      "quotaBytes": 0,
      "note": "示例账号"
    }
  ]
}
```
- 文件权限 `0600`；`root` 为空时按 `<data>/roots/<name>` 推导；加载时做**唯一性校验**（AK 不可重复、name 不可重复）。
- **AK 生成规则**：`AK` 前缀 + 大写 base32 随机（总长 32～40）；SK：32 字节随机 base64url 或 base32。
- **SK 只在生成/轮换时打印一次到 stdout**，之后仅存于 `accounts.json`（0600）。
  *（原因：S3 签名校验必须拿到明文 SK 才能重算 HMAC 链，无法只存哈希 —— 这是本项目已知且接受的取舍。）*

### 3.2 CLI（标准库 `flag`，手写子命令，不引 cobra）
```
objbox serve   -addr 127.0.0.1:18930 -data /var/lib/objbox
objbox account add <name> [-note "..."] [-readonly]
objbox account list
objbox account rotate <name>
objbox account disable|enable <name>
objbox account remove <name>
```
- `account add` 打印：name / AK / SK / root 路径 / 用途提示（S3 Endpoint 占位 `https://s3.example.com`、Region 占位 `us-east-1`、PathStyle=on）。
- `account list` 里 SK 一律**掩码**（前 4 后 4），可加 `-show-secret` 显式打印全文（默认关闭）。

### 3.3 认证（认证中间件，M0 核心）
对每个入站请求：
1. 解析 `Authorization: AWS4-HMAC-SHA256 Credential=<AK>/<date>/<region>/s3/aws4_request, SignedHeaders=..., Signature=...`。
2. 由 AK 查账号：无此 AK → `403 AccessDenied`；账号 `disabled` → `403 AccessDenied`。
3. **重算签名并比对**（用 `aws-sdk-go-v2/aws/signer/v4` 的 `SignHTTP`，payload hash 取请求头 `x-amz-content-sha256` 的值；该值为 `UNSIGNED-PAYLOAD` 时按原值参与计算）。
4. 签名不符 → `403 SignatureDoesNotMatch`；`X-Amz-Date` 与本地时间差超过 **±15 分钟** → `403 RequestTimeTooSkewed`。
5. 校验通过 → 把账号身份写入 `context`，交给该账号的协议处理器。
- **日志**：`Authorization` 头一律脱敏（只留 AK 与签名前 8 位），永不落盘完整值。
- 若 `accounts.json` 中账号数 > 0，则**匿名请求一律 403**（M0 不做公开桶）。

### 3.4 隔离（M0 核心，安全要求最高）
- 每个账号只允许访问自己 `root` 下的内容；协议层拿到的 bucket/key 必须经过**唯一入口** `safeJoin(root, bucket, key)`：
  - 拒绝：`..`、编码后的 `%2e%2e`/`%2f`、反斜杠、空字节 `\x00`、以 `/` 开头的绝对路径、含 `//` 的异常路径；
  - 拒绝：bucket 名不合规（只允许 `[a-z0-9.-]{3,63}` 且不能以 `-`/`.` 开头结尾）、key 过长（> 1024 字节）；
  - 任何拒绝 → `400 InvalidRequest`（错误码与消息要写清原因）。
- **跨账号**：签名合法但请求的 bucket 不属于该账号 → 与"桶不存在"**完全相同的响应**（`403 AccessDenied`），不泄露存在性。
- `readonly` 账号：`PUT`/`POST`/`DELETE` 一律 `403 AccessDenied`，读正常。
- 所有文件操作只经过 `safeJoin` 的结果路径，禁止出现裸 `filepath.Join(root, userInput)`。

### 3.5 协议（gofakes3 后端实现，M0 范围）
必须实现的后端方法（gofakes3 的 `Backend` 接口，缺失则补桩并注明）：
- `ListBuckets` / `CreateBucket` / `BucketExists` / `DeleteBucket`（**非空桶拒绝**，返回 `BucketNotEmpty`）
- `PutObject` / `GetObject`（含 `Range` 基础支持）/ `HeadObject` / `DeleteObject`
- `ListBucket`（prefix + delimiter + max-keys + 分页 marker/continuation）
- 元数据（bbolt）：`bucket/key → {size, etag, contentType, lastModified, userMeta}`；
  - **ETag = 对象内容的 MD5 十六进制，外层带双引号**（形如 `"d41d8cd98f00b204e9800998ecf8427e"`）
  - 写入：**临时文件 → fsync → rename 原子替换**，随后写 bbolt
  - `objbox serve` 启动时若 bbolt 里缺少某对象（或文件已消失）→ 以磁盘为准修正（**索引可重建**）
- 不在 M0 范围（**不要实现**）：分片上传、批量删、CopyObject、预签名 URL、Web 管理页、Docker、多语言 README。

## 4. 验收标准（Hermes 会独立复核，写清自测命令）

### 4.1 静态检查（必须全绿）
```bash
cd /root/proj/objbox
timeout 300 go vet ./...          # 0 报错
timeout 300 go test ./... -count=1 # 全绿
timeout 300 make build            # 产出 ./objbox
```

### 4.2 必写的单测（表驱动）
1. **路径逃逸表**：`../etc/passwd`、`..%2f..%2fetc`、`%2e%2e/`、`a/../../b`、`\..\..\x`、`/abs/path`、`key\x00.txt`、`a//b` → 每个都必须被拒绝（错误非 nil）。
2. **签名校验**：正确签名 → 通过；错 SK → `SignatureDoesNotMatch`；时间偏移 20 分钟 → `RequestTimeTooSkewed`；篡改 body 后签名不匹配 → 拒绝。
3. **隔离**：账号 A 用合法签名请求账号 B 的桶 → `403 AccessDenied`，且响应与"桶不存在"逐字节一致。
4. **只读账号**：`PUT` → 403；`GET` → 200。
5. **停用账号**：一切请求 → 403。
6. **CRUD 往返**：建桶 → 传对象 → 读回内容一致 → HEAD 的 `Content-Length`/`ETag` 正确 → 删除 → 再读 404。

### 4.3 端到端（必须真跑 HTTP，不许只测内存）
写 `internal/server/e2e_test.go`：用 `httptest` 起真 HTTP 服务 + **`aws-sdk-go-v2/service/s3` 客户端**（PathStyle=true，自定义 Endpoint 指向 httptest）打真请求，覆盖：`CreateBucket` → `PutObject` → `GetObject`（内容一致）→ `HeadObject`（ETag 带引号）→ `ListBuckets` → `ListObjectsV2` → `DeleteObject` → `DeleteBucket`；以及一个"错 SK 的客户端必须被拒"的用例。

### 4.4 手工冒烟（写进报告）
`./objbox serve` 起好后，用 `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18930/`（无签名）应得 `403`。

## 5. 交付报告（必须做）
把报告写到 **`/tmp/objbox-m0-report.md`**，然后用 `cat /tmp/objbox-m0-report.md` 打印出来（保证内容能回流到命令输出）。报告包含：
1. 目录树与各文件职责
2. 逐条对照 §3 功能清单的完成情况（做了什么 / 没做什么 / 为什么）
3. §4 的实测输出（`go vet`、`go test`、`make build` 的真实结尾若干行、e2e 用例名与结果）
4. `git log --oneline -n <提交数>` 与每笔对应的改动点
5. 已知限制与下一步建议
