package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YLing2024/objbox/internal/account"
	adminauth "github.com/YLing2024/objbox/internal/admin"
)

// decodeBuckets 解析 GET buckets 响应。
func decodeBuckets(t *testing.T, data []byte) []adminBucket {
	t.Helper()
	var out struct {
		Buckets []adminBucket `json:"buckets"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("解析桶列表失败: %v（%s）", err, data)
	}
	return out.Buckets
}

func findBucket(list []adminBucket, name string) (adminBucket, bool) {
	for _, b := range list {
		if b.Name == name {
			return b, true
		}
	}
	return adminBucket{}, false
}

// M6 §7：管理面建短账号名应回退到 <name>-bucket，而不是直接失败。
func TestM6AdminCreateShortNameFallsBack(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]string{"name": "ab"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("短账号名应 200，实际 %d（%s）", resp.StatusCode, data)
	}
	var created struct {
		Bucket           string `json:"bucket"`
		AutoCreateBucket bool   `json:"autoCreateBucket"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if created.Bucket != "ab-bucket" || !created.AutoCreateBucket {
		t.Fatalf("应回退默认桶 ab-bucket：%+v", created)
	}
	if fi, err := os.Stat(filepath.Join(dataDir, "roots", "ab", "ab-bucket")); err != nil || !fi.IsDir() {
		t.Fatalf("回退桶目录应已创建: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "roots", "ab", "ab")); !os.IsNotExist(err) {
		t.Fatalf("非法默认桶 ab 不应存在: err=%v", err)
	}
}

// M6 §7：显式指定非法桶名仍必须 400。
func TestM6AdminCreateExplicitInvalidBucket(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]any{"name": "cd", "bucket": "xx"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("显式非法桶名应 400，实际 %d（%s）", resp.StatusCode, data)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "roots", "cd")); !os.IsNotExist(err) {
		t.Fatalf("非法桶名不应创建账号 root: err=%v", err)
	}
}

