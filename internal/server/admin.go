package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/YLing2024/objbox/internal/account"
	adminauth "github.com/YLing2024/objbox/internal/admin"
	"github.com/YLing2024/objbox/internal/usage"
)

// Version 为二进制版本号，出现在管理面 overview 中。
const Version = "0.3.0"

// adminAccount 是管理面账号列表项；SK 默认掩码，仅在 reveal 时返回明文。
type adminAccount struct {
	Name       string `json:"name"`
	AK         string `json:"ak"`
	SK         string `json:"sk"`
	Root       string `json:"root"`
	UsageBytes int64  `json:"usageBytes"`
	Status     string `json:"status"`
	Note       string `json:"note"`
	QuotaBytes int64  `json:"quotaBytes"`
	Readonly   bool   `json:"readonly"`
	Disabled   bool   `json:"disabled"`
}

func accountStatus(a *account.Account) string {
	switch {
	case a.Disabled:
		return "disabled"
	case a.Readonly:
		return "readonly"
	default:
		return "enabled"
	}
}

func newAdminAccount(a *account.Account, sk string) adminAccount {
	size, _ := usage.DirSize(a.Root)
	return adminAccount{
		Name:       a.Name,
		AK:         a.AK,
		SK:         sk,
		Root:       a.Root,
		UsageBytes: size,
		Status:     accountStatus(a),
		Note:       a.Note,
		QuotaBytes: a.QuotaBytes,
		Readonly:   a.Readonly,
		Disabled:   a.Disabled,
	}
}

// ---- JSON 辅助 ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(dst)
}

// serveAdminAPI 分发 /api/admin/*。除 login 外一律要求已认证。
func (s *Server) serveAdminAPI(w http.ResponseWriter, r *http.Request) {
	if strings.TrimPrefix(r.URL.Path, "/api/admin/") == "login" {
		s.adminLogin(w, r)
		return
	}
	user, ok := s.admin.Authenticate(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "未认证")
		return
	}

	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/"), "/")
	switch {
	case rest == "logout":
		s.adminLogout(w, r, user)
	case rest == "accounts":
		s.adminAccounts(w, r)
	case strings.HasPrefix(rest, "accounts/"):
		s.adminAccountItem(w, r, strings.TrimPrefix(rest, "accounts/"))
	case rest == "overview":
		s.adminOverview(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "接口不存在")
	}
}

// adminLogin：builtin 口令登录；sso 模式不提供口令登录（一律 401）。
func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	if s.admin.AuthMode() == adminauth.ModeSSO {
		s.admin.Audit("login", "-", r)
		writeJSONError(w, http.StatusUnauthorized, "SSO 模式下不提供口令登录")
		return
	}
	ip := adminauth.ClientIP(r)
	if !s.admin.AllowLogin(ip) {
		s.admin.Audit("login.blocked", "-", r)
		writeJSONError(w, http.StatusTooManyRequests, "尝试过于频繁，请稍后再试")
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体非法")
		return
	}
	if !s.admin.VerifyPassword(body.Password) {
		s.admin.RecordFailure(ip)
		s.admin.Audit("login", "-", r)
		writeJSONError(w, http.StatusUnauthorized, "口令错误")
		return
	}
	s.admin.ResetFailures(ip)
	s.admin.SetSessionCookie(w, "admin")
	s.admin.Audit("login", "admin", r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": "admin"})
}

func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request, user string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	s.admin.ClearSessionCookie(w)
	s.admin.Audit("logout", user, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.adminListAccounts(w, r)
	case http.MethodPost:
		s.adminCreateAccount(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET 或 POST")
	}
}

