package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adminauth "github.com/YLing2024/objbox/internal/admin"
)

// M5 管理 API：默认创建账号并建同名桶，列表回传桶字段与桶是否存在。
func TestM5AdminCreateDefaultBucket(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]string{"name": "frank"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建应 200，实际 %d（%s）", resp.StatusCode, data)
	}
	var created struct {
		Bucket           string `json:"bucket"`
		AutoCreateBucket bool   `json:"autoCreateBucket"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if created.Bucket != "frank" || !created.AutoCreateBucket {
		t.Fatalf("创建响应桶字段错误: %+v", created)
	}
	if fi, err := os.Stat(filepath.Join(dataDir, "roots", "frank", "frank")); err != nil || !fi.IsDir() {
		t.Fatalf("默认桶目录应已创建: err=%v", err)
	}

	// 列表回传 bucket / autoCreateBucket / bucketExists。
	_, listData := doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts", nil)
	accounts := decodeAccounts(t, listData)
	var found *adminAccount
	for i := range accounts {
		if accounts[i].Name == "frank" {
			found = &accounts[i]
		}
	}
	if found == nil {
		t.Fatal("列表应包含 frank")
	}
	if found.Bucket != "frank" || !found.AutoCreateBucket || !found.BucketExists {
		t.Fatalf("列表桶字段错误: %+v", *found)
	}
}

// M5 管理 API：-no-bucket 等价物（autoCreateBucket=false）不建桶。
func TestM5AdminCreateNoBucket(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]any{"name": "grace", "autoCreateBucket": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建应 200，实际 %d（%s）", resp.StatusCode, data)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "roots", "grace", "grace")); !os.IsNotExist(err) {
		t.Fatalf("autoCreateBucket=false 不应建桶，stat err=%v", err)
	}

	_, listData := doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts", nil)
	for _, a := range decodeAccounts(t, listData) {
		if a.Name == "grace" && (a.AutoCreateBucket || a.BucketExists) {
			t.Fatalf("grace 桶字段错误: %+v", a)
		}
	}
}

// M5 管理 API：指定桶名建桶；桶已存在时幂等成功。
func TestM5AdminCreateCustomBucketIdempotent(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]any{"name": "heidi", "bucket": "team-bucket"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建应 200，实际 %d（%s）", resp.StatusCode, data)
	}
	bucketDir := filepath.Join(dataDir, "roots", "heidi", "team-bucket")
	if fi, err := os.Stat(bucketDir); err != nil || !fi.IsDir() {
		t.Fatalf("指定桶目录应已创建: err=%v", err)
	}

	// 删除账号表条目（保留 root 与桶），再用同名重建 → 建桶幂等。
	if resp, data := doJSON(t, client, http.MethodDelete, ts.URL+"/api/admin/accounts/heidi", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("删除应 200: %d %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]any{"name": "heidi", "bucket": "team-bucket"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("桶已存在时重建账号应幂等成功，实际 %d（%s）", resp.StatusCode, data)
	}
}

// M5 管理 API：非法桶名 → 400，且不创建账号。
func TestM5AdminCreateInvalidBucket(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]any{"name": "ivan", "bucket": "Bad_Bucket"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法桶名应 400，实际 %d（%s）", resp.StatusCode, data)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "roots", "ivan")); !os.IsNotExist(err) {
		t.Fatalf("非法桶名不应创建账号 root，stat err=%v", err)
	}
}

// M5：accounts.create 审计记录桶名。
func TestM5AdminAuditRecordsBucket(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	if resp, data := doJSON(t, client, http.MethodPost, ts.URL+"/api/admin/accounts",
		map[string]any{"name": "judy", "bucket": "judy-bucket"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("创建失败: %d %s", resp.StatusCode, data)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, adminauth.AuditFileName))
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if !strings.Contains(content, "op=accounts.create") || !strings.Contains(content, "detail=bucket=judy-bucket") {
		t.Fatalf("审计应记录桶名：\n%s", content)
	}
	// 审计不得含明文 SK。
	_, listData := doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/accounts?reveal=judy", nil)
	var revealed struct {
		Accounts []struct {
			SK string `json:"sk"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(listData, &revealed); err == nil && len(revealed.Accounts) > 0 {
		if strings.Contains(content, revealed.Accounts[0].SK) {
			t.Fatalf("审计日志不得含明文 SK")
		}
	}
}
