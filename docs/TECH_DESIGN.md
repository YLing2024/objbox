# objbox 技术方案（v1 讨论稿）

> 名字待定：`objbox` / `objhub`（改名成本为零）。下文统一用 objbox。
> 定位一句话：**极简自建 S3 兼容对象存储 —— 一个账号一套 AK/SK、一个账号一个隔离空间、不引入用户系统。**

## 1. 设计动机

davbox 补的是 WebDAV 那薄薄一层（账号管理 + 可信目录隔离），协议不重写。
objbox 是同一件事在对象存储上的复制：市面方案（MinIO / Garage / SeaweedFS）都是"运维平台"体量
（多节点、纠删码、集群概念），而我们只要**一根尾巴**：几个账号、每账号一套密钥、彼此看不见。

**概念只有两个**：`admin`（管账号） · `账号`（拿 AK/SK 存取对象）。没有注册、没有登录页、没有用户表。

## 2. 技术选型

| 项 | 选择 | 理由 |
|---|---|---|
| 语言 | **Go 1.26，单二进制** | 与 davbox 同构；交叉编译、无运行时依赖 |
| HTTP | 标准库 `net/http` | 零框架 |
| 签名校验 | **SigV4，复用 `aws-sdk-go-v2/aws/signer/v4` 重算比对** | 不自研密码学；只在"校验"一侧用，不引入 SDK 全家桶 |
| XML | `encoding/xml` | 标准库 |
| 元数据 | **纯文件系统 + sidecar JSON**（无数据库） | 无中间件；"停掉不伤心" |
| 前端 | **原生 HTML/CSS/JS，无构建步骤**，后端自带静态服务 | 与 davbox 一致；nginx 只做反代 |
| CLI | 标准库 `flag` 手写子命令 | 不引 cobra |

## 3. 存储布局

```
/data/objbox/
├── accounts.json                 # 账号表（0600）
├── admin-password.txt            # 首次启动生成（0600）
└── roots/
    └── <account>/                # 账号的隔离根 = 它能触及的全部空间
        ├── <bucket>/
        │   └── <key>             # 对象直接落盘，key 里的 / 即目录
        └── .objbox/
            ├── meta/<bucket>/<key>.json    # ETag / Content-Type / 自定义元数据
            └── multipart/<uploadId>/       # 分片上传临时区（part 号 + 汇总）
```

- 写入：**临时文件 → fsync → rename 原子替换**（读者永远看不到半个对象）
- 元数据放 `.objbox/meta/` 而不是对象旁（不污染列表、不依赖 xattr 支持）
- 删除对象时同时删 meta；`Account > Bucket > Key` 三级一一对应

## 4. accounts.json

```json
{
  "version": 1,
  "accounts": [
    {
      "name": "devbox",
      "ak": "OBJ7F3KQ2M9XZ4WTVD6RYH1BN8C5PL",
      "sk": "<32 字节随机，base32/hex>",
      "root": "/data/objbox/roots/devbox",
      "readonly": false,
      "disabled": false,
      "quotaBytes": 0,
      "note": "开发环境对象存储"
    }
  ]
}
```

- AK：`OBJ` 前缀 + 随机，便于一眼认出、便于 grep
- SK：生成时**只在终端/弹窗打印一次**，之后仅存明文于 0600 文件（见 §7 说明）
- 轮换：`objbox account rotate <name>` → 新 SK 生效、旧 SK **立即失效**

## 5. S3 API 子集

**M1 必须**
- 服务级：`GET /` → ListBuckets
- 桶级：`PUT` / `DELETE` / `HEAD`；`GET ?list-type=2`（ListObjectsV2）、`GET`（V1）、`POST ?delete`（批量删）
- 对象级：`PUT` / `GET`（支持 **Range**）/ `HEAD` / `DELETE` / CopyObject（`x-amz-copy-source`）
- 分片：Initiate / UploadPart / Complete / Abort / ListParts （大文件刚需，必须进 M1）
- **预签名 URL**：GET / PUT（`X-Amz-Signature` 查询参数），用于临时分享
- 签名：SigV4（header + presign）、`UNSIGNED-PAYLOAD`、时钟容差 ±15 分钟

**明确非目标（不做）**：ACL/Policy 引擎、版本控制、对象锁、生命周期、事件通知、STS 临时凭证、跨区复制、S3 Select、静态网站托管。
权限模型只保留一个开关：**账号级 `readonly`**。

