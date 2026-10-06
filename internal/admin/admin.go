// Package admin 实现管理面的认证与会话，与 S3 协议面完全隔离。
//
// 认证模式由环境变量 AUTH_MODE 决定：
//
//   - builtin（默认）：首次启动生成随机管理员口令，明文写入 <data>/admin-password.txt
//     （0600），口令的 bcrypt 哈希存 <data>/admin.json；会话 cookie 用
//     <data>/secret.key（0600）签名。
//   - sso：不做任何自带登录，只信任网关注入的 X-Auth-User 头，缺失即未认证。
//
// 铁律：本包只服务管理面；S3 端点始终走 AK/SK，不受 AUTH_MODE 影响。
package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YLing2024/objbox/internal/randstr"
	"golang.org/x/crypto/bcrypt"
)

// 认证模式取值。
const (
	ModeBuiltin = "builtin"
	ModeSSO     = "sso"
)

// 管理面文件名与约束。
const (
	// CookieName 是管理会话 cookie 名；不使用 __Host- 前缀以便 nginx 反代。
	CookieName = "objbox_admin"
	// SessionTTL 是会话有效期。
	SessionTTL = 12 * time.Hour

	// HeaderUser 是 sso 模式下网关注入的用户头。
	HeaderUser = "X-Auth-User"

	passwordFile = "admin-password.txt"
	hashFile     = "admin.json"
	secretFile   = "secret.key"
	// AuditFileName 是管理审计日志文件名。
	AuditFileName = "admin-audit.log"

	// 登录失败限速：同一 IP 每分钟最多 maxLoginFailures 次失败。
	loginWindow      = time.Minute
	maxLoginFailures = 10
)

// ModeFromEnv 读取 AUTH_MODE，缺省或未知一律按 builtin。
func ModeFromEnv() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AUTH_MODE"))) {
	case ModeSSO:
		return ModeSSO
	default:
		return ModeBuiltin
	}
}

// Service 持有管理面认证状态。
type Service struct {
	mode    string
	dataDir string
	now     func() time.Time

	hash   []byte
	secret []byte

	// epoch 是会话版本号：登录时写入签发值，校验时要求与当前值相等；
	// logout 时 +1 并原子落盘，使所有既有会话立即失效。
	epochMu sync.Mutex
	epoch   int64

	passwordPath string
	hashPath     string
	secretPath   string

	rlMu sync.Mutex
	rl   map[string]*attempt

	audit *auditLog
}

// adminState 是 <data>/admin.json 的磁盘结构。
type adminState struct {
	PasswordHash string `json:"passwordHash"`
	SessionEpoch int64  `json:"sessionEpoch"`
}

type attempt struct {
	count int
	start time.Time
}

// New 构造管理面服务。builtin 模式下会按需生成口令与签名密钥文件。
func New(dataDir, mode string) (*Service, error) {
	if dataDir == "" {
		return nil, errors.New("admin: 数据目录不能为空")
	}
	if mode != ModeSSO {
		mode = ModeBuiltin
	}
	s := &Service{
		mode:         mode,
		dataDir:      dataDir,
		now:          time.Now,
		rl:           map[string]*attempt{},
		passwordPath: filepath.Join(dataDir, passwordFile),
		hashPath:     filepath.Join(dataDir, hashFile),
		secretPath:   filepath.Join(dataDir, secretFile),
	}
	if mode == ModeBuiltin {
		if err := s.loadOrCreatePassword(); err != nil {
			return nil, err
		}
		if err := s.loadOrCreateSecret(); err != nil {
			return nil, err
		}
	}
	a, err := newAuditLog(filepath.Join(dataDir, AuditFileName))
	if err != nil {
		return nil, err
	}
	s.audit = a
	return s, nil
}

// AuthMode 返回当前认证模式。
func (s *Service) AuthMode() string { return s.mode }

// DataDir 返回数据目录。
func (s *Service) DataDir() string { return s.dataDir }

// PasswordFilePath 返回 builtin 口令明文文件路径（供部署方查看）。
func (s *Service) PasswordFilePath() string { return s.passwordPath }

