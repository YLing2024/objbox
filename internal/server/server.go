// Package server 负责 HTTP 装配：认证中间件 → 按账号分发到独立的 gofakes3 处理器。
//
// 每个账号一个 Backend（root 固定），因此账号之间在存储层就互相不可见；
// 跨账号或桶不存在的请求在分发前统一返回 403 AccessDenied，不泄露存在性。
package server

import (
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"os"

	"github.com/YLing2024/objbox/internal/account"
	adminauth "github.com/YLing2024/objbox/internal/admin"
	"github.com/YLing2024/objbox/internal/auth"
	"github.com/YLing2024/objbox/internal/backend"
	"github.com/YLing2024/objbox/internal/randstr"
	"github.com/YLing2024/objbox/internal/settings"
	"github.com/johannesboyne/gofakes3"
)

// Server 是本项目的 S3 HTTP 入口。
type Server struct {
	auth     *auth.Authenticator
	store    *account.Store
	admin    *adminauth.Service
	settings *settings.Store

	// AccessLog 为 true 时打印访问日志；Authorization 一律脱敏。
	AccessLog bool

	mu       sync.Mutex
	handlers map[string]http.Handler
	backends map[string]*backend.Backend
}

// New 构造 Server，并为当前所有账号预建后端（启动即重建索引）。
func New(store *account.Store) (*Server, error) {
	if store == nil {
		return nil, fmt.Errorf("server: account store 不能为空")
	}
	s := &Server{
		auth:     auth.NewAuthenticator(store),
		store:    store,
		handlers: map[string]http.Handler{},
		backends: map[string]*backend.Backend{},
	}
	adm, err := adminauth.New(store.DataDir(), adminauth.ModeFromEnv())
	if err != nil {
		return nil, err
	}
	s.admin = adm
	cfg, err := settings.Load(store.DataDir())
	if err != nil {
		s.Close()
		return nil, err
	}
	s.settings = cfg
	for _, a := range store.List() {
		if _, _, err := s.accountHandler(a); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// Close 关闭所有账号后端。
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for name, b := range s.backends {
		if err := b.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.backends, name)
		delete(s.handlers, name)
	}
	if s.admin != nil {
		if err := s.admin.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.admin = nil
	}
	return firstErr
}

// accountHandler 返回（必要时惰性创建）账号的协议处理器与后端。
func (s *Server) accountHandler(a *account.Account) (http.Handler, *backend.Backend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.handlers[a.Name]; ok {
		return h, s.backends[a.Name], nil
	}
	b, err := backend.New(a.Root)
	if err != nil {
		return nil, nil, fmt.Errorf("server: 初始化账号 %q 后端失败: %w", a.Name, err)
	}
	// 时钟容差与认证由本项目的 auth 层负责，关闭 gofakes3 自身的 skew 检查。
	g := gofakes3.New(b,
		gofakes3.WithTimeSkewLimit(0),
		gofakes3.WithHostBucket(false),
	)
	h := g.Server()
	s.backends[a.Name] = b
	s.handlers[a.Name] = h
	return h, b, nil
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 处理请求前先看 accounts.json 是否被 CLI 改过；有变化立即换表。
	// 解析失败时 MaybeReload 保留旧表，不会退化成全部 403。
	s.store.MaybeReload()

	id := newRequestID()
	r = r.WithContext(auth.WithRequestID(r.Context(), id))

	if s.AccessLog {
		log.Printf("%s %s auth=%s", r.Method, r.URL.Path, auth.MaskAuthorization(r.Header.Get("Authorization")))
	}

	// 管理面先于 S3 协议层路由：/api/admin/* 与管理页静态资源。
	// S3 端点仍完全走下面的 AK/SK 认证，不受 AUTH_MODE 影响。
	if s.admin != nil {
		if strings.HasPrefix(r.URL.Path, "/api/admin/") {
			s.serveAdminAPI(w, r)
			return
		}
	}

	// 跨域预检必须在签名校验之前短路：命中的来源直接回 204，不要求任何签名/认证。
	// 未命中的预检不额外回 CORS 头，交回下面的正常流程（其余行为不变）。
	if s.serveCORSPreflight(w, r) {
		return
	}

	if s.admin != nil && s.serveAdminStatic(w, r) {
		return
	}

	// 普通 S3 请求：来源命中白名单时补 CORS 头；所有 S3 响应带 Vary: Origin。
	// 管理面在上面的分支已返回，不受影响。
	corsOrigin := s.corsOriginFor(r)
	if corsOrigin != "" {
		setCORSHeaders(w.Header(), corsOrigin)
	}
	addVaryOrigin(w.Header())

	acct, err := s.auth.Authenticate(r)
	if err != nil {
		// 认证/预检错误直接写原始 writer：保持 M0「跨账号与桶不存在响应逐字节一致」。
		auth.WriteError(w, r, err)
		return
	}

	h, b, err := s.accountHandler(acct)
	if err != nil {
		auth.WriteError(w, r, auth.InternalErrorf("%v", err))
		return
	}

	// 请求即建桶：认证通过、拿到账号后端后，为允许的操作自动补建缺失的桶。
	if err := ensureBucketForRequest(r, acct, b); err != nil {
		auth.WriteError(w, r, err)
		return
	}

	if err := precheck(r, acct); err != nil {
		auth.WriteError(w, r, err)
		return
	}

	// 多区间只保留第一段（有意忽略多余区间，见 normalizeRange 注释）。
	normalizeRange(r)

	rw := newRespWriter(w, id, r.URL.Path, corsOrigin)
	if size, ok := rangeSizeHint(b, r); ok {
		rw.rangeSize, rw.hasRangeSize = size, true
	}

	if err := quotaPrecheck(r, acct, b); err != nil {
		// 配额拒绝发生在读取请求体之前；先把客户端还在发送的 body 读完，
		// 否则服务端直接关闭连接会让客户端收不到 403 响应（TCP RST）。
		drainRequestBody(r)
		auth.WriteError(w, r, err)
		return
	}

	h.ServeHTTP(rw, r.WithContext(auth.WithAccount(r.Context(), acct)))
	rw.finish()
}