## 6. 隔离与安全

| 场景 | 行为 |
|---|---|
| AK → 账号映射 | AK 唯一映射到一个账号，root 在进程内写死为白名单 |
| 跨账号访问 | 签名合法但桶不属于自己 → **403 AccessDenied**（与"桶不存在"统一回复，防枚举） |
| 路径逃逸 | `..`、`%2e%2e`、反斜杠、空字节、绝对路径、超长 key → **400** |
| 只读账号 | PUT/POST/DELETE → 403 |
| 停用账号 | 一切请求 → 403 |
| 轮换 SK | 旧 SK 立即失效 |
| 日志 | access log 里 `Authorization` 一律脱敏，永不落盘明文 |
| 限流 | 每 AK 令牌桶（可选，防爆破） |

## 7. 一个必须讲清的取舍：SK 明文落盘

S3 的请求校验要求服务端拿到**明文 SK** 才能重算 HMAC 链 —— 所以 SK 无法只存哈希。
做法与 davbox 的 `accounts.json` 一致：**明文 + 文件权限 0600 + 只在自己机器上**。
（可选增强：`OBJBOX_MASTER_KEY` 环境变量 → AES-GCM 加密 SK 落盘，解密只在内存；列为 M3 可选项。）

## 8. 管理面

- **CLI**：`objbox serve` · `account add|list|remove|rotate|disable|enable` · `usage <name>`
- **Web admin（M3）**：单页
  - 账号列表：AK、SK（掩码）、root、用量、状态
  - 新增账号 / 轮换 SK → **弹窗显示一次 + 一键复制**
  - 一键复制连接信息（Endpoint / Region / AK / SK）
- **认证模式**：`AUTH_MODE=builtin`（默认，自带管理员口令）· `AUTH_MODE=sso`（管理端交给 Auth Gateway，读 `X-Auth-User`，缺 → 401）
- **铁律（沿用 davbox）**：**S3 协议端点永远走 AK/SK，不受 AUTH_MODE 影响。**

## 9. 部署

- systemd 单二进制：`/opt/objbox/objbox`，数据 `/data/objbox`，监听 `127.0.0.1:18930`
- nginx：`s3.s3.example.com` → 反代
  - 必须放开 `client_max_body_size`（大对象）
  - 上传路径 `proxy_request_buffering off`（否则大文件先落 nginx 临时盘）
  - 保持 `Host` 头原样（SigV4 的 CanonicalRequest 含 Host）
- Docker 多阶段构建（可选）：静态二进制 → alpine 镜像，体积目标 < 30 MB
- 备份：`/data/objbox` 纳入现有每日全量备份线

## 10. 里程碑

| 阶段 | 内容 | 验收方式 |
|---|---|---|
| **M0** | 骨架 + accounts.json + CLI add/list + SigV4 校验 + ListBuckets + Put/Get/Head/Delete + 逃逸拒绝 | `aws s3` CLI 真跑通 |
| **M1** | ListV2/V1、批量删、CopyObject、Range、分片上传、预签名 URL、用量统计 | `aws s3 cp` 大文件 + `rclone` |
| **M2** | 隔离与安全用例全绿（跨账号/只读/停用/逃逸） | 逐条打真实请求 |
| **M3** | Web admin 页 + `AUTH_MODE=sso` 对接 Auth Gateway | 浏览器真人路径 |
| **M4** | Docker + 双语 README + 部署 `s3.s3.example.com` + 隐私扫描 → 转公开 | 线上真机 |

## 11. 需要拍板的点

1. **名字**：`objbox` / `objhub` / 你另给
2. 桶模型：**每账号可建多个桶**（推荐，兼容所有 S3 客户端）还是"一账号一桶"（更简）
3. 元数据：**纯文件系统 + sidecar**（推荐，无数据库）确认
4. SK 明文落盘（0600）确认，主密钥加密要不要做
5. S3 子集范围：上面清单有没有要加/砍的（版本控制？桶策略？）
6. Web admin 要不要（M3），还是只要 CLI
7. 端口 `18930` / 数据目录 `/data/objbox` / 域名 `s3.s3.example.com` 确认
8. 是否要对外提供"公开只读桶"（对象级匿名 GET）
