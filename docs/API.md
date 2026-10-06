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

### 用量与配额

- `objbox account list` 的 `USAGE(B)` 列为账号 root 下所有桶目录的递归大小（不含 `.objbox` 内部目录）。
- `quotaBytes > 0` 时，写入（`PutObject` / `CopyObject` / 分片初始化 / 分片完成）前检查；
  超出返回 `403 QuotaExceeded`。删除对象后用量下降，可继续写入。
- `objbox account quota <name> <bytes>` 设置配额，`<bytes>=0` 表示不限。

## 明确不支持

以下 S3 能力当前**不实现**，请求会得到 `NotImplemented`、`MethodNotAllowed` 或按普通 404/403 处理：

- 桶级配置类 API：versioning、ACL、CORS、lifecycle、policy、tagging、encryption、
  logging、notification、replication、website、inventory、analytics、metrics、
  accelerate、requestPayment、object-lock、public-access-block、ownership-controls。
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