// drainRequestBody 丢弃请求体（带上限，避免被超大请求拖住）。
func drainRequestBody(r *http.Request) {
	if r.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 128<<20))
	_ = r.Body.Close()
}

func newRequestID() string {
	b, err := randstr.Bytes(8)
	if err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}

// normalizeRange 处理多区间 Range：S3 单次 GET 不支持多段，这里有意「忽略多余区间」，
// 只保留第一段交给协议层；语法错误仍由协议层返回 416。
func normalizeRange(r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return
	}
	const prefix = "bytes="
	raw := r.Header.Get("Range")
	if raw == "" || !strings.HasPrefix(raw, prefix) || !strings.Contains(raw, ",") {
		return
	}
	first := strings.TrimSpace(strings.SplitN(raw[len(prefix):], ",", 2)[0])
	r.Header.Set("Range", prefix+first)
}

// rangeSizeHint 在带 Range 的对象 GET/HEAD 上查对象大小，供 416 生成 Content-Range。
func rangeSizeHint(b *backend.Backend, r *http.Request) (int64, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return 0, false
	}
	if r.Header.Get("Range") == "" {
		return 0, false
	}
	bucket, key := splitPath(r)
	if bucket == "" || key == "" {
		return 0, false
	}
	return b.ObjectSize(bucket, key)
}

// quotaPrecheck 在写入前做配额检查：quotaBytes<=0 表示不限。
//   - PUT 对象：当前用量 + 本次大小（CopyObject 取源对象大小）；
//   - 分片初始化：当前用量已超配额则拒绝；
//   - 分片完成：当前用量 + 已上传分片大小之和。
func quotaPrecheck(r *http.Request, acct *account.Account, b *backend.Backend) error {
	if acct.QuotaBytes <= 0 {
		return nil
	}
	bucket, key := splitPath(r)
	q := r.URL.Query()
	isInit := r.Method == http.MethodPost && q.Has("uploads") && q.Get("uploadId") == ""
	isComplete := r.Method == http.MethodPost && q.Get("uploadId") != ""
	isPut := r.Method == http.MethodPut && key != "" && !q.Has("uploadId") && !q.Has("uploads")
	if !isInit && !isComplete && !isPut {
		return nil
	}

	used, err := b.AccountUsage()
	if err != nil {
		return nil // 统计失败不阻断写入，避免因统计问题导致服务不可用
	}

	switch {
	case isInit:
		if used >= acct.QuotaBytes {
			return auth.QuotaExceeded()
		}
		return nil
	case isComplete:
		size, ok, err := b.MultipartTotalSize(bucket, key, q.Get("uploadId"))
		if err != nil || !ok {
			return nil // 交由协议层返回 NoSuchUpload
		}
		if used+size > acct.QuotaBytes {
			return auth.QuotaExceeded()
		}
		return nil
	case isPut:
		var add int64
		if src := r.Header.Get("x-amz-copy-source"); src != "" {
			if sb, sk, err := parseCopySource(src); err == nil {
				if size, ok := b.ObjectSize(sb, sk); ok {
					add = size
				}
			}
		} else if r.ContentLength > 0 {
			add = r.ContentLength
		}
		if used+add > acct.QuotaBytes {
			return auth.QuotaExceeded()
		}
	}
	return nil
}

