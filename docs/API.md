# objbox S3 API 对照表

本文档列出 objbox 当前**支持**与**明确不支持**的 S3 API，以及错误码表，供使用者对照。
示例端点统一使用占位 `https://s3.example.com`，实际部署时替换为自己的地址。

约定：

- 仅支持 **Path-Style**（`/<bucket>/<key>`），不支持 Virtual-Hosted-Style。
- 所有请求都需要 SigV4 认证：既支持 `Authorization` 头，也支持**预签名 URL**（query 签名）。
- 账号之间在存储层完全隔离：一个 AK 只能看到自己账号 root 下的桶与对象；
  访问其他账号的桶与访问不存在的桶返回**完全相同的** 403。
- 对象内容直接落盘，元数据在 bbolt；写入为「临时文件 → fsync → rename」原子替换。

## 支持的操作

### 服务级

| 操作 | 方法与路径 | 说明 |
|---|---|---|
| ListBuckets | `GET /` | 列出本账号的桶 |

### 桶级

| 操作 | 方法与路径 | 说明 |
|---|---|---|
| CreateBucket | `PUT /<bucket>` | 桶名规则 `[a-z0-9.-]{3,63}`、首尾非 `-`/`.`、不含 `..` |
| DeleteBucket | `DELETE /<bucket>` | 非空桶返回 `BucketNotEmpty` |
| HeadBucket | `HEAD /<bucket>` | |
| ListObjects V1 | `GET /<bucket>` | 支持 `prefix` / `delimiter` / `marker` / `max-keys` |
| ListObjects V2 | `GET /<bucket>?list-type=2` | 支持 `prefix` / `delimiter` / `continuation-token` / `start-after` / `max-keys` |
| DeleteObjects（批量删） | `POST /<bucket>?delete` | XML body，最多 1000 个 `<Object><Key>`；不存在的 key 视为成功 |
| ListMultipartUploads | `GET /<bucket>?uploads` | 支持 `prefix` / `delimiter` / `key-marker` / `upload-id-marker` / `max-uploads` |

### 对象级

| 操作 | 方法与路径 | 说明 |
|---|---|---|
| PutObject | `PUT /<bucket>/<key>` | key 上限 1024 字节；写入前做配额检查 |
| GetObject | `GET /<bucket>/<key>` | 支持单区间 `Range`；响应头含 `Content-Type`/`Content-Length`/`ETag`/`Last-Modified`/`Accept-Ranges: bytes` |
| HeadObject | `HEAD /<bucket>/<key>` | |
| DeleteObject | `DELETE /<bucket>/<key>` | 不存在的 key 也返回 204 |
| CopyObject | `PUT /<bucket>/<key>` + `x-amz-copy-source: /<srcBucket>/<srcKey>` | 源与目标必须在**同一账号** root 内；跨账号源统一 403；`?versionId=` 忽略 |

### 分片上传（Multipart Upload）

| 操作 | 方法与路径 | 说明 |
|---|---|---|
| CreateMultipartUpload | `POST /<bucket>/<key>?uploads` | 返回 `InitiateMultipartUploadResult`（含 `UploadId`） |
| UploadPart | `PUT /<bucket>/<key>?partNumber=N&uploadId=ID` | 分片**流式落盘**，响应头带分片 `ETag` |
| ListParts | `GET /<bucket>/<key>?uploadId=ID` | 返回各分片 `PartNumber`/`ETag`/`Size` |
| CompleteMultipartUpload | `POST /<bucket>/<key>?uploadId=ID` | 按 body 顺序拼接，逐片校验 ETag 与大小 |
| AbortMultipartUpload | `DELETE /<bucket>/<key>?uploadId=ID` | 清理临时分片 |

分片说明：

- 分片临时文件放在账号 root 的内部目录 `<root>/.objbox/multipart/<uploadId>/`，
  不会被 `ListObjects` 看到；未完成的分片任务也不出现在对象列表里。
