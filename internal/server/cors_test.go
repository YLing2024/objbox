package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YLing2024/objbox/internal/account"
	"github.com/YLing2024/objbox/internal/auth"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	corsA = "https://app.example.com"
	corsB = "https://other.example.com"
)

// preflight 发送一次浏览器预检请求。
func preflight(t *testing.T, ts *httptest.Server, path, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "PUT")
	req.Header.Set("Access-Control-Request-Headers", "authorization,x-amz-content-sha256,x-amz-date")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("预检请求失败: %v", err)
	}
	return resp
}

func hasCORSHeaders(h http.Header) bool {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "Access-Control-") {
			return true
		}
	}
	return false
}

// requireCORSHeaders 校验一次命中白名单的响应头是否齐全。
func requireCORSHeaders(t *testing.T, h http.Header, origin string) {
	t.Helper()
	if got := h.Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("Allow-Origin = %q，期望 %q", got, origin)
	}
	if got := h.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q，期望 true", got)
	}
	if got := h.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "PUT") {
		t.Errorf("Allow-Methods 应含 PUT: %q", got)
	}
	ah := h.Get("Access-Control-Allow-Headers")
	if !strings.Contains(ah, "Authorization") || !strings.Contains(ah, "x-amz-content-sha256") {
		t.Errorf("Allow-Headers 应含 Authorization 与 x-amz-content-sha256: %q", ah)
	}
	if got := h.Get("Access-Control-Expose-Headers"); !strings.Contains(got, "ETag") {
		t.Errorf("Expose-Headers 应含 ETag: %q", got)
	}
	if got := h.Get("Access-Control-Max-Age"); got != "3600" {
		t.Errorf("Max-Age = %q，期望 3600", got)
	}
	if got := h.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary 应含 Origin: %q", got)
	}
}

// enableCORS 登录管理面并写入跨域白名单。
func enableCORS(t *testing.T, ts *httptest.Server, dataDir string, origins ...string) *http.Client {
	t.Helper()
	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)
	resp, data := doJSON(t, client, http.MethodPut, ts.URL+"/api/admin/settings",
		map[string]any{"corsOrigins": origins})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存跨域白名单应 200，实际 %d（%s）", resp.StatusCode, data)
	}
	return client
}

// 需求测试 1：未配置白名单时预检不回任何 Access-Control-*。
func TestCORSDisabledNoHeaders(t *testing.T) {
	ts, _, _ := newAdminTestServer(t, "builtin")

	resp := preflight(t, ts, "/alice/k", corsA)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("未配置时预检应走正常认证（403），实际 %d", resp.StatusCode)
	}
	if hasCORSHeaders(resp.Header) {
		t.Fatalf("未配置时不应回 CORS 头: %v", resp.Header)
	}
}

// 需求测试 2：命中白名单预检 → 204 且头齐全，不要求任何签名/认证。
func TestCORSPreflightHit(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	enableCORS(t, ts, dataDir, corsA)

	resp := preflight(t, ts, "/alice/some-key", corsA)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("命中白名单预检应 204，实际 %d", resp.StatusCode)
	}
	requireCORSHeaders(t, resp.Header, corsA)
}

// 需求测试 3：未命中白名单 → 不回 CORS 头。
func TestCORSPreflightMiss(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	enableCORS(t, ts, dataDir, corsA)

	resp := preflight(t, ts, "/alice/some-key", corsB)
	resp.Body.Close()
	if hasCORSHeaders(resp.Header) {
		t.Fatalf("未命中来源不应回 CORS 头: %v", resp.Header)
	}
}

