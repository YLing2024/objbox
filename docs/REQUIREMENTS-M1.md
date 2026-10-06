# objbox 需求书 · M1（协议完整 + 预签名）

> 自包含文档。前置：M0 已完成并上线（`/opt/objbox/objbox`，systemd `objbox.service`，数据 `/data/objbox`，
> 监听 `127.0.0.1:18930`，nginx 反代域名以 `s3.example.com` 占位）。M0 已有的能力**不要回退**：
> 账号/CLI/热重载/SigV4 header 校验/路径隔离/只读与停用/Bucket CRUD/Object CRUD/ListObjects V1&V2/Range 单区间/
> 磁盘为准的桶发现/`BucketNotEmpty` 语义。
> Python/Go 之外不要新增运行时依赖；**不得新增数据库或中间件**。

## 0. 铁律（与 M0 相同，必须遵守）

1. 每完成一个逻辑单元就 `git commit` 一次（conventional commits）；**禁止** `git push`、`git checkout -b`、`rebase`、`reset --hard`、`merge`；就在 `main`。
2. 禁止 `systemctl` / `service` / `kill` / `pkill` / `killall` / `fuser -k`；禁止改 `/root/proj/objbox` 之外的路径；命令带 `timeout`。
3. 遇到文档与仓库现实冲突（命名不同、能力已存在）以**仓库现实为准**，直接实现，不要提问。
4. 仓库内不得出现真实域名 / IP / 密钥；示例用 `s3.example.com`、`127.0.0.1` 占位。
5. 交付时工作区干净（`git status --short` 为空）。

## 1. 本阶段要实现的六件事

### 1.1 分片上传（Multipart Upload）—— 最高优先级
S3 客户端在对象大于阈值（rclone 默认 200MiB 左右、aws cli 默认 8MiB）时会走分片；**没有它 = 大文件传不了**。

必须实现并真实可用：
- `POST /<bucket>/<key>?uploads` → 创建分片任务，返回 `InitiateMultipartUploadResult`（含 `UploadId`）
- `PUT /<bucket>/<key>?partNumber=N&uploadId=ID` → 存分片（**流式落盘，不要整片读进内存**），响应头带该分片 `ETag`
- `GET /<bucket>/<key>?uploadId=ID` → `ListPartsResult`（含每个分片的 `PartNumber`/`ETag`/`Size`）
- `POST /<bucket>/<key>?uploadId=ID` → `CompleteMultipartUpload`：按请求体里给出的分片顺序拼接，**逐个校验 ETag 与大小**，不符 → `InvalidPart`（400）；成功返回最终 ETag（拼接后的 MD5 或 S3 风格 `-N` 后缀，二者择一并在 README/注释写清）
- `DELETE /<bucket>/<key>?uploadId=ID` → `AbortMultipartUpload`，清理临时分片
- `GET /<bucket>?uploads` → `ListMultipartUploadsResult`
- 未完成的分片任务**不得出现在** `ListObjects` 结果里（`.objbox` 内部目录也不能被当成对象）
- 分片临时文件放账号 root 的内部目录（如 `<root>/.objbox/multipart/<uploadId>/`），元数据（key/uploadId/分片清单）进 bbolt
- 并发上传多个分片必须安全（同一 uploadId 的不同 part 可并行）；同一 part 重复上传以最后一次为准
- 过期清理：启动时清理超过 24 小时未完成的 uploadId（简单实现即可）
- 最小分片大小 5MiB 的校验**不强制**（宽松接受更小的片，便于测试），但要在注释里写明这是有意放宽

### 1.2 批量删除
`POST /<bucket>?delete` 接受 XML 请求体（最多 1000 个 `<Object><Key>`），返回 `DeleteResult`（`Deleted` 与 `Error` 列表）。
不存在的 key 视为成功（S3 语义）。**只删自己账号 root 内的对象。**

### 1.3 CopyObject
`PUT /<bucket>/<key>` 带 `x-amz-copy-source: /<srcBucket>/<srcKey>`（可能 URL 编码，需解码；可带 `?versionId=` 忽略）：
- 同账号内复制（源与目标必须在**同一账号** root 内，跨账号 → 统一 403）
- 目标存在则覆盖；返回 `CopyObjectResult` XML（含 `ETag`、`LastModified`）
- 复制 0 字节对象、以及带 `/` 的深层 key 都要能用