- 元数据（key / uploadId / 分片清单）存在 bbolt，重启不丢；启动时清理超过 24 小时未完成的任务。
- **有意放宽**：不强制最小分片 5MiB，接受更小的分片（便于测试）；真实 S3 只允许最后一片小于 5MiB。
- 同一 uploadId 的不同分片可并发上传；同一分片重复上传以最后一次为准。
- **最终对象 ETag = 拼接后完整内容的 MD5 十六进制**（带双引号），与普通 `PutObject` 语义一致；
  objbox **不**采用 S3 的 `"<md5>-<N>"` 复合 ETag。

### 认证与预签名

| 方式 | 说明 |
|---|---|
| `Authorization` 头 | 标准 SigV4 header 签名 |
| 预签名 URL | query 参数 `X-Amz-Algorithm`/`X-Amz-Credential`/`X-Amz-Date`/`X-Amz-Expires`/`X-Amz-SignedHeaders`/`X-Amz-Signature`；支持 GET 与 PUT；GET 可配合 `Range`；`X-Amz-Expires` 上限 604800 秒（7 天） |

预签名请求同样受账号隔离与只读约束；过期一律返回 `403 AccessDenied`（`Request has expired`）。
可用 `objbox presign -account <name> -bucket <b> -key <k> [-method GET|PUT] [-expires 3600]` 生成。

### 跨域（CORS）

objbox 支持一份**全局跨域白名单**（在管理页「设置」弹窗的「跨域白名单（CORS）」里配置，或由环境变量
`CORS_ORIGINS` 作为首次默认值），生效范围是 S3 API 路径（桶 / 对象的所有动词，含预签名请求）。
管理面 `/api/admin/*` 不放开跨域。

| 行为 | 说明 |
|---|---|
| 预检（`OPTIONS` + `Origin` + `Access-Control-Request-Method`） | 命中白名单直接回 `204` + CORS 头，**不要求签名/认证**；未命中不回 CORS 头，交回正常流程 |
| 命中响应头 | `Access-Control-Allow-Origin` 回显请求来源（不用 `*`）、`Allow-Credentials: true`、`Allow-Methods: OPTIONS, GET, HEAD, PUT, POST, DELETE`、`Allow-Headers` 覆盖 `Authorization` 与 `x-amz-*`、`Expose-Headers` 覆盖 `ETag` 与 `x-amz-*`、`Max-Age: 3600` |
| `Vary` | 所有 S3 响应带 `Vary: Origin` |
| 普通请求 | 签名校验、权限与桶策略完全不变；命中来源时附带上述 CORS 头 |

来源格式：`scheme://host` 或 `scheme://host:port`，`scheme` 限 `http/https`；非法项返回 `400`
并指明第 N 行。留空 = 关闭。设置文件（`settings.json`，0600，原子写）优先于环境变量。

### 用量与配额

- `objbox account list` 的 `USAGE(B)` 列为账号 root 下所有桶目录的递归大小（不含 `.objbox` 内部目录）。
- `quotaBytes > 0` 时，写入（`PutObject` / `CopyObject` / 分片初始化 / 分片完成）前检查；
  超出返回 `403 QuotaExceeded`。删除对象后用量下降，可继续写入。
- `objbox account quota <name> <bytes>` 设置配额，`<bytes>=0` 表示不限。

### 桶自动创建

objbox 的隔离模型是「账号 = 独立存储空间」，但真实客户端（思源笔记、rclone、各类 App）
不会主动建桶，而是直接读写某个桶里的对象。为消除「先手工建桶」这一步摩擦，新建账号默认
`autoCreateBucket = true`、默认桶名 = 账号名；建账号（CLI 或管理 API）时若开启该开关，
会在账号 root 下预建默认桶，桶已存在视为成功（幂等）。

请求通过 SigV4 认证并按账号分发后，若请求指向的桶在该账号 root 下不存在，按下表处理：