// 需求测试 4：真实请求（PUT/GET/HEAD/DELETE）业务码正常且 CORS 头齐全。
func TestCORSRealRequests(t *testing.T) {
	ts, store, dataDir := newAdminTestServer(t, "builtin")
	alice, err := store.Add("alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	enableCORS(t, ts, dataDir, corsA)
	hdrs := map[string]string{"Origin": corsA}

	put := signedDo(t, ts, http.MethodPut, "/alice/k.txt", []byte("hello"), alice, hdrs)
	put.Body.Close()
	if put.StatusCode != http.StatusOK {
		t.Fatalf("带来源的 PUT 应 200，实际 %d", put.StatusCode)
	}
	requireCORSHeaders(t, put.Header, corsA)

	get := signedDo(t, ts, http.MethodGet, "/alice/k.txt", nil, alice, hdrs)
	body := readBody(t, get)
	if get.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("带来源的 GET 应 200 且内容正确，实际 %d %q", get.StatusCode, body)
	}
	requireCORSHeaders(t, get.Header, corsA)
	if !strings.Contains(get.Header.Get("Access-Control-Expose-Headers"), "ETag") {
		t.Fatalf("GET 应暴露 ETag")
	}

	head := signedDo(t, ts, http.MethodHead, "/alice/k.txt", nil, alice, hdrs)
	head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("带来源的 HEAD 应 200，实际 %d", head.StatusCode)
	}
	requireCORSHeaders(t, head.Header, corsA)
	if head.Header.Get("ETag") == "" {
		t.Fatalf("HEAD 应带 ETag")
	}

	del := signedDo(t, ts, http.MethodDelete, "/alice/k.txt", nil, alice, hdrs)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("带来源的 DELETE 应 204，实际 %d", del.StatusCode)
	}
	requireCORSHeaders(t, del.Header, corsA)
}

// presignPutURL 生成 alice 对 /bucket/key 的预签名 PUT URL。
func presignPutURL(t *testing.T, ts *httptest.Server, acct *account.Account, bucket, key string) string {
	t.Helper()
	base := ts.URL + "/" + bucket + "/" + key
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("X-Amz-Expires", strconv.Itoa(3600))
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodPut, u.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: acct.AK, SecretAccessKey: acct.SK}
	signed, _, err := signer.PresignHTTP(context.Background(), creds, req, auth.UnsignedPayload, auth.Service, testRegion, time.Now().UTC())
	if err != nil {
		t.Fatalf("生成预签名 URL 失败: %v", err)
	}
	return signed
}

