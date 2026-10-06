# objbox

极简自建 S3 兼容对象存储：**一个账号一套 AK/SK、一个账号一个隔离空间**，单二进制、无数据库、无中间件。

- 语言：Go；HTTP 用标准库 `net/http`
- S3 协议层：[gofakes3](https://github.com/johannesboyne/gofakes3)（只做路由/XML，不做认证）
- 认证：SigV4 头签名 + 预签名 URL，用 `aws-sdk-go-v2/aws/signer/v4` 重算比对
- 元数据：bbolt（纯 Go 嵌入式 KV）；对象内容直接落盘

> 本文档中的端点一律用占位 `https://s3.example.com`，请替换为你自己的地址。

## 构建与运行

```bash
make build                 # 产出 ./objbox
./objbox serve -addr 127.0.0.1:18930 -data /var/lib/objbox
```

账号管理（`accounts.json` 权限 0600，含明文 SK）：

```bash
./objbox account add demo -note "示例"
./objbox account list
./objbox account quota demo 1073741824   # 1GiB，0 表示不限
./objbox account rotate demo
./objbox account disable demo
./objbox account enable demo
./objbox account remove demo
```

生成预签名下载链接（便于手机/浏览器直接下载）：

```bash
./objbox presign -account demo -bucket my-bucket -key path/to/file \
  -method GET -expires 3600 -endpoint https://s3.example.com
```

## 支持的 S3 操作

服务级：`ListBuckets`

桶级：`CreateBucket` / `DeleteBucket`（非空拒绝）/ `HeadBucket` /
`ListObjects`（V1 与 V2，支持 prefix、delimiter、分页）/
`DeleteObjects`（批量删，最多 1000 个 key）/ `ListMultipartUploads`

对象级：`PutObject` / `GetObject`（单区间 `Range`）/ `HeadObject` / `DeleteObject` /
`CopyObject`（同账号内，`x-amz-copy-source`）

分片上传：`CreateMultipartUpload` / `UploadPart` / `ListParts` /
`CompleteMultipartUpload` / `AbortMultipartUpload`（分片流式落盘，不整片读入内存）

认证：SigV4 `Authorization` 头；预签名 URL（GET / PUT，有效期上限 7 天）

配额：`quotaBytes > 0` 时写入前检查，超出返回 `403 QuotaExceeded`

完整清单与**明确不支持**的 API、错误码表见 [`docs/API.md`](docs/API.md)。

## rclone 用法示例

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

## aws cli 用法示例

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
aws --endpoint-url "$ENDPOINT" s3api copy-object \
  --bucket my-bucket --key big-copy.bin \
  --copy-source my-bucket/big.bin
aws --endpoint-url "$ENDPOINT" s3 rm s3://my-bucket/big.bin
```

预签名（由 objbox CLI 生成后直接用 curl/浏览器访问）：

```bash
URL=$(./objbox presign -account demo -bucket my-bucket -key big.bin \
  -endpoint https://s3.example.com)
curl -o big.bin "$URL"
```

## 已知限制

- 不支持 Virtual-Hosted-Style、对象版本、桶/对象 ACL、多区间 Range、匿名公开访问等，详见 `docs/API.md`。
- 分片上传不强制最小分片 5MiB（有意放宽，便于测试）。
- 分片最终对象 ETag 为拼接内容的 MD5，不使用 S3 的 `-N` 复合形式。