// loadOrCreatePassword 加载 bcrypt 哈希与会话版本号；不存在时生成随机口令并同时落盘明文与哈希。
func (s *Service) loadOrCreatePassword() error {
	raw, err := os.ReadFile(s.hashPath)
	if err == nil {
		var f adminState
		if uerr := json.Unmarshal(raw, &f); uerr != nil || f.PasswordHash == "" {
			return fmt.Errorf("admin: 解析 %s 失败: %w", s.hashPath, uerr)
		}
		s.hash = []byte(f.PasswordHash)
		s.epoch = f.SessionEpoch
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("admin: 读取 %s 失败: %w", s.hashPath, err)
	}

	pw, err := randstr.Token(24) // 32 字符随机口令
	if err != nil {
		return fmt.Errorf("admin: 生成口令失败: %w", err)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("admin: 生成口令哈希失败: %w", err)
	}
	if err := writeFile0600(s.passwordPath, []byte(pw+"\n")); err != nil {
		return err
	}
	s.hash = h
	s.epoch = 0
	return s.saveAdminState()
}

// saveAdminState 原子写回口令哈希与会话版本号（0600）。
func (s *Service) saveAdminState() error {
	blob, err := json.MarshalIndent(adminState{
		PasswordHash: string(s.hash),
		SessionEpoch: s.epoch,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("admin: 序列化管理状态失败: %w", err)
	}
	return writeFile0600(s.hashPath, append(blob, '\n'))
}

// InvalidateSessions 递增会话版本号并原子落盘，使所有既有会话立即失效。
//
// builtin 模式为单管理员场景，登出即使全部会话失效是可接受的。
func (s *Service) InvalidateSessions() error {
	s.epochMu.Lock()
	defer s.epochMu.Unlock()
	s.epoch++
	return s.saveAdminState()
}

// loadOrCreateSecret 加载会话签名密钥；不存在或过短时生成 32 字节随机密钥。
func (s *Service) loadOrCreateSecret() error {
	raw, err := os.ReadFile(s.secretPath)
	if err == nil && len(raw) >= 16 {
		s.secret = raw
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("admin: 读取 %s 失败: %w", s.secretPath, err)
	}
	b, err := randstr.Bytes(32)
	if err != nil {
		return fmt.Errorf("admin: 生成签名密钥失败: %w", err)
	}
	if err := writeFile0600(s.secretPath, b); err != nil {
		return err
	}
	s.secret = b
	return nil
}

// AllowLogin 判断该 IP 当前是否允许再尝试登录（失败次数未超上限）。
func (s *Service) AllowLogin(ip string) bool {
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	a := s.rl[ip]
	if a == nil {
		return true
	}
	if s.now().Sub(a.start) >= loginWindow {
		delete(s.rl, ip)
		return true
	}
	return a.count < maxLoginFailures
}

// RecordFailure 记录一次登录失败；跨窗口则重新计数。
func (s *Service) RecordFailure(ip string) {
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	now := s.now()
	a := s.rl[ip]
	if a == nil || now.Sub(a.start) >= loginWindow {
		s.rl[ip] = &attempt{count: 1, start: now}
		return
	}
	a.count++
	if len(s.rl) > 4096 {
		s.pruneLocked(now)
	}
}

// ResetFailures 登录成功后清零该 IP 的失败计数。
func (s *Service) ResetFailures(ip string) {
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	delete(s.rl, ip)
}

func (s *Service) pruneLocked(now time.Time) {
	for k, a := range s.rl {
		if now.Sub(a.start) >= loginWindow {
			delete(s.rl, k)
		}
	}
}

// VerifyPassword 校验管理员口令（仅 builtin 模式）。
func (s *Service) VerifyPassword(password string) bool {
	if s.mode != ModeBuiltin || len(s.hash) == 0 {
		return false
	}
	return bcrypt.CompareHashAndPassword(s.hash, []byte(password)) == nil
}

// Authenticate 解析当前管理面用户。
//
// sso：只信任 X-Auth-User，缺失返回未认证；
// builtin：校验签名 cookie，无 cookie 或签名/有效期不符返回未认证。
func (s *Service) Authenticate(r *http.Request) (string, bool) {
	if s.mode == ModeSSO {
		if u := strings.TrimSpace(r.Header.Get(HeaderUser)); u != "" {
			return u, true
		}
		return "", false
	}
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return s.verifySession(c.Value)
}

// SetSessionCookie 写入已签名的会话 cookie。
//
// 请求来自 HTTPS（TLS 或 X-Forwarded-Proto: https）时附带 Secure，
// 本地 http 调试则不设，避免登录后无法保持会话。
func (s *Service) SetSessionCookie(w http.ResponseWriter, r *http.Request, user string) {
	exp := s.now().Add(SessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    s.signSession(user, exp),
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
		MaxAge:   int(SessionTTL.Seconds()),
	})
}