// 需求测试 4（续）：带 Origin 的预签名 PUT 返回对象并暴露 ETag。
func TestCORSPresignedPutExposesETag(t *testing.T) {
	ts, store, dataDir := newAdminTestServer(t, "builtin")
	alice, err := store.Add("alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	enableCORS(t, ts, dataDir, corsA)

	signed := presignPutURL(t, ts, alice, "alice", "presigned.txt")
	req, err := http.NewRequest(http.MethodPut, signed, strings.NewReader("presigned-body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", corsA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("预签名 PUT 失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预签名 PUT 应 200，实际 %d", resp.StatusCode)
	}
	requireCORSHeaders(t, resp.Header, corsA)
	if resp.Header.Get("ETag") == "" {
		t.Fatalf("预签名 PUT 响应应带 ETag")
	}

	// 对象确实落盘：再用已签名账号读回内容。
	get := signedDo(t, ts, http.MethodGet, "/alice/presigned.txt", nil, alice, map[string]string{"Origin": corsA})
	body := readBody(t, get)
	if get.StatusCode != http.StatusOK || string(body) != "presigned-body" {
		t.Fatalf("预签名 PUT 后应能读回对象，实际 %d %q", get.StatusCode, body)
	}
}

// 需求测试 5：运行时生效——配 A→204；改成 B 后不重启，A 无头、B 204。
func TestCORSRuntimeChange(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")
	client := enableCORS(t, ts, dataDir, corsA)

	if resp := preflight(t, ts, "/alice/k", corsA); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("首次命中 A 应 204，实际 %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 改成 B，不重启。
	resp, data := doJSON(t, client, http.MethodPut, ts.URL+"/api/admin/settings",
		map[string]any{"corsOrigins": []string{corsB}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改成 B 应 200，实际 %d（%s）", resp.StatusCode, data)
	}

	a := preflight(t, ts, "/alice/k", corsA)
	a.Body.Close()
	if hasCORSHeaders(a.Header) {
		t.Fatalf("改成 B 后 A 不应再有 CORS 头: %v", a.Header)
	}
	b := preflight(t, ts, "/alice/k", corsB)
	b.Body.Close()
	if b.StatusCode != http.StatusNoContent {
		t.Fatalf("改成 B 后 B 应 204，实际 %d", b.StatusCode)
	}
	requireCORSHeaders(t, b.Header, corsB)
}

// 需求测试 7：环境变量是初始默认值；设置文件写入后覆盖环境变量。
func TestCORSEnvDefaultThenOverride(t *testing.T) {
	t.Setenv("CORS_ORIGINS", "https://env.example.com, "+corsA)
	ts, _, dataDir := newAdminTestServer(t, "builtin")

	// 没有设置文件时，环境变量生效。
	envOrg := "https://env.example.com"
	resp := preflight(t, ts, "/alice/k", envOrg)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("环境变量默认值应命中 204，实际 %d", resp.StatusCode)
	}

	// 保存设置文件后覆盖环境变量。
	enableCORS(t, ts, dataDir, corsB)
	old := preflight(t, ts, "/alice/k", envOrg)
	old.Body.Close()
	if hasCORSHeaders(old.Header) {
		t.Fatalf("设置文件应有值后环境变量来源不应再命中: %v", old.Header)
	}
	now := preflight(t, ts, "/alice/k", corsB)
	now.Body.Close()
	if now.StatusCode != http.StatusNoContent {
		t.Fatalf("设置文件来源应命中 204，实际 %d", now.StatusCode)
	}
}

// 需求测试 6：管理接口读写、非法值 400（含第 N 行）、未认证 401、方法限制。
func TestAdminSettingsAPI(t *testing.T) {
	ts, _, dataDir := newAdminTestServer(t, "builtin")

	// 未认证 401。
	anon := newCookieClient(t)
	if resp, _ := doJSON(t, anon, http.MethodGet, ts.URL+"/api/admin/settings", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证 GET 应 401，实际 %d", resp.StatusCode)
	}
	if resp, _ := doJSON(t, anon, http.MethodPut, ts.URL+"/api/admin/settings",
		map[string]any{"corsOrigins": []string{corsA}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证 PUT 应 401，实际 %d", resp.StatusCode)
	}

	client := newCookieClient(t)
	loginBuiltin(t, ts, dataDir, client)

	resp, data := doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings 应 200，实际 %d（%s）", resp.StatusCode, data)
	}
	var got adminSettingsResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.CORSOrigins) != 0 || got.CORSEnabled {
		t.Fatalf("初始应为空且关闭: %+v", got)
	}

	// 合法写入。
	resp, data = doJSON(t, client, http.MethodPut, ts.URL+"/api/admin/settings",
		map[string]any{"corsOrigins": []string{corsA, "http://127.0.0.1:5173"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法 PUT 应 200，实际 %d（%s）", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.CORSOrigins) != 2 || !got.CORSEnabled || got.CORSSource != "settings" {
		t.Fatalf("PUT 响应错误: %+v", got)
	}

	// 读回一致。
	_, data = doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/settings", nil)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.CORSOrigins) != 2 {
		t.Fatalf("读回不一致: %+v", got)
	}

	// 第 2 行非法 → 400 且指明第 2 行。
	resp, data = doJSON(t, client, http.MethodPut, ts.URL+"/api/admin/settings",
		map[string]any{"corsOrigins": []string{corsA, "ftp://bad.example.com"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法来源应 400，实际 %d（%s）", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "第 2 行") {
		t.Fatalf("400 应指明第 2 行: %s", data)
	}

	// 缺字段 400。
	if resp, _ := doJSON(t, client, http.MethodPut, ts.URL+"/api/admin/settings",
		map[string]any{}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺字段应 400，实际 %d", resp.StatusCode)
	}

	// 方法限制。
	if resp, _ := doJSON(t, client, http.MethodPatch, ts.URL+"/api/admin/settings", map[string]any{}); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH 应 405，实际 %d", resp.StatusCode)
	}

	// 校验失败不得破坏已有值。
	_, data = doJSON(t, client, http.MethodGet, ts.URL+"/api/admin/settings", nil)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.CORSOrigins) != 2 {
		t.Fatalf("校验失败后已有值不应改变: %+v", got)
	}
}