### 1.4 预签名 URL（Presigned）—— 认证层要改
现状：认证中间件只认 `Authorization` 头。
要求：同时支持 **query 签名**——`?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=...&X-Amz-Date=...&X-Amz-Expires=N&X-Amz-SignedHeaders=host&X-Amz-Signature=...`
- 用**同一套** SigV4 重算比对逻辑（复用 `aws-sdk-go-v2/aws/signer/v4`），不要另写一套密码学
- `X-Amz-Expires` 上限 7 天（604800）；超时 → `403 AccessDenied`（`Request has expired` 文案）；过期后**必然**失效
- 支持 GET 与 PUT 预签名；GET 预签名要能配合 `Range`
- 预签名请求仍然受**账号隔离**约束（AK 决定账号，桶必须在它自己 root 内）
- 额外提供 CLI：`objbox presign -account <name> -bucket <b> -key <k> [-method GET|PUT] [-expires 3600]`，打印完整 URL（便于手机/浏览器直接下载）

### 1.5 HTTP 语义补齐
- 对象 `GET`/`HEAD` 响应头必须带：`Content-Type`、`Content-Length`、`ETag`、`Last-Modified`、`Accept-Ranges: bytes`
- `Range` 非法（越界/语法错）→ `416`（带 `Content-Range: bytes */<size>`）；多区间只支持单区间即可，但要**明确返回 416 或忽略多余区间**并在注释写清（不要返回错误格式的 body）
- 每个响应带 `x-amz-request-id`（随机 hex 即可）与 `Server: objbox`
- 错误响应必须是 S3 风格 XML（`<Error><Code>..</Code><Message>..</Message><Resource>..</Resource><RequestId>..</RequestId></Error>`）
- 允许的桶名规则依旧：`[a-z0-9.-]{3,63}`、首尾非 `-`/`.`；对象 key 上限 1024 字节

### 1.6 用量与配额
- `account list` 的 `USAGE(B)` 列继续可用（目录递归大小 + TTL 缓存即可）
- `quotaBytes > 0` 时：写入（PUT / multipart 初始化与 complete）前检查，超出 → `403`，错误码 `QuotaExceeded`；删除后应能继续写
- CLI 增加 `objbox account quota <name> <bytes>`

## 2. 验收标准（Hermes 会独立复核）

### 2.1 静态
```bash
timeout 300 go vet ./...              # 0 报错
timeout 600 go test ./... -count=1 -race   # 全绿
timeout 300 make build                # 产出 ./objbox
```

### 2.2 必写测试（表驱动，中文注释）
1. **Multipart 全流程**：初始化 → 传 3 个分片（其中一片 ≥5MiB）→ ListParts → Complete → GetObject 校验内容与 MD5 一致；`AbortMultipartUpload` 后 `ListObjects` 看不到残留、临时文件被清理。
2. **Multipart 异常**：ETag 不符 → `InvalidPart`；uploadId 不存在 → `NoSuchUpload`。
3. **批量删除**：3 个 key（含 1 个不存在）→ 返回 `Deleted` 3 条、无 `Error`；再 ListObjects 只剩预期。
4. **CopyObject**：复制后两份内容一致；源不存在 → `NoSuchKey`；跨账号源 → 403。
5. **预签名**：`expires=60` 的 GET 立即访问 200、内容正确；把 `X-Amz-Date` 改成 20 分钟前（模拟过期）→ 403；签名被篡改（改 key 或 signature）→ 403；PUT 预签名能成功上传。
6. **配额**：`quotaBytes=1MiB` 的账号，传 2MiB → 403 `QuotaExceeded`；删掉后传 512KiB → 成功。
7. **Range**：`bytes=0-9` → 206 + `Content-Range`；`bytes=99999999-` → 416。
8. **e2e（真 HTTP + aws-sdk-go-v2/service/s3 客户端）**：至少覆盖 `CreateMultipartUpload/UploadPart/Complete` 与 `GetObject`（大对象 ≥8MiB，验证走的是分片路径）。

### 2.3 手工冒烟（写进报告）
启动 `./objbox serve`，用 `curl` 打一个未签名请求确认仍是 403；用 `objbox presign` 生成一个链接并 `curl` 下载成功。

## 3. 文档要求
- 更新 `docs/REQUIREMENTS-M0.md` 的"非目标"清单（把本阶段已实现的项目移出）
- 新增 `docs/API.md`：列出**支持**与**明确不支持**的 S3 API 清单（含错误码表），供使用者对照
- `README.md` 增补"支持的 S3 操作"与"rclone / aws cli 用法示例"（**不要**出现真实域名，用 `s3.example.com`）

## 4. 交付报告
写到 `/tmp/objbox-m1-report.md` 并 `cat` 出来，包含：逐条对照 §1 的完成情况（做了什么/未做/原因）、§2 实测输出（`go vet`/`go test -race`/`make build` 结尾）、`git log --oneline`（每笔对应改动点）、已知限制。