// adminListAccounts 默认掩码 SK；?reveal=<name> 时仅该账号返回明文（已鉴权）。
func (s *Server) adminListAccounts(w http.ResponseWriter, r *http.Request) {
	reveal := r.URL.Query().Get("reveal")
	op := "accounts.list"
	if reveal != "" {
		op = "accounts.reveal"
	}
	out := make([]adminAccount, 0)
	for _, a := range s.store.List() {
		sk := account.MaskSecret(a.SK)
		if reveal != "" && reveal == a.Name {
			sk = a.SK
		}
		out = append(out, newAdminAccount(a, sk))
	}
	s.admin.Audit(op, reveal, r)
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (s *Server) adminCreateAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		Note       string `json:"note"`
		Readonly   bool   `json:"readonly"`
		QuotaBytes *int64 `json:"quotaBytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体非法")
		return
	}
	name := strings.TrimSpace(body.Name)
	if !account.ValidName(name) {
		writeJSONError(w, http.StatusBadRequest, "账号名需匹配 [a-z0-9][a-z0-9-]{0,31}（不得以 - 开头）")
		return
	}
	acct, err := s.store.Add(name, body.Note, body.Readonly)
	if err != nil {
		writeJSONError(w, accountErrorStatus(err), err.Error())
		return
	}
	if body.QuotaBytes != nil {
		if err := s.store.SetQuota(name, *body.QuotaBytes); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		acct.QuotaBytes = *body.QuotaBytes
	}
	s.admin.Audit("accounts.create", name, r)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":      acct.Name,
		"ak":        acct.AK,
		"sk":        acct.SK,
		"root":      acct.Root,
		"note":      acct.Note,
		"readonly":  acct.Readonly,
		"endpoint":  requestEndpoint(r),
		"region":    "us-east-1",
		"pathStyle": true,
	})
}

// adminAccountItem 处理 /api/admin/accounts/<name>[/<action>]。
func (s *Server) adminAccountItem(w http.ResponseWriter, r *http.Request, rest string) {
	segs := strings.Split(rest, "/")
	name := segs[0]
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "缺少账号名")
		return
	}
	if len(segs) == 1 {
		switch r.Method {
		case http.MethodPatch:
			s.adminUpdateAccount(w, r, name)
		case http.MethodDelete:
			s.adminDeleteAccount(w, r, name)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 PATCH 或 DELETE")
		}
		return
	}
	if len(segs) != 2 || r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "不支持的操作")
		return
	}
	switch segs[1] {
	case "rotate":
		sk, err := s.store.Rotate(name)
		if err != nil {
			writeJSONError(w, accountErrorStatus(err), err.Error())
			return
		}
		s.admin.Audit("accounts.rotate", name, r)
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "sk": sk})
	case "disable":
		s.adminSetDisabled(w, r, name, true)
	case "enable":
		s.adminSetDisabled(w, r, name, false)
	default:
		writeJSONError(w, http.StatusNotFound, "接口不存在")
	}
}

func (s *Server) adminSetDisabled(w http.ResponseWriter, r *http.Request, name string, disabled bool) {
	if err := s.store.SetDisabled(name, disabled); err != nil {
		writeJSONError(w, accountErrorStatus(err), err.Error())
		return
	}
	op := "accounts.enable"
	if disabled {
		op = "accounts.disable"
	}
	s.admin.Audit(op, name, r)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "disabled": disabled})
}

func (s *Server) adminUpdateAccount(w http.ResponseWriter, r *http.Request, name string) {
	var body struct {
		Note       *string `json:"note"`
		QuotaBytes *int64  `json:"quotaBytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体非法")
		return
	}
	if body.Note == nil && body.QuotaBytes == nil {
		writeJSONError(w, http.StatusBadRequest, "无可更新字段")
		return
	}
	if body.QuotaBytes != nil && *body.QuotaBytes < 0 {
		writeJSONError(w, http.StatusBadRequest, "配额必须是非负整数")
		return
	}
	acct, err := s.store.Update(name, body.Note, body.QuotaBytes)
	if err != nil {
		writeJSONError(w, accountErrorStatus(err), err.Error())
		return
	}
	s.admin.Audit("accounts.update", name, r)
	writeJSON(w, http.StatusOK, newAdminAccount(acct, account.MaskSecret(acct.SK)))
}

func (s *Server) adminDeleteAccount(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.store.Remove(name); err != nil {
		writeJSONError(w, accountErrorStatus(err), err.Error())
		return
	}
	s.admin.Audit("accounts.delete", name, r)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    name,
		"message": "账号已从账号表删除，数据目录保留",
	})
}

func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request) {
	accounts := s.store.List()
	var total int64
	for _, a := range accounts {
		if size, err := usage.DirSize(a.Root); err == nil {
			total += size
		}
	}
	s.admin.Audit("overview", "-", r)
	writeJSON(w, http.StatusOK, map[string]any{
		"authMode":        s.admin.AuthMode(),
		"accounts":        len(accounts),
		"totalUsageBytes": total,
		"version":         Version,
	})
}

// accountErrorStatus 把账号层的「不存在」映射为 404，其余按 400 处理。
func accountErrorStatus(err error) int {
	if strings.Contains(err.Error(), "不存在") {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

// requestEndpoint 用请求 Host 推导 S3 Endpoint，供前端展示连接信息。
func requestEndpoint(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
	}
	return scheme + "://" + r.Host
}