// ClearSessionCookie 让会话 cookie 立即过期。
func (s *Service) ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// requestIsHTTPS 判断请求是否来自 HTTPS：TLS 直连，或网关转发的
// X-Forwarded-Proto 为 https。
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	p := r.Header.Get("X-Forwarded-Proto")
	if p == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(strings.Split(p, ",")[0]), "https")
}

func (s *Service) signSession(user string, exp time.Time) string {
	s.epochMu.Lock()
	epoch := s.epoch
	s.epochMu.Unlock()
	payload := user + "|" + strconv.FormatInt(exp.Unix(), 10) + "|" + strconv.FormatInt(epoch, 10)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Service) verifySession(token string) (string, bool) {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 || i == len(token)-1 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(token[:i])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(token[i+1:])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return "", false
	}
	parts := strings.SplitN(string(payload), "|", 3)
	if len(parts) != 3 || parts[0] == "" {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", false
	}
	epoch, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", false
	}
	s.epochMu.Lock()
	cur := s.epoch
	s.epochMu.Unlock()
	if epoch != cur {
		// 已登出（或版本号被提升）：旧会话立即失效。
		return "", false
	}
	if !s.now().Before(time.Unix(exp, 0)) {
		return "", false
	}
	return parts[0], true
}

// ClientIP 从请求中解析来源 IP：优先网关注入的 X-Forwarded-For / X-Real-IP，
// 否则退回 RemoteAddr。
func ClientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Audit 追加一条审计记录。op 为操作名，account 为目标账号（无则 "-"）。
func (s *Service) Audit(op, account string, r *http.Request) {
	if s.audit == nil {
		return
	}
	s.audit.write(s.now(), op, account, ClientIP(r))
}

// Close 关闭审计日志文件。
func (s *Service) Close() error {
	if s.audit == nil {
		return nil
	}
	return s.audit.Close()
}

// auditLog 是追加写的审计日志（0600）。
type auditLog struct {
	mu sync.Mutex
	f  *os.File
}

func newAuditLog(path string) (*auditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("admin: 创建审计日志目录失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("admin: 打开审计日志失败: %w", err)
	}
	return &auditLog{f: f}, nil
}

func (a *auditLog) write(now time.Time, op, account, ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	line := fmt.Sprintf("%s op=%s account=%s ip=%s\n",
		now.UTC().Format(time.RFC3339), sanitizeField(op), sanitizeField(account), sanitizeField(ip))
	_, _ = a.f.WriteString(line)
}

func (a *auditLog) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.f.Close()
}

// sanitizeField 去掉换行与空白，避免审计日志被注入伪造行。
func sanitizeField(s string) string {
	if s == "" {
		return "-"
	}
	r := strings.NewReplacer("\n", "_", "\r", "_", " ", "_", "\t", "_")
	return r.Replace(s)
}

// writeFile0600 原子地以 0600 写入敏感文件（临时文件 + rename）。
func writeFile0600(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("admin: 创建目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".admin-*.tmp")
	if err != nil {
		return fmt.Errorf("admin: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("admin: 替换 %s 失败: %w", path, err)
	}
	return nil
}
