package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/YLing2024/objbox/internal/account"
	adminauth "github.com/YLing2024/objbox/internal/admin"
	"github.com/YLing2024/objbox/internal/backend"
	"github.com/YLing2024/objbox/internal/usage"
)

// Version 为二进制版本号，出现在管理面 overview 中。
const Version = "0.3.0"

// adminAccount 是管理面账号列表项；SK 默认掩码，仅在 reveal 时返回明文。
type adminAccount struct {
	Name             string `json:"name"`
	AK               string `json:"ak"`
	SK               string `json:"sk"`
	Root             string `json:"root"`
	UsageBytes       int64  `json:"usageBytes"`
	Status           string `json:"status"`
	Note             string `json:"note"`
	QuotaBytes       int64  `json:"quotaBytes"`
	Readonly         bool   `json:"readonly"`
	Disabled         bool   `json:"disabled"`
	Bucket           string `json:"bucket"`
	AutoCreateBucket bool   `json:"autoCreateBucket"`
	BucketExists     bool   `json:"bucketExists"`
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
		Name:             a.Name,
		AK:               a.AK,
		SK:               sk,
		Root:             a.Root,
		UsageBytes:       size,
		Status:           accountStatus(a),
		Note:             a.Note,
		QuotaBytes:       a.QuotaBytes,
		Readonly:         a.Readonly,
		Disabled:         a.Disabled,
		Bucket:           a.Bucket,
		AutoCreateBucket: a.AutoCreateBucket,
		BucketExists:     backend.BucketExistsInRoot(a.Root, a.Bucket),
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
	case rest == "settings":
		s.adminSettings(w, r)
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
	s.admin.SetSessionCookie(w, r, "admin")
	s.admin.Audit("login", "admin", r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": "admin"})
}

func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request, user string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	// 提升会话版本号：登出后旧 cookie 即使未过期也立即失效。
	if err := s.admin.InvalidateSessions(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "退出失败")
		return
	}
	s.admin.ClearSessionCookie(w, r)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":  out,
		"endpoint":  requestEndpoint(r),
		"region":    "us-east-1",
		"pathStyle": true,
	})
}

func (s *Server) adminCreateAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name             string `json:"name"`
		Note             string `json:"note"`
		Readonly         bool   `json:"readonly"`
		QuotaBytes       *int64 `json:"quotaBytes"`
		Bucket           string `json:"bucket"`
		AutoCreateBucket *bool  `json:"autoCreateBucket"`
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

	bucket := strings.TrimSpace(body.Bucket)
	// 只有显式指定桶名才校验并报错；未指定时默认桶名非法会自动回退，
	// 绝不因此让建号失败（M6 §7）。
	if bucket != "" {
		if err := backend.ValidateBucket(bucket); err != nil {
			writeJSONError(w, http.StatusBadRequest, "桶名不合法："+err.Error())
			return
		}
	}
	wantAuto := body.AutoCreateBucket == nil || *body.AutoCreateBucket
	resolved, autoCreate, bucketNote := backend.DefaultBucketFor(name, bucket, wantAuto)

	acct, err := s.store.AddAccount(name, body.Note, body.Readonly, account.AddOptions{
		Bucket:           resolved,
		AutoCreateBucket: &autoCreate,
	})
	if err != nil {
		writeJSONError(w, accountErrorStatus(err), err.Error())
		return
	}
	// 建账号即建桶：失败则回滚账号创建，保证「建完就能用」。
	if acct.AutoCreateBucket {
		if err := backend.EnsureBucketAt(acct.Root, acct.Bucket); err != nil {
			_ = s.store.Remove(name)
			writeJSONError(w, http.StatusInternalServerError, "创建默认桶失败: "+err.Error())
			return
		}
	}
	if body.QuotaBytes != nil {
		if err := s.store.SetQuota(name, *body.QuotaBytes); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		acct.QuotaBytes = *body.QuotaBytes
	}
	s.admin.AuditDetail("accounts.create", name, "bucket="+acct.Bucket, r)
	resp := map[string]any{
		"name":             acct.Name,
		"ak":               acct.AK,
		"sk":               acct.SK,
		"root":             acct.Root,
		"note":             acct.Note,
		"readonly":         acct.Readonly,
		"bucket":           acct.Bucket,
		"autoCreateBucket": acct.AutoCreateBucket,
		"endpoint":         requestEndpoint(r),
		"region":           "us-east-1",
		"pathStyle":        true,
	}
	if bucketNote != "" {
		resp["bucketNote"] = bucketNote
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminAccountItem 处理 /api/admin/accounts/<name>[/<sub>...]。
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

	switch segs[1] {
	case "rotate", "disable", "enable":
		if len(segs) != 2 || r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "不支持的操作")
			return
		}
		s.adminAccountAction(w, r, name, segs[1])
	case "bucket":
		// 改账号的默认桶名与自动建桶开关。
		if len(segs) == 2 && r.Method == http.MethodPatch {
			s.adminUpdateBucket(w, r, name)
			return
		}
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 PATCH")
	case "buckets":
		if len(segs) == 2 {
			switch r.Method {
			case http.MethodGet:
				s.adminListBuckets(w, r, name)
			case http.MethodPost:
				s.adminCreateBucket(w, r, name)
			default:
				writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET 或 POST")
			}
			return
		}
		if len(segs) == 3 {
			if r.Method == http.MethodDelete {
				s.adminDeleteBucket(w, r, name, segs[2])
				return
			}
			writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 DELETE")
			return
		}
		writeJSONError(w, http.StatusNotFound, "接口不存在")
	default:
		writeJSONError(w, http.StatusNotFound, "接口不存在")
	}
}