| 操作 | 桶不存在时（autoCreateBucket=true） |
|---|---|
| PUT Object | 先建桶，再正常写入 |
| POST 分片（`?uploads` / `?uploadId`） | 先建桶，再正常处理 |
| GET / HEAD Object | 先建桶，再按「对象不存在」返回 `404 NoSuchKey` |
| GET Bucket（ListObjects V1/V2）、HEAD Bucket | 先建桶，再返回空列表 / 200 |
| DELETE Object | **不建桶**；桶不存在返回 `404 NoSuchBucket` |
| DELETE Bucket | **不建桶**；桶不存在返回 `404 NoSuchBucket` |
| CopyObject（源或目标桶） | **不建桶**；桶不存在返回 `403 AccessDenied` |

- `autoCreateBucket = false` 的账号完全不自动建桶：跨账号访问与桶不存在继续返回
  **逐字节相同的** `403 AccessDenied`（M0 反枚举语义原样保留）。
- 桶名合法性校验**不放宽**：不符合 `ValidateBucket` 规则的桶名照旧返回 `400`。
- 自动建桶只发生在当前账号自己的 root 下；创建失败返回 `5xx`，不会静默当作成功。
- **错误码变化**：`autoCreateBucket=true` 时桶不存在不再返回 403，而是按上表返回
  404 / 空列表 / 200；`autoCreateBucket=false` 时仍为 403。

#### 接入示例（思源笔记 / rclone）

只需 Endpoint / AK / SK；Bucket 一栏可填账号名或任意合法桶名，首次写入时自动创建。

rclone：

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

思源笔记「设置 → 云端 → S3」：Endpoint 填 `https://s3.example.com`，Access Key / Secret Key
填账号的 AK/SK，Bucket 填账号名或任意合法桶名，Region 填 `us-east-1`，路径风格开启。
无需手工建桶。

## 明确不支持

以下 S3 能力当前**不实现**，请求会得到 `NotImplemented`、`MethodNotAllowed` 或按普通 404/403 处理：

- 桶级配置类 API：versioning、ACL、**按桶 CORS（`?cors` 子资源）**、lifecycle、policy、tagging、encryption、
  logging、notification、replication、website、inventory、analytics、metrics、
  accelerate、requestPayment、object-lock、public-access-block、ownership-controls。
  （全局跨域白名单见上「跨域（CORS）」一节，与 S3 的按桶 CORS 配置 API 不同。）
- 对象版本：`?versioning`、`?versions`、`?versionId=` 的版本语义（`versionId` 被忽略）。
- 对象级 ACL、retention、legal-hold、restore、select、torrent、tagging。
- 浏览器表单直传（`POST /<bucket>` 且非 `?delete`）。
- **多区间 Range**：一次 GET 只取一段；携带多段时**有意忽略多余区间，只取第一段**（不返回多段 body）。
- 公开桶 / 匿名访问：只要存在账号，匿名请求一律 403。
- Virtual-Hosted-Style 访问。
- 服务端加密与存储类：仅 `STANDARD`。
- MinIO 扩展（如 `x-minio-force-delete`）。

## 错误码表

错误响应统一为 S3 风格 XML：

```xml
<Error>
  <Code>...</Code>
  <Message>...</Message>
  <Resource>...</Resource>
  <RequestId>...</RequestId>
</Error>
```

每个响应都带 `x-amz-request-id` 响应头与 `Server: objbox`。
（认证层因安全考虑保持响应体逐字节一致、不含随机字段，request id 仅出现在响应头。）

| Code | HTTP | 说明 |
|---|---|---|
| `AccessDenied` | 403 | 无此 AK / 账号停用 / 跨账号 / 只读写操作 / 预签名过期 |
| `SignatureDoesNotMatch` | 403 | 签名重算不一致 |
| `RequestTimeTooSkewed` | 403 | 请求时间与本地相差超过 ±15 分钟 |
| `QuotaExceeded` | 403 | 账号配额不足 |
| `InvalidRequest` | 400 | 路径 / 桶名 / key 非法 |
| `InvalidBucketName` | 400 | 桶名不合规 |
| `KeyTooLongError` | 400 | key 超过 1024 字节 |
| `MalformedXML` | 400 | 批量删除 / 完成分片的 XML 非法 |
| `InvalidPart` | 400 | 分片缺失、ETag 或大小不符 |
| `InvalidPartOrder` | 400 | 完成分片时分片号未严格递增 |
| `IncompleteBody` | 400 | 实际字节数与 Content-Length 不符 |
| `NoSuchBucket` | 404 | 桶不存在 |
| `NoSuchKey` | 404 | 对象不存在 |
| `NoSuchUpload` | 404 | uploadId 不存在或已完成 / 已中止 |
| `BucketAlreadyExists` | 409 | 桶已存在 |
| `BucketNotEmpty` | 409 | 桶非空，不能删除 |
| `InvalidRange` | 416 | Range 越界或语法错误（带 `Content-Range: bytes */<size>`） |
| `MethodNotAllowed` | 405 | 方法不支持 |
| `NotImplemented` | 501 | 该 S3 能力未实现 |
| `InternalError` | 500 | 服务内部错误 |

