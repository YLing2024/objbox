[简体中文](README.md) ｜ [English](README.en.md)

# objbox

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

极简自建 S3 兼容对象存储：**一个账号一套 AK/SK、一个账号一个隔离空间**，单二进制、无数据库、无中间件。

## 为什么自己做

MinIO、Garage、SeaweedFS 这类平台型方案提供的是完整的集群、纠删码、复制与生命周期治理，
代价是要维护一组服务、一堆配置和一个不小的运维面。

objbox 只补其中很薄的一层：**账号 + AK/SK + 隔离**。它复用标准库 `net/http` 与
[gofakes3](https://github.com/johannesboyne/gofakes3) 的路由/XML，认证由自己按 SigV4 重算，
元数据用嵌入式 bbolt，对象内容直接落盘。适合"一台机器、几个账号、想用 rclone/aws cli/mc
当网盘用"的场景；不追求替代平台型方案。

- 语言：Go；HTTP 用标准库 `net/http`
- S3 协议层：gofakes3（只做路由/XML，不做认证）
- 认证：SigV4 头签名 + 预签名 URL，用 `aws-sdk-go-v2/aws/signer/v4` 重算比对
- 元数据：bbolt（纯 Go 嵌入式 KV）；对象内容直接落盘

> 本文档中的端点一律用占位 `https://s3.example.com`，请替换为你自己的地址。

## 特性

- **账号隔离**：每个账号一个独立 root 目录，跨账号访问与"桶不存在"返回逐字节相同的 403。
- **桶自动创建**：一个应用一个账号，建号即建桶；客户端只填 AK/SK，Bucket 随便填或填账号名即可用。
- **AK/SK 认证**：标准 SigV4 `Authorization` 头，以及 GET / PUT 预签名 URL。
- **只读与停用**：账号可设为只读（拒绝写操作）或停用（拒绝全部请求），即时生效。
- **热重载**：`accounts.json` 变更后服务端自动重载，解析失败保留旧表，不清空。
- **S3 子集**：桶/对象 CRUD、前缀与分页列表、批量删除、同账号 CopyObject。
- **预签名**：有效期上限 7 天，支持 `Range`。
- **分片上传**：流式落盘、并发分片、启动时清理过期任务。
- **配额**：按账号限制写入字节数，超出返回 `403 QuotaExceeded`。
- **跨域白名单（CORS）**：浏览器端预签名直传 / 直下可用的跨域白名单，管理页配置、保存即生效。
- **管理页**：内嵌 React 管理页，支持 `AUTH_MODE=builtin`（自带口令）与 `sso`（信任网关）。
  账号一键查看详情并复制接入信息（Endpoint / AK / SK / Bucket / Region / Path-style 与 rclone 示例），
  新建账号后自动打开；账号内可建桶 / 看桶 / 删桶并查看对象数与占用，默认桶与自动建桶开关可就地修改。
- **单二进制**：前端构建产物用 `//go:embed` 打进二进制，部署只需一个文件加一个数据目录。

## 快速开始

### 源码构建

```bash
make build                 # 先构建 web/，再 go build，产出 ./objbox
./objbox serve -addr 127.0.0.1:18930 -data /var/lib/objbox
```

账号管理（`accounts.json` 权限 0600，含明文 SK）：

```bash
./objbox account add demo -note "示例"       # 账号名须匹配 [a-z0-9][a-z0-9-]{0,31}
./objbox account list
./objbox account quota demo 1073741824       # 1GiB，0 表示不限
./objbox account rotate demo
./objbox account disable demo
./objbox account enable demo
./objbox account remove demo
```

生成预签名下载链接：

```bash
./objbox presign -account demo -bucket my-bucket -key path/to/file \
  -method GET -expires 3600 -endpoint https://s3.example.com
```

浏览器访问 `http://127.0.0.1:18930/` 打开管理页；首次启动会生成管理员口令并写入
`<data>/admin-password.txt`（0600）。

### Docker

```bash
make docker                # docker build -t objbox:latest .
make docker-run            # 前台运行，端口 18930，挂载 ./data:/data
make image-size            # 打印镜像体积
```

或使用 Compose（见 `docker-compose.yml`）：

```bash
docker compose up -d
```

> **数据卷必须持久化**：账号表、元数据库与对象内容都在 `/data` 下，容器重建不丢。
> Compose 里已把 `./data` 挂到 `/data`。

## 支持的 S3 操作

服务级：`ListBuckets`

桶级：`CreateBucket` / `DeleteBucket`（非空拒绝）/ `HeadBucket` /
`ListObjects`（V1 与 V2，支持 prefix、delimiter、分页）/
`DeleteObjects`（批量删，最多 1000 个 key）/ `ListMultipartUploads`

对象级：`PutObject` / `GetObject`（单区间 `Range`）/ `HeadObject` / `DeleteObject` /
`CopyObject`（同账号内，`x-amz-copy-source`）

分片上传：`CreateMultipartUpload` / `UploadPart` / `ListParts` /
`CompleteMultipartUpload` / `AbortMultipartUpload`

认证：SigV4 `Authorization` 头；预签名 URL（GET / PUT，有效期上限 7 天）

配额：`quotaBytes > 0` 时写入前检查，超出返回 `403 QuotaExceeded`

**明确不做**：版本控制（versioning/versions）、对象锁（object-lock）、生命周期
（lifecycle）、桶/对象 ACL、Bucket Policy、事件通知（notification）、跨区复制
（replication）、STS 临时凭据、S3 Select，以及公开桶/匿名访问、Virtual-Hosted-Style、
服务端加密、非 STANDARD 存储类。完整清单与错误码表见 [`docs/API.md`](docs/API.md)。

## 用法示例

### rclone

```ini
# ~/.config/rclone/rclone.conf
[objbox]
type = s3
provider = Other
access_key_id = AKEXAMPLE...
secret_access_key = SKEXAMPLE...
endpoint = https://s3.example.com
region = us-east-1
force_path_style = true
```

```bash
rclone lsd objbox:                                   # 列桶
rclone mkdir objbox:my-bucket                        # 建桶
rclone copy ./local-dir objbox:my-bucket/remote-dir  # 上传（大文件自动分片）
rclone ls objbox:my-bucket                           # 列对象
rclone copy objbox:my-bucket/remote-dir ./local-dir  # 下载
rclone delete objbox:my-bucket/remote-dir            # 删除对象
rclone rmdir objbox:my-bucket                        # 删除空桶
```

### aws cli

```bash
export AWS_ACCESS_KEY_ID=AKEXAMPLE...
export AWS_SECRET_ACCESS_KEY=SKEXAMPLE...
export AWS_DEFAULT_REGION=us-east-1
ENDPOINT=https://s3.example.com

aws --endpoint-url "$ENDPOINT" s3api list-buckets
aws --endpoint-url "$ENDPOINT" s3api create-bucket --bucket my-bucket
aws --endpoint-url "$ENDPOINT" s3 cp ./big.bin s3://my-bucket/big.bin        # 大文件自动走分片
aws --endpoint-url "$ENDPOINT" s3 ls s3://my-bucket/
aws --endpoint-url "$ENDPOINT" s3api get-object --bucket my-bucket --key big.bin out.bin
aws --endpoint-url "$ENDPOINT" s3 rm s3://my-bucket/big.bin
```

预签名（由 objbox CLI 生成后直接用 curl/浏览器访问）：

```bash
URL=$(./objbox presign -account demo -bucket my-bucket -key big.bin \
  -endpoint https://s3.example.com)
curl -o big.bin "$URL"
```

## 跨域直连（浏览器端使用）

浏览器里直接读写本对象存储（**预签名直传 / 直下**，或前端用 `fetch` 直连 S3 API）时，
请求会带 `Origin` 并触发 CORS 预检。objbox 的跨域白名单在**管理页**配置，不在环境变量里改。

1. 打开管理页，页面底部「**跨域白名单（CORS）**」一节；
2. 文本域里每行填一个来源（也支持逗号分隔），如 `https://app.example.com`；
3. 点「保存」，提示「已保存并即时生效」——**无需重启**。

规则与说明：

- 来源必须是 `scheme://host` 或 `scheme://host:port`，`scheme` 只支持 `http` / `https`；
  非法项会当场报错并指出是哪一行，不会被静默丢弃。
- 留空表示**关闭跨域**。
- 生效优先级：**管理页保存的值（`settings.json`）优先**；设置文件里还没有该键时，用环境变量
  `CORS_ORIGINS`（逗号分隔）作为**首次默认值**；两者都没有 = 关闭。
- 命中白名单的预检（`OPTIONS` + `Origin` + `Access-Control-Request-Method`）直接回 `204`，
  **不要求任何签名/认证**；普通请求的签名校验、权限与桶策略完全不变。
- 命中时回显请求来源（不使用 `*`）、允许携带凭据，并暴露 `ETag` 等响应头，便于浏览器端做
  校验与断点续传。
- 生效范围是 S3 API 路径（桶 / 对象的所有动词，含预签名请求）；管理面自身不放开跨域。
- 按桶的 S3 CORS 子资源（`PUT/GET/DELETE /<bucket>?cors`）本次**未实现**，跨域只走这份全局白名单。

预签名直传示例（CLI 生成 URL，浏览器 `fetch` 直接 PUT）：

```bash
./objbox presign -account demo -bucket my-bucket -key upload/photo.jpg \
  -method PUT -expires 3600 -endpoint https://s3.example.com
```

```js
// 浏览器端：把上面的 URL 原样 PUT 上去即可（无需 AK/SK，签名在 URL 里）
await fetch(PRESIGNED_URL, { method: 'PUT', body: file })
```

> **安全提示**：白名单里**只填你自己的前端域名**，不要填 `*`。命中来源的网页可以携带凭据直连
> 本对象存储；预签名 URL 有时效，等同于临时凭证，请勿泄露或写入公开页面。

## 配置

环境变量：

| 变量 | 取值 | 默认 | 说明 |
|---|---|---|---|
| `AUTH_MODE` | `builtin` \| `sso` | `builtin` | **仅管理面**认证方式；S3 端点始终走 AK/SK |
| `CORS_ORIGINS` | 逗号分隔的来源 | 空 | 跨域白名单的**首次默认值**；仅在设置文件还没有该键时生效，已在管理页保存过就以设置文件为准 |
| `OBJBOX_PUBLIC_ENDPOINT` | URL | 空 | 管理页详情窗口展示 / 复制的对外 Endpoint；留空则按 `X-Forwarded-Proto` + `X-Forwarded-Host`（或 `Host`）运行时推导 |

常用命令行参数（均为进程启动参数，不是环境变量）：

| 参数 | 默认 | 说明 |
|---|---|---|
| `serve -addr` | `127.0.0.1:18930` | HTTP 监听地址 |
| `-data` | `/var/lib/objbox` | 数据目录（账号表、元数据库、对象内容） |
| `serve -access-log` | `false` | 打印访问日志，`Authorization` 一律脱敏 |

`AUTH_MODE` 说明：

- `builtin`：首次启动生成随机管理员口令，明文写入 `<data>/admin-password.txt`（0600），
  bcrypt 哈希存 `<data>/admin.json`；会话 cookie `objbox_admin`（`HttpOnly; SameSite=Lax; Path=/`，
  HTTPS 下另带 `Secure`，TTL 12 小时）。登录失败按来源 IP 限速：每分钟最多 10 次。
- `sso`：不做自带登录，管理面只信任网关注入的 `X-Auth-User` 头；缺失该头一律 401。

未显式指定 `-data` 时，CLI 会提示"正在使用默认数据目录 X"，`account add` 输出也会打印实际使用的目录。

## 安全模型

- **隔离规则**：每个账号有独立 root，所有对象路径都经唯一入口 `SafeJoin` 校验，
  任何 `..`、绝对路径或越界符号链接都会被拒绝；跨账号与桶不存在统一返回 403，不泄露存在性。
- **403 统一语义**：无此 AK、账号停用、跨账号、只读写操作、预签名过期一律 `403 AccessDenied`，
  响应体逐字节一致（不带随机字段），避免被用来枚举账号或对象。
- **SK 明文落盘的取舍**：SigV4 校验要求服务端持有明文 SK 才能重算 HMAC 链，因此 SK 无法只存哈希，
  只能明文写 `accounts.json`（0600）与内存；请确保数据目录权限，且不要把该文件纳入备份共享。
  轮换 SK 会使旧 SK 立即失效；管理页默认掩码 SK，只有显式 `?reveal=<name>` 且已鉴权才返回明文。
- **使用建议**：只在自己机器或内网使用；对公网暴露时务必配合 HTTPS（反向代理终止 TLS），
  并把数据目录放在只有服务账号可读的位置。
- **`X-Forwarded-For` 信任边界**：服务端登录限速优先取 `X-Forwarded-For`/`X-Real-IP`（为网关反代设计）。
  **直接暴露公网时必须由前置代理覆盖该头**，否则客户端可伪造 IP 绕过登录限速。nginx 示例：

  ```nginx
  location / {
      proxy_pass http://127.0.0.1:18930;
      proxy_set_header Host $host;
      proxy_set_header X-Forwarded-For $remote_addr;   # 覆盖，不追加客户端值
      proxy_set_header X-Forwarded-Proto $scheme;
  }
  ```

## 许可与贡献

本项目使用 [MIT 许可](LICENSE)，版权归 `Copyright (c) 2026 YLing2024`。
欢迎提 Issue 与 PR，开发与提交规范见 [`CONTRIBUTING.md`](CONTRIBUTING.md)。
