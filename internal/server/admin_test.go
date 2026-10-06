package server

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YLing2024/objbox/internal/account"
	adminauth "github.com/YLing2024/objbox/internal/admin"
)

// newAdminTestServer 起一个管理面测试服务，AUTH_MODE 显式指定。
func newAdminTestServer(t *testing.T, mode string) (ts *httptest.Server, store *account.Store, dataDir string) {
	t.Helper()
	t.Setenv("AUTH_MODE", mode)
	dataDir = t.TempDir()
	store, err := account.Load(dataDir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	srv, err := New(store)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ts = httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
	})
	return ts, store, dataDir
}

func newCookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func readAdminPassword(t *testing.T, dataDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, "admin-password.txt"))
	if err != nil {
		t.Fatalf("读取管理员口令失败: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

func doJSON(t *testing.T, client *http.Client, method, url string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, url, err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func loginBuiltin(t *testing.T, ts *httptest.Server, dataDir string, client *http.Client) {
	t.Helper()
	resp, _ := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/login",
		map[string]string{"password": readAdminPassword(t, dataDir)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录应 200，实际 %d", resp.StatusCode)
	}
}

func decodeAccounts(t *testing.T, data []byte) []adminAccount {
	t.Helper()
	var out struct {
		Accounts []adminAccount `json:"accounts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("解析账号列表失败: %v（%s）", err, data)
	}
	return out.Accounts
}

// 4.2.1 builtin：无 cookie → 401；错口令 → 401；正确口令 → cookie 后可访问。
func TestAdminBuiltinAuthFlow(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	anon := &http.Client{}

	resp, _ := doJSON(t, anon, http.MethodGet, ts.URL+"/api/admin/accounts", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 cookie 访问应 401，实际 %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, anon, http.MethodGet, ts.URL+"/api/admin/overview", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 cookie overview 应 401，实际 %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, anon, http.MethodGet, ts.URL+"/api/admin/accounts?reveal=alice", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 cookie reveal 应 401，实际 %d", resp.StatusCode)
	}

	client := newCookieClient(t)
	resp, _ = doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/login",
		map[string]string{"password": "wrong-password"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错口令应 401，实际 %d", resp.StatusCode)
	}

	loginBuiltin(t, ts, dataDir, client)
	resp, data := doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带 cookie 访问应 200，实际 %d", resp.StatusCode)
	}
	_ = data

	resp, data = doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/overview", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("overview 应 200，实际 %d", resp.StatusCode)
	}
	var ov map[string]any
	if err := json.Unmarshal(data, &ov); err != nil {
		t.Fatal(err)
	}
	if ov["authMode"] != "builtin" {
		t.Fatalf("overview authMode = %v，期望 builtin", ov["authMode"])
	}

	// 退出后失效。
	if resp, _ := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/logout", map[string]string{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("退出应 200，实际 %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("退出后应 401，实际 %d", resp.StatusCode)
	}
}

// 4.2.1 登录限速：同一 IP 第 11 次失败 → 429。
func TestAdminLoginRateLimit(t *testing.T) {
	ts, _, _ := newAdminTestServer(t, "builtin")
	client := &http.Client{}

	for i := 1; i <= 10; i++ {
		resp, _ := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/login",
			map[string]string{"password": "nope"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401，实际 %d", i, resp.StatusCode)
		}
	}
	resp, _ := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/login",
		map[string]string{"password": "nope"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("第 11 次失败应 429，实际 %d", resp.StatusCode)
	}
}

// 4.2.2 sso：无 X-Auth-User → 401；带 → 200；口令登录不可用；S3 端点不受影响。
func TestAdminSSOMode(t *testing.T) {
	ts, store, dataDir := newAdminTestServer(t, "sso")
	client := &http.Client{}

	resp, _ := doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sso 无头应 401，实际 %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/admin/accounts", nil)
	req.Header.Set(adminauth.HeaderUser, "tester")
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("sso 带头应 200，实际 %d", resp2.StatusCode)
	}

	// 即使口令正确也不能走 builtin 登录（sso 模式一律 401）。
	resp, _ = doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/login",
		map[string]string{"password": "anything"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sso 模式登录应 401，实际 %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "admin-password.txt")); !os.IsNotExist(err) {
		t.Fatalf("sso 模式不应生成自带口令文件，stat err=%v", err)
	}

	// 铁律：S3 端点始终走 AK/SK，与 AUTH_MODE 无关。
	alice, err := store.Add("alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	s3resp := signedDo(t, ts, http.MethodGet, "/", nil, alice, nil)
	s3resp.Body.Close()
	if s3resp.StatusCode != http.StatusOK {
		t.Fatalf("sso 模式下 S3 ListBuckets 应 200，实际 %d", s3resp.StatusCode)
	}
}

// 4.2.3 一次性密钥：create/rotate 响应含 SK，列表默认掩码，reveal 才明文。
func TestAdminOneTimeSecretAndReveal(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]string{"name": "carol", "note": "测试"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建应 200，实际 %d（%s）", resp.StatusCode, data)
	}
	var created struct {
		Name      string `json:"name"`
		AK        string `json:"ak"`
		SK        string `json:"sk"`
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		PathStyle bool   `json:"pathStyle"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if created.AK == "" || created.SK == "" {
		t.Fatalf("创建响应应含 AK/SK：%s", data)
	}
	if created.Endpoint == "" || created.Region != "us-east-1" || !created.PathStyle {
		t.Fatalf("连接信息不完整：%+v", created)
	}

	// 默认掩码
	_, data = doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts", nil)
	accounts := decodeAccounts(t, data)
	var carol adminAccount
	found := false
	for _, a := range accounts {
		if a.Name == "carol" {
			carol, found = a, true
		}
	}
	if !found {
		t.Fatalf("列表缺少 carol：%s", data)
	}
	if carol.SK == created.SK || !strings.Contains(carol.SK, "****") {
		t.Fatalf("列表 SK 应掩码，实际 %q", carol.SK)
	}

	// reveal 出明文
	_, data = doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts?reveal=carol", nil)
	accounts = decodeAccounts(t, data)
	for _, a := range accounts {
		if a.Name == "carol" && a.SK != created.SK {
			t.Fatalf("reveal 明文 SK 不符：%q vs %q", a.SK, created.SK)
		}
	}

	// 轮换后返回新 SK，旧 SK 失效
	resp, data = doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts/carol/rotate", map[string]string{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("轮换应 200，实际 %d", resp.StatusCode)
	}
	var rotated struct {
		SK string `json:"sk"`
	}
	if err := json.Unmarshal(data, &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.SK == "" || rotated.SK == created.SK {
		t.Fatalf("轮换应返回不同的新 SK：%q", rotated.SK)
	}
	_, data = doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts?reveal=carol", nil)
	for _, a := range decodeAccounts(t, data) {
		if a.Name == "carol" && a.SK != rotated.SK {
			t.Fatalf("reveal 应返回新 SK：%q vs %q", a.SK, rotated.SK)
		}
	}
}

// 4.2.4 账号 CRUD 与 CLI 结果一致：管理 API 改动，独立加载的 store 立即看到。
func TestAdminCRUDMatchesCLI(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	if resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]string{"name": "dave", "note": "初始"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("创建 dave 失败: %d %s", resp.StatusCode, data)
	}

	reload := func() *account.Store {
		s, err := account.Load(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if a, ok := reload().Find("dave"); !ok || a.Note != "初始" {
		t.Fatalf("CLI 视角看不到 API 新建的账号：%+v", a)
	}

	quota := int64(2048)
	note := "改过"
	if resp, data := doJSON(t, client, http.MethodPatch, ts.URL+"/api/admin/accounts/dave",
		map[string]any{"note": note, "quotaBytes": quota}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH 失败: %d %s", resp.StatusCode, data)
	}
	if a, ok := reload().Find("dave"); !ok || a.Note != note || a.QuotaBytes != quota {
		t.Fatalf("PATCH 未与 CLI 共享：%+v", a)
	}

	if resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts/dave/disable", map[string]string{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable 失败: %d %s", resp.StatusCode, data)
	}
	if a, ok := reload().Find("dave"); !ok || !a.Disabled {
		t.Fatalf("disable 未与 CLI 共享：%+v", a)
	}

	if resp, data := doJSON(t, client, http.MethodDelete, ts.URL+"/api/admin/accounts/dave", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("删除失败: %d %s", resp.StatusCode, data)
	}
	if _, ok := reload().Find("dave"); ok {
		t.Fatal("删除后 CLI 视角仍能看到 dave")
	}
}

// 4.2.5 审计日志：3 次操作 3 条记录，且全文不含明文 SK。
func TestAdminAuditLog(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client) // 1

	_, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]string{"name": "erin"}) // 2
	var created struct {
		SK string `json:"sk"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	_, data = doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts/erin/rotate", map[string]string{}) // 3
	var rotated struct {
		SK string `json:"sk"`
	}
	if err := json.Unmarshal(data, &rotated); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dataDir, adminauth.AuditFileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("审计日志不存在: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("审计日志权限 = %o，期望 600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) != 3 {
		t.Fatalf("应有 3 条审计记录，实际 %d 条：\n%s", len(lines), content)
	}
	if !strings.Contains(content, "op=") || !strings.Contains(content, "ip=") {
		t.Fatalf("审计记录字段缺失：\n%s", content)
	}
	if strings.Contains(content, created.SK) || strings.Contains(content, rotated.SK) {
		t.Fatalf("审计日志绝不能含明文 SK：\n%s", content)
	}
}

// 4.2.6 静态资源：/ 返回管理页 HTML，/assets/* 可取到 JS/CSS；匿名无 Accept 仍 403。
func TestAdminStaticResources(t *testing.T) {
	ts, _, _ := newAdminTestServer(t, "builtin")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ 应 200，实际 %d", resp.StatusCode)
	}
	html := string(body)
	if !strings.Contains(html, "<html") || !strings.Contains(html, "objbox") {
		t.Fatalf("/ 应返回管理页 HTML，实际：%s", html)
	}
	if strings.Contains(html, "__OBJBOX_AUTH_MODE__") {
		t.Fatalf("AUTH_MODE 占位符未被替换：%s", html)
	}
	if !strings.Contains(html, "builtin") {
		t.Fatalf("页面应注入 builtin 模式：%s", html)
	}

	// 找一个真实资源并取回。
	entries, err := fs.ReadDir(adminAssets, "assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("内嵌 assets 缺失: %v", err)
	}
	name := entries[0].Name()
	aresp, err := http.Get(ts.URL + "/assets/" + name)
	if err != nil {
		t.Fatal(err)
	}
	abody, _ := io.ReadAll(aresp.Body)
	aresp.Body.Close()
	if aresp.StatusCode != http.StatusOK || len(abody) == 0 {
		t.Fatalf("/assets/%s 应 200 且有内容，实际 %d", name, aresp.StatusCode)
	}
	if aresp.Header.Get("Content-Type") == "" {
		t.Fatalf("/assets/%s 缺少 Content-Type", name)
	}

	// 不存在的资源 404。
	miss, err := http.Get(ts.URL + "/assets/not-exist.js")
	if err != nil {
		t.Fatal(err)
	}
	miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("缺失资源应 404，实际 %d", miss.StatusCode)
	}

	// Go 默认客户端不带 Accept：匿名 GET / 仍按 S3 处理 → 403（M0 语义不回退）。
	anon, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != http.StatusForbidden {
		t.Fatalf("无 Accept 的匿名 GET / 应 403，实际 %d", anon.StatusCode)
	}
}