// adminAccountAction 处理 rotate / disable / enable。
func (s *Server) adminAccountAction(w http.ResponseWriter, r *http.Request, name, action string) {
	switch action {
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

// adminBucket 是管理面桶列表项；不含 SK。
type adminBucket struct {
	Name      string `json:"name"`
	Objects   int    `json:"objects"`
	Bytes     int64  `json:"bytes"`
	IsDefault bool   `json:"isDefault"`
}

// adminListBuckets 返回该账号当前桶数组（含对象数 / 占用字节数 / 是否默认桶）。
// 只读，不打开账号元数据库，也不返回任何 SK。
func (s *Server) adminListBuckets(w http.ResponseWriter, r *http.Request, name string) {
	acct, ok := s.store.Find(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "账号不存在")
		return
	}
	names, err := backend.ListBucketNames(acct.Root)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "读取桶列表失败: "+err.Error())
		return
	}
	out := make([]adminBucket, 0, len(names))
	for _, b := range names {
		objects, bytes, err := backend.BucketStats(acct.Root, b)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "统计桶用量失败: "+err.Error())
			return
		}
		out = append(out, adminBucket{Name: b, Objects: objects, Bytes: bytes, IsDefault: b == acct.Bucket})
	}
	s.admin.Audit("accounts.buckets.list", name, r)
	writeJSON(w, http.StatusOK, map[string]any{"buckets": out})
}

// adminCreateBucket 建桶：桶名走 ValidateBucket（不放宽），已存在 409，成功 201。
func (s *Server) adminCreateBucket(w http.ResponseWriter, r *http.Request, name string) {
	acct, ok := s.store.Find(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "账号不存在")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体非法")
		return
	}
	bucket := strings.TrimSpace(body.Name)
	if err := backend.ValidateBucket(bucket); err != nil {
		writeJSONError(w, http.StatusBadRequest, "桶名不合法："+err.Error())
		return
	}
	if err := backend.CreateBucketAt(acct.Root, bucket); err != nil {
		if errors.Is(err, backend.ErrBucketExists) {
			writeJSONError(w, http.StatusConflict, "桶已存在")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "创建桶失败: "+err.Error())
		return
	}
	s.admin.AuditDetail("accounts.bucket.create", name, "bucket="+bucket, r)
	writeJSON(w, http.StatusCreated, map[string]any{"name": bucket})
}