// M6：账号列表响应带运行时推导的 Endpoint（优先 X-Forwarded-*），且不含真实域名。
func TestM6AdminListEndpointDerivation(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/admin/accounts", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "s3.example.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		PathStyle bool   `json:"pathStyle"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Endpoint != "https://s3.example.com" || out.Region != "us-east-1" || !out.PathStyle {
		t.Fatalf("连接信息推导错误: %+v", out)
	}

	// OBJBOX_PUBLIC_ENDPOINT 优先于转发头。
	t.Setenv("OBJBOX_PUBLIC_ENDPOINT", "https://public.example.com/")
	resp, data := doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("列表应 200: %d %s", resp.StatusCode, data)
	}
	var out2 struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(data, &out2); err != nil {
		t.Fatal(err)
	}
	if out2.Endpoint != "https://public.example.com" {
		t.Fatalf("OBJBOX_PUBLIC_ENDPOINT 应优先，实际 %q", out2.Endpoint)
	}
}

// M7 §1：GET buckets 返回桶数组、对象数 / 占用体积与 isDefault 标记。
func TestM7AdminListBuckets(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	if resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]string{"name": "bob"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("建号失败: %d %s", resp.StatusCode, data)
	}
	// 直接在默认桶里放一个对象文件（管理面统计走磁盘）。
	if err := os.WriteFile(filepath.Join(dataDir, "roots", "bob", "bob", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 再建一个非默认桶。
	if resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts/bob/buckets",
		map[string]string{"name": "bob-extra"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("建桶应 201: %d %s", resp.StatusCode, data)
	}

	resp, data := doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts/bob/buckets", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET buckets 应 200: %d %s", resp.StatusCode, data)
	}
	list := decodeBuckets(t, data)
	if len(list) != 2 {
		t.Fatalf("应返回 2 个桶，实际 %d（%s）", len(list), data)
	}
	def, ok := findBucket(list, "bob")
	if !ok || !def.IsDefault || def.Objects != 1 || def.Bytes != 5 {
		t.Fatalf("默认桶统计错误: %+v", def)
	}
	extra, ok := findBucket(list, "bob-extra")
	if !ok || extra.IsDefault || extra.Objects != 0 || extra.Bytes != 0 {
		t.Fatalf("非默认桶统计错误: %+v", extra)
	}
}

// M7 §1：POST 建桶成功 201 / 重名 409 / 非法名 400 / 未认证 401 / 未知账号 404。
func TestM7AdminCreateBucketStatuses(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	anon := newCookieClient(t)

	// 未认证。
	if resp, _ := doJSON(t, anon, http.MethodPost, ts.URL+"/api/admin/accounts/nobody/buckets",
		map[string]string{"name": "whatever"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，实际 %d", resp.StatusCode)
	}

	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)
	doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts", map[string]string{"name": "carol"})

	if resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts/carol/buckets",
		map[string]string{"name": "carol-new"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("建桶应 201，实际 %d（%s）", resp.StatusCode, data)
	}
	if resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts/carol/buckets",
		map[string]string{"name": "carol-new"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("重名应 409，实际 %d（%s）", resp.StatusCode, data)
	} else if !strings.Contains(string(data), "已存在") {
		t.Fatalf("409 应给出明确中文错误：%s", data)
	}
	if resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts/carol/buckets",
		map[string]string{"name": "Bad_Bucket"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法桶名应 400，实际 %d（%s）", resp.StatusCode, data)
	}
	if resp, _ := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts/nobody/buckets",
		map[string]string{"name": "carol-new"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知账号应 404，实际 %d", resp.StatusCode)
	}
}

// M7 §1：DELETE 空桶成功 / 非空 409 / 不存在 404；删默认桶允许且不自动重建。
func TestM7AdminDeleteBucketStatuses(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts", map[string]string{"name": "dave"})

	// 空桶删除成功。
	if resp, data := doJSON(t, client, http.MethodDelete, ts.URL+"/api/admin/accounts/dave/buckets/dave", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("删空桶应 200，实际 %d（%s）", resp.StatusCode, data)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "roots", "dave", "dave")); !os.IsNotExist(err) {
		t.Fatalf("默认桶应已删除，stat err=%v", err)
	}
	// 删默认桶不得被自动重建。
	if _, err := os.Stat(filepath.Join(dataDir, "roots", "dave", "dave")); !os.IsNotExist(err) {
		t.Fatal("删除默认桶后不得自动重建")
	}

	// 不存在 404。
	if resp, _ := doJSON(t, client, http.MethodDelete, ts.URL+"/api/admin/accounts/dave/buckets/dave", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("删不存在桶应 404，实际 %d", resp.StatusCode)
	}

	// 非空 409。
	doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts/dave/buckets", map[string]string{"name": "dave-full"})
	if err := os.WriteFile(filepath.Join(dataDir, "roots", "dave", "dave-full", "x.bin"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if resp, data := doJSON(t, client, http.MethodDelete, ts.URL+"/api/admin/accounts/dave/buckets/dave-full", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("删非空桶应 409，实际 %d（%s）", resp.StatusCode, data)
	} else if !strings.Contains(string(data), "对象") {
		t.Fatalf("409 应提示桶内还有对象：%s", data)
	}
}

// M7 §1：PATCH 改默认桶 / 开关后原子落盘，独立重载可读回新值（热重载语义）。
func TestM7AdminUpdateBucketPersists(t *testing.T) {
	ts, store, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts", map[string]string{"name": "erin"})

	resp, data := doJSON(t, client, http.MethodPatch, ts.URL+"/api/admin/accounts/erin/bucket",
		map[string]any{"bucket": "erin-bucket", "autoCreateBucket": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH 应 200，实际 %d（%s）", resp.StatusCode, data)
	}

	// 独立重载（等价 CLI/另一进程视角）。
	reloaded, err := account.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := reloaded.Find("erin")
	if !ok || a.Bucket != "erin-bucket" || a.AutoCreateBucket {
		t.Fatalf("落盘未生效: %+v ok=%v", a, ok)
	}
	// 本进程内存表也已更新（热重载不覆盖回旧值）。
	if a2, ok := store.Find("erin"); !ok || a2.Bucket != "erin-bucket" || a2.AutoCreateBucket {
		t.Fatalf("内存账号表未更新: %+v", a2)
	}

	// 非法桶名 400。
	if resp, _ := doJSON(t, client, http.MethodPatch, ts.URL+"/api/admin/accounts/erin/bucket",
		map[string]any{"bucket": "Bad_Bucket"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法桶名应 400，实际 %d", resp.StatusCode)
	}
	// 未知账号 404。
	if resp, _ := doJSON(t, client, http.MethodPatch, ts.URL+"/api/admin/accounts/nobody/bucket",
		map[string]any{"autoCreateBucket": false}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知账号应 404，实际 %d", resp.StatusCode)
	}
	// 空 body 400。
	if resp, _ := doJSON(t, client, http.MethodPatch, ts.URL+"/api/admin/accounts/erin/bucket",
		map[string]any{}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("无字段应 400，实际 %d", resp.StatusCode)
	}
}

// M7 §4：桶管理接口任何响应都不得包含 SK 明文；审计日志同样不得含 SK。
// （建号 / rotate / reveal 属既有的一次性密钥接口，按设计返回 SK，不在此断言范围内。）
func TestM7AdminNeverLeaksSecret(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	_, created := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts", map[string]string{"name": "frank"})
	var cred struct {
		SK string `json:"sk"`
	}
	if err := json.Unmarshal(created, &cred); err != nil || cred.SK == "" {
		t.Fatalf("解析建号响应失败: %v（%s）", err, created)
	}

	bodies := [][]byte{}
	add := func(resp *http.Response, data []byte) {
		if resp.StatusCode >= 500 {
			t.Fatalf("意外 5xx: %d %s", resp.StatusCode, data)
		}
		bodies = append(bodies, data)
	}
	r, d := doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts/frank/buckets", nil)
	add(r, d)
	r, d = doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts/frank/buckets", map[string]string{"name": "frank-x"})
	add(r, d)
	r, d = doJSON(t, client, http.MethodPatch, ts.URL+"/api/admin/accounts/frank/bucket", map[string]any{"autoCreateBucket": false})
	add(r, d)
	r, d = doJSON(t, client, http.MethodDelete, ts.URL+"/api/admin/accounts/frank/buckets/frank-x", nil)
	add(r, d)

	for i, b := range bodies {
		if strings.Contains(string(b), cred.SK) {
			t.Fatalf("响应 #%d 泄露了明文 SK：%s", i, b)
		}
	}

	raw, err := os.ReadFile(filepath.Join(dataDir, adminauth.AuditFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cred.SK) {
		t.Fatalf("审计日志泄露了明文 SK:\n%s", raw)
	}
	for _, op := range []string{"accounts.bucket.create", "accounts.bucket.delete", "accounts.bucket.update"} {
		if !strings.Contains(string(raw), op) {
			t.Fatalf("审计应含 %s:\n%s", op, raw)
		}
	}
}