// precheck 在进入协议层之前校验路径并做隔离判定：
//   - 非法 bucket/key → 400 InvalidRequest；
//   - autoCreateBucket=false：桶不在本账号 root 下 → 403 AccessDenied，
//     与「桶不存在」完全一致（M0 反枚举语义原样保留）；
//   - autoCreateBucket=true：桶缺失不再 403（自动建桶已在 ensureBucketForRequest
//     完成），放行后由协议层给出 NoSuchBucket / NoSuchKey / 空列表等语义；
//     CopyObject 例外，其源/目标桶缺失仍统一 403，绝不隐式建桶；
//   - CopyObject 的源桶不在本账号 root 下 → 403 AccessDenied（跨账号统一拒绝）。
func precheck(r *http.Request, acct *account.Account) error {
	bucket, key := splitPath(r)
	if bucket == "" {
		return nil
	}

	if err := backend.ValidateBucket(bucket); err != nil {
		return auth.InvalidRequestf("bucket 名非法: %s", err.Error())
	}
	if key != "" {
		if err := backend.ValidateKey(key); err != nil {
			return auth.InvalidRequestf("key 非法: %s", err.Error())
		}
	}

	// 建桶：PUT /bucket 且无子资源查询，允许桶尚不存在。
	if r.Method == http.MethodPut && key == "" && r.URL.RawQuery == "" {
		return nil
	}

	if !bucketInRoot(acct.Root, bucket) {
		if acct.AutoCreateBucket && !isCopyObject(r) {
			return nil
		}
		return auth.AccessDenied()
	}

	// CopyObject：源桶必须也在本账号 root 内，否则统一 403（跨账号语义）。
	if isCopyObject(r) {
		src := r.Header.Get("x-amz-copy-source")
		srcBucket, _, err := parseCopySource(src)
		if err != nil {
			return auth.InvalidRequestf("x-amz-copy-source 非法: %s", err.Error())
		}
		if !bucketInRoot(acct.Root, srcBucket) {
			return auth.AccessDenied()
		}
	}
	return nil
}

// isCopyObject 判断是否为 CopyObject（PUT 对象且带 x-amz-copy-source）。
func isCopyObject(r *http.Request) bool {
	return r.Method == http.MethodPut &&
		r.Header.Get("x-amz-copy-source") != "" &&
		splitKey(r) != ""
}

// splitKey 只取路径中的 key 部分。
func splitKey(r *http.Request) string {
	_, key := splitPath(r)
	return key
}

// ensureBucketForRequest 在请求已通过认证、按账号取得后端之后，为本账号
// autoCreateBucket=true 且桶缺失的「读/写对象/分片」请求自动补建桶。
//
// 只在当前账号自己的 root 下创建（复用 Backend.EnsureBucket），桶名照旧走
// ValidateBucket；CREATE 失败返回 5xx，绝不静默当作成功。DELETE 与 CopyObject
// 明确不建桶。
func ensureBucketForRequest(r *http.Request, acct *account.Account, b *backend.Backend) error {
	if !acct.AutoCreateBucket {
		return nil
	}
	bucket, key := splitPath(r)
	if bucket == "" {
		return nil
	}
	// 非法桶名交给 precheck 返回 400，不在这里建。
	if backend.ValidateBucket(bucket) != nil {
		return nil
	}
	if exists, _ := b.BucketExists(bucket); exists {
		return nil
	}
	if !autoCreateAllowed(r, key) {
		return nil
	}
	if err := b.EnsureBucket(bucket); err != nil {
		return auth.InternalErrorf("自动创建桶失败: %v", err)
	}
	return nil
}

// autoCreateAllowed 明确列出「桶不存在时自动建桶」的触发范围：
//   - PUT 对象（非 CopyObject、非 UploadPart）；
//   - POST 分片（?uploads / ?uploadId）；
//   - GET / HEAD（对象、桶列表）。
//
// DELETE 对象/桶、CopyObject、批量删除（?delete）一律不建桶。
func autoCreateAllowed(r *http.Request, key string) bool {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		if key == "" {
			return false // 显式 CreateBucket 走正常流程
		}
		if r.Header.Get("x-amz-copy-source") != "" {
			return false // CopyObject 不建桶
		}
		return !q.Has("uploadId") // UploadPart：无任务不应建桶
	case http.MethodPost:
		return q.Has("uploads") || q.Has("uploadId")
	case http.MethodGet, http.MethodHead:
		return true
	default:
		return false
	}
}

// parseCopySource 解析 x-amz-copy-source：形如 /<bucket>/<key>，可能整体 URL 编码，
// 可带 ?versionId=（本服务忽略版本）。
func parseCopySource(src string) (bucket, key string, err error) {
	raw := strings.TrimPrefix(strings.TrimSpace(src), "/")
	decoded, derr := url.QueryUnescape(raw)
	if derr == nil {
		raw = decoded
	}
	raw = strings.TrimPrefix(raw, "/")
	i := strings.IndexByte(raw, '/')
	if i <= 0 || i == len(raw)-1 {
		return "", "", fmt.Errorf("缺少 bucket/key 分隔")
	}
	bucket = raw[:i]
	key = strings.SplitN(raw[i+1:], "?", 2)[0]
	if bucket == "" || key == "" {
		return "", "", fmt.Errorf("bucket 或 key 为空")
	}
	return bucket, key, nil
}

// splitPath 把请求路径拆成 bucket/key；根路径返回空。
func splitPath(r *http.Request) (bucket, key string) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	if trimmed == "" {
		return "", ""
	}
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i], trimmed[i+1:]
	}
	return trimmed, ""
}

func bucketInRoot(root, bucket string) bool {
	if root == "" || backend.ValidateBucket(bucket) != nil {
		return false
	}
	fi, err := os.Stat(filepath.Join(root, bucket))
	return err == nil && fi.IsDir()
}