> `autoCreateBucket=true` 时桶不存在不再返回 `AccessDenied`，而是按上表与「桶自动创建」
> 章节的触发范围返回 `NoSuchBucket` / `NoSuchKey` / 空列表；`autoCreateBucket=false`
> 时保持 403 反枚举语义。

## 管理 API（`/api/admin/*`，JSON）

管理面认证由环境变量 `AUTH_MODE` 决定（`builtin` 口令会话 / `sso` 信任 `X-Auth-User`）。
**这些接口与 S3 协议无关；S3 端点始终走 AK/SK，不受 `AUTH_MODE` 影响。**

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/admin/login` | `builtin` 口令登录，设置 `objbox_admin` cookie；`sso` 模式一律 401 |
| POST | `/api/admin/logout` | 退出：递增会话版本号使**所有**既有会话立即失效，并清除 cookie |
| GET | `/api/admin/accounts` | 账号列表（SK 默认掩码）；`?reveal=<name>` 返回该账号明文 SK（需鉴权） |
| POST | `/api/admin/accounts` | 新建账号，返回一次性 AK/SK 与连接信息（Endpoint 由请求 Host 推导） |
| POST | `/api/admin/accounts/<name>/rotate` | 轮换 SK，返回一次性新 SK |
| POST | `/api/admin/accounts/<name>/disable` | 停用账号 |
| POST | `/api/admin/accounts/<name>/enable` | 启用账号 |
| PATCH | `/api/admin/accounts/<name>` | 修改 `note` / `quotaBytes` |
| DELETE | `/api/admin/accounts/<name>` | 删除账号表条目（数据目录保留） |
| GET | `/api/admin/accounts/<name>/buckets` | 该账号桶列表 `[{name, objects, bytes, isDefault}]`（只读，走磁盘统计） |
| POST | `/api/admin/accounts/<name>/buckets` | 新建桶，body `{name}`；成功 `201`，已存在 `409`，非法桶名 `400` |
| DELETE | `/api/admin/accounts/<name>/buckets/<bucket>` | 删除**空**桶；非空 `409`，不存在 `404` |
| PATCH | `/api/admin/accounts/<name>/bucket` | 改默认桶名 / 自动建桶开关，body `{bucket?, autoCreateBucket?}` |
| GET | `/api/admin/overview` | `{authMode, accounts, totalUsageBytes, version}` |
| GET | `/api/admin/settings` | 读服务端设置：`{corsOrigins, corsEnabled, corsSource}` |
| PUT | `/api/admin/settings` | 写跨域白名单，body `{corsOrigins: [...]}`；校验失败 `400`（指明第 N 行） |

- 除 `login` 外均需鉴权：`builtin` 校验签名 cookie，`sso` 校验 `X-Auth-User`，未通过返回 401。
- `POST /api/admin/accounts` 请求体可选字段：`bucket`（默认 = 账号名）、
  `autoCreateBucket`（默认 `true`）；`autoCreateBucket=true` 时创建成功后预建默认桶。
  默认桶名非法（例如账号名不足 3 位）时**不失败**：自动回退到 `<账号名>-bucket`；
  回退名仍不合法则跳过建桶、把 `autoCreateBucket` 置为 `false`，并在响应 `bucketNote`
  里说明「未自动建桶」及原因。**只有显式指定了非法桶名才返回 `400`。**
  列表/详情响应含 `bucket`、`autoCreateBucket`、`bucketExists`，以及顶层
  `endpoint` / `region` / `pathStyle`（详情窗口连接信息用；除 `?reveal=<name>` 外不返回明文 SK）。
- **Endpoint 运行时推导**：优先取环境变量 `OBJBOX_PUBLIC_ENDPOINT`（若设置），否则用请求的
  `X-Forwarded-Proto` + `X-Forwarded-Host`（或 `Host`）拼出，仓库内不写死任何真实域名。
- **桶管理**：桶名一律走 `ValidateBucket`（不放宽）；`GET buckets` 的 `objects` / `bytes`
  由 `internal/usage` 统计，`isDefault` 标记账号 `bucket` 字段指向的桶；删除默认桶允许，
  但**不会**自动重建（重建只发生在数据面请求时）。改默认桶 / 开关为原子落盘，热重载立即生效。
  审计动作 `accounts.bucket.create` / `accounts.bucket.delete` / `accounts.bucket.update`
  记录账号名与桶名；任何响应都不含明文 SK。

#### 桶管理接口示例

```http
GET /api/admin/accounts/demo/buckets
200 {"buckets":[{"name":"demo","objects":3,"bytes":4096,"isDefault":true}]}

