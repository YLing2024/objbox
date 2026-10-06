// Package server 负责 HTTP 装配：认证中间件 → 按账号分发到独立的 gofakes3 处理器。
//
// 每个账号一个 Backend（root 固定），因此账号之间在存储层就互相不可见；
// 跨账号或桶不存在的请求在分发前统一返回 403 AccessDenied，不泄露存在性。
package server

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"os"

	"github.com/YLing2024/objbox/internal/account"
	"github.com/YLing2024/objbox/internal/auth"
	"github.com/YLing2024/objbox/internal/backend"
	"github.com/johannesboyne/gofakes3"
)

// Server 是本项目的 S3 HTTP 入口。
type Server struct {
	auth *auth.Authenticator

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
		handlers: map[string]http.Handler{},
		backends: map[string]*backend.Backend{},
	}
	for _, a := range store.List() {
		if _, err := s.handlerFor(a); err != nil {
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
	return firstErr
}

func (s *Server) handlerFor(a *account.Account) (http.Handler, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.handlers[a.Name]; ok {
		return h, nil
	}
	b, err := backend.New(a.Root)
	if err != nil {
		return nil, fmt.Errorf("server: 初始化账号 %q 后端失败: %w", a.Name, err)
	}
	// 时钟容差与认证由本项目的 auth 层负责，关闭 gofakes3 自身的 skew 检查。
	g := gofakes3.New(b,
		gofakes3.WithTimeSkewLimit(0),
		gofakes3.WithHostBucket(false),
	)
	h := g.Server()
	s.backends[a.Name] = b
	s.handlers[a.Name] = h
	return h, nil
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.AccessLog {
		log.Printf("%s %s auth=%s", r.Method, r.URL.Path, auth.MaskAuthorization(r.Header.Get("Authorization")))
	}
	acct, err := s.auth.Authenticate(r)
	if err != nil {
		auth.WriteError(w, r, err)
		return
	}
	if err := precheck(r, acct); err != nil {
		auth.WriteError(w, r, err)
		return
	}
	h, err := s.handlerFor(acct)
	if err != nil {
		auth.WriteError(w, r, auth.InternalErrorf("%v", err))
		return
	}
	h.ServeHTTP(w, r.WithContext(auth.WithAccount(r.Context(), acct)))
}

// precheck 在进入协议层之前校验路径并做隔离判定：
//   - 非法 bucket/key → 400 InvalidRequest；
//   - 对象级请求、桶级请求（除建桶）指向的桶不在本账号 root 下
//     → 403 AccessDenied，与“桶不存在”完全一致。
func precheck(r *http.Request, acct *account.Account) error {
	path := r.URL.Path
	if path == "" || path == "/" {
		return nil
	}

	trimmed := strings.TrimPrefix(path, "/")
	var bucket, key string
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		bucket, key = trimmed[:i], trimmed[i+1:]
	} else {
		bucket = trimmed
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
		return auth.AccessDenied()
	}
	return nil
}

func bucketInRoot(root, bucket string) bool {
	if root == "" || backend.ValidateBucket(bucket) != nil {
		return false
	}
	fi, err := os.Stat(filepath.Join(root, bucket))
	return err == nil && fi.IsDir()
}