// adminDeleteBucket 删空桶：不存在 404，非空 409；删默认桶也允许，且不自动重建。
func (s *Server) adminDeleteBucket(w http.ResponseWriter, r *http.Request, name, bucket string) {
	acct, ok := s.store.Find(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if err := backend.ValidateBucket(bucket); err != nil {
		writeJSONError(w, http.StatusBadRequest, "桶名不合法："+err.Error())
		return
	}
	switch err := backend.DeleteBucketAt(acct.Root, bucket); {
	case errors.Is(err, backend.ErrBucketNotFound):
		writeJSONError(w, http.StatusNotFound, "桶不存在")
		return
	case errors.Is(err, backend.ErrBucketNotEmpty):
		writeJSONError(w, http.StatusConflict, "桶内还有对象，请先清空桶内对象再删除")
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "删除桶失败: "+err.Error())
		return
	}
	s.admin.AuditDetail("accounts.bucket.delete", name, "bucket="+bucket, r)
	writeJSON(w, http.StatusOK, map[string]any{"name": bucket, "message": "桶已删除"})
}

// adminUpdateBucket 改账号默认桶名与自动建桶开关；桶名非法 400，一次原子落盘。
func (s *Server) adminUpdateBucket(w http.ResponseWriter, r *http.Request, name string) {
	var body struct {
		Bucket           *string `json:"bucket"`
		AutoCreateBucket *bool   `json:"autoCreateBucket"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体非法")
		return
	}
	if body.Bucket == nil && body.AutoCreateBucket == nil {
		writeJSONError(w, http.StatusBadRequest, "无可更新字段")
		return
	}
	if body.Bucket != nil {
		b := strings.TrimSpace(*body.Bucket)
		if err := backend.ValidateBucket(b); err != nil {
			writeJSONError(w, http.StatusBadRequest, "桶名不合法："+err.Error())
			return
		}
		body.Bucket = &b
	}
	acct, err := s.store.UpdateBucket(name, body.Bucket, body.AutoCreateBucket)
	if err != nil {
		writeJSONError(w, accountErrorStatus(err), err.Error())
		return
	}
	s.admin.AuditDetail("accounts.bucket.update", name, "bucket="+acct.Bucket, r)
	writeJSON(w, http.StatusOK, newAdminAccount(acct, account.MaskSecret(acct.SK)))
}

// adminSettings 读写服务端级设置（当前仅跨域白名单），复用管理面鉴权。
func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.readSettings(w, r)
	case http.MethodPut:
		s.updateSettings(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET 或 PUT")
	}
}

// adminSettingsResponse 是设置读写接口的响应体。
type adminSettingsResponse struct {
	CORSOrigins []string `json:"corsOrigins"`
	CORSEnabled bool     `json:"corsEnabled"`
	CORSSource  string   `json:"corsSource"`
}

func (s *Server) settingsResponse() adminSettingsResponse {
	origins := s.settings.Origins()
	if origins == nil {
		origins = []string{}
	}
	return adminSettingsResponse{
		CORSOrigins: origins,
		CORSEnabled: len(origins) > 0,
		CORSSource:  string(s.settings.Source()),
	}
}

func (s *Server) readSettings(w http.ResponseWriter, r *http.Request) {
	s.admin.Audit("settings.get", "-", r)
	writeJSON(w, http.StatusOK, s.settingsResponse())
}

// updateSettings 校验后原子落盘并即时生效；校验失败 400 并指明第 N 行非法。
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CORSOrigins *[]string `json:"corsOrigins"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体非法")
		return
	}
	if body.CORSOrigins == nil {
		writeJSONError(w, http.StatusBadRequest, "缺少 corsOrigins 字段")
		return
	}
	if err := s.settings.SetCORSOrigins(*body.CORSOrigins); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.admin.Audit("settings.update", "-", r)
	writeJSON(w, http.StatusOK, s.settingsResponse())
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

// requestEndpoint 运行时推导对外 S3 Endpoint，供前端展示与一键复制接入信息。
//
// 优先级：环境变量 OBJBOX_PUBLIC_ENDPOINT（若设置）> X-Forwarded-Proto + X-Forwarded-Host
// （或 Host）。本服务通常跑在反向代理之后，代理应透传这两个头；仓库内不写死任何真实域名。
func requestEndpoint(r *http.Request) string {
	if pub := strings.TrimSpace(os.Getenv("OBJBOX_PUBLIC_ENDPOINT")); pub != "" {
		return strings.TrimRight(pub, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
	}
	host := strings.TrimSpace(strings.Split(r.Host, ",")[0])
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host
}