POST /api/admin/accounts/demo/buckets
{"name":"photos"}
201 {"name":"photos"}
# 已存在 → 409 {"error":"桶已存在"}
# 非法桶名 → 400 {"error":"桶名不合法：..."}

DELETE /api/admin/accounts/demo/buckets/photos
200 {"name":"photos","message":"桶已删除"}
# 非空 → 409 {"error":"桶内还有对象，请先清空桶内对象再删除"}
# 不存在 → 404 {"error":"桶不存在"}

PATCH /api/admin/accounts/demo/bucket
{"bucket":"demo-bucket","autoCreateBucket":false}
200 {"name":"demo","bucket":"demo-bucket","autoCreateBucket":false,...}
```

- 账号不存在 → `404`；管理面未认证 → `401`。
- `builtin` 会话 cookie `objbox_admin` 带 `HttpOnly; SameSite=Lax; Path=/`；请求为 HTTPS
  （TLS 或 `X-Forwarded-Proto: https`）时另带 `Secure`，本地 http 调试不带。
- 会话令牌为无状态签名 `base64(user|expiry|epoch).HMAC`；`<data>/admin.json` 记录 `sessionEpoch`，
  `logout` 递增该值使所有既有会话立即失效（旧 cookie 未过期也不通过校验）。
- 管理页静态资源：`GET /`（浏览器或带 `Accept: text/html` 的客户端）返回内置 HTML，
  `GET /assets/*` 返回 JS/CSS；带 S3 签名的请求仍按协议层处理。
- 所有管理 API 调用写入 `<data>/admin-audit.log`（0600，一行一条：时间 / 操作 / 账号 / 来源 IP），不记录明文 SK。
- 账号表改动与 CLI 共用同一套原子读写逻辑（临时文件 + rename），服务端热重载立即生效。

#### 设置接口（读写跨域白名单）

```http
GET /api/admin/settings
200 {"corsOrigins":["https://app.example.com"],"corsEnabled":true,"corsSource":"settings"}
# corsSource: settings（来自设置文件）/ env（来自环境变量 CORS_ORIGINS）/ none（关闭）

PUT /api/admin/settings
{"corsOrigins":["https://app.example.com","http://127.0.0.1:5173"]}
200 {"corsOrigins":[...],"corsEnabled":true,"corsSource":"settings"}
# 非法来源 → 400 {"error":"第 2 行来源非法：ftp://bad.example.com（只支持 http 或 https）"}
# 缺字段 → 400 {"error":"缺少 corsOrigins 字段"}
```

- 写入为原子替换（临时文件 + fsync + rename），**保存即生效**，无需重启；`settings.json` 权限 0600。
- 文件损坏（JSON 非法或含非法来源）时，启动会用默认值并把原文件保留为 `settings.json.bak`，
  不影响服务启动。
- 审计动作 `settings.get` / `settings.update`。
