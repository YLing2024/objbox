package server

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YLing2024/objbox/internal/account"
	"github.com/YLing2024/objbox/internal/backend"
)

// presetDisk 在账号 root 下直接预置一个对象文件，模拟管理员绕过 S3 协议落盘。
func presetDisk(t *testing.T, root, bucket, key, body string) {
	t.Helper()
	p := filepath.Join(root, bucket, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("预置目录失败: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}
}

// newSingleAccountServer 建一个只含指定账号的服务（可选只读），返回已启动的 httptest 服务。
// preset 在 server 启动前调用，用于直接在账号 root 下预置数据。
func newSingleAccountServer(t *testing.T, name string, readonly bool, preset func(root string)) (*httptest.Server, *account.Account) {
	t.Helper()
	store, err := account.Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	acct, err := store.Add(name, "", readonly)
	if err != nil {
		t.Fatal(err)
	}
	if preset != nil {
		preset(acct.Root)
	}
	srv, err := New(store)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); srv.Close() })
	return ts, acct
}

func TestIsolationCrossAccountMatchesMissing(t *testing.T) {
	// M0 反枚举断言在 autoCreateBucket=false 下必须原样成立。
	store, err := account.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	no := false
	alice, err := store.AddAccount("alice", "", false, account.AddOptions{AutoCreateBucket: &no})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.AddAccount("bob", "", false, account.AddOptions{AutoCreateBucket: &no})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); srv.Close() })

	resp := signedDo(t, ts, http.MethodPut, "/alice-bucket", nil, alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建桶状态 = %d，期望 200", resp.StatusCode)
	}
	resp.Body.Close()

	// bob 访问 alice 的桶
	cross := signedDo(t, ts, http.MethodGet, "/alice-bucket", nil, bob, nil)
	crossBody := readBody(t, cross)
	if cross.StatusCode != http.StatusForbidden {
		t.Fatalf("跨账号访问状态 = %d，期望 403", cross.StatusCode)
	}

	// bob 访问一个根本不存在的桶
	missing := signedDo(t, ts, http.MethodGet, "/missing-bucket", nil, bob, nil)
	missingBody := readBody(t, missing)
	if missing.StatusCode != http.StatusForbidden {
		t.Fatalf("桶不存在状态 = %d，期望 403", missing.StatusCode)
	}

	if !bytes.Equal(crossBody, missingBody) {
		t.Fatalf("跨账号与桶不存在的响应应逐字节一致:\n跨账号: %s\n不存在: %s", crossBody, missingBody)
	}
}

func TestReadonlyAccount(t *testing.T) {
	store, err := account.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ro, err := store.Add("reader", "", true)
	if err != nil {
		t.Fatal(err)
	}
	// 预置一个对象（在 server 打开后端之前直接写盘）。
	b, err := backend.New(ro.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.PutObject("ro-bucket", "hello.txt", map[string]string{}, strings.NewReader("hi"), 2, nil); err != nil {
		t.Fatal(err)
	}
	b.Close()

	srv, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); srv.Close() })

	get := signedDo(t, ts, http.MethodGet, "/ro-bucket/hello.txt", nil, ro, nil)
	body := readBody(t, get)
	if get.StatusCode != http.StatusOK || string(body) != "hi" {
		t.Fatalf("只读账号 GET 应 200/内容一致，实际 %d %q", get.StatusCode, body)
	}

	put := signedDo(t, ts, http.MethodPut, "/ro-bucket/other.txt", []byte("x"), ro, nil)
	put.Body.Close()
	if put.StatusCode != http.StatusForbidden {
		t.Fatalf("只读账号 PUT 应 403，实际 %d", put.StatusCode)
	}
}

func TestDisabledAccount(t *testing.T) {
	store, err := account.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dis, err := store.Add("disabled", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDisabled("disabled", true); err != nil {
		t.Fatal(err)
	}
	srv, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); srv.Close() })

	for _, c := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/", nil},
		{http.MethodPut, "/some-bucket", nil},
		{http.MethodGet, "/some-bucket/key", nil},
	} {
		resp := signedDo(t, ts, c.method, c.path, c.body, dis, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("停用账号 %s %s 应 403，实际 %d", c.method, c.path, resp.StatusCode)
		}
	}
}

func TestCRUDRoundTrip(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)

	// 建桶
	resp := signedDo(t, ts, http.MethodPut, "/crud-bucket", nil, alice, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建桶状态 = %d", resp.StatusCode)
	}

	// 传对象
	const content = "hello objbox"
	put := signedDo(t, ts, http.MethodPut, "/crud-bucket/hello.txt", []byte(content), alice, map[string]string{"Content-Type": "text/plain"})
	put.Body.Close()
	if put.StatusCode != http.StatusOK {
		t.Fatalf("PutObject 状态 = %d", put.StatusCode)
	}

	// 读回
	get := signedDo(t, ts, http.MethodGet, "/crud-bucket/hello.txt", nil, alice, nil)
	got := readBody(t, get)
	if get.StatusCode != http.StatusOK || string(got) != content {
		t.Fatalf("GetObject = %d %q", get.StatusCode, got)
	}

	// HEAD 校验 Content-Length 与 ETag
	head := signedDo(t, ts, http.MethodHead, "/crud-bucket/hello.txt", nil, alice, nil)
	head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HeadObject 状态 = %d", head.StatusCode)
	}
	if cl := head.Header.Get("Content-Length"); cl != "12" {
		t.Fatalf("HEAD Content-Length = %q，期望 12", cl)
	}
	etag := head.Header.Get("ETag")
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) != 34 {
		t.Fatalf("HEAD ETag = %q，应为带双引号的 MD5", etag)
	}

	// 删除
	del := signedDo(t, ts, http.MethodDelete, "/crud-bucket/hello.txt", nil, alice, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("DeleteObject 状态 = %d，期望 204", del.StatusCode)
	}

	// 再读 404
	get2 := signedDo(t, ts, http.MethodGet, "/crud-bucket/hello.txt", nil, alice, nil)
	get2.Body.Close()
	if get2.StatusCode != http.StatusNotFound {
		t.Fatalf("删除后读取应 404，实际 %d", get2.StatusCode)
	}
}

func TestInvalidBucketAndPathRejected(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/InvalidBucket/key", http.StatusBadRequest},
		{http.MethodGet, "/ab/key", http.StatusBadRequest},
		{http.MethodGet, "/valid-bucket/../secret", http.StatusBadRequest},
	}
	for _, c := range cases {
		resp := signedDo(t, ts, c.method, c.path, nil, alice, nil)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("%s %s 状态 = %d，期望 %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
}

func TestAnonymousRejectedWhenAccountsExist(t *testing.T) {
	ts, _, _, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("匿名请求应 403，实际 %d", resp.StatusCode)
	}
}

func TestHTTPRangeRequest(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	create := signedDo(t, ts, http.MethodPut, "/range-bucket", nil, alice, nil)
	create.Body.Close()
	put := signedDo(t, ts, http.MethodPut, "/range-bucket/nums", []byte("0123456789"), alice, nil)
	put.Body.Close()

	get := signedDo(t, ts, http.MethodGet, "/range-bucket/nums", nil, alice, map[string]string{"Range": "bytes=2-5"})
	body := readBody(t, get)
	if get.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range 状态 = %d，期望 206", get.StatusCode)
	}
	if string(body) != "2345" {
		t.Fatalf("Range 内容 = %q，期望 2345", body)
	}
	if cr := get.Header.Get("Content-Range"); cr != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q，期望 bytes 2-5/10", cr)
	}
}

func TestAccessLogMasksAuthorization(t *testing.T) {
	store, err := account.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alice, err := store.Add("alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	srv.AccessLog = true
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); srv.Close() })

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	resp := signedDo(t, ts, http.MethodGet, "/", nil, alice, nil)
	resp.Body.Close()

	out := buf.String()
	if strings.Contains(out, alice.SK) {
		t.Fatalf("访问日志绝不能包含 SK: %q", out)
	}
	if !strings.Contains(out, alice.AK) {
		t.Fatalf("访问日志应保留 AK: %q", out)
	}
}

// TestPresetBucketOnDiskVisible 覆盖「盘上已存在、未经 S3 协议创建」的桶：
// 账号 root 下直接预置桶目录与对象文件 → 启动服务 → 该账号能
// ListBuckets 列出它、ListObjects 看到对象、GetObject 读回内容一致、HeadObject 元信息正确。
func TestPresetBucketOnDiskVisible(t *testing.T) {
	const content = "hello preset"
	ts, alice := newSingleAccountServer(t, "alice", false, func(root string) {
		presetDisk(t, root, "preset-bucket", "docs/hello.txt", content)
	})

	// ListBuckets：必须包含直接落盘的桶，且不得暴露内部目录。
	resp := signedDo(t, ts, http.MethodGet, "/", nil, alice, nil)
	body := string(readBody(t, resp))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<Name>preset-bucket</Name>") {
		t.Fatalf("ListBuckets 状态/内容不符: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "<Name>.objbox</Name>") {
		t.Fatalf("内部目录不得作为桶暴露: %s", body)
	}

	// ListObjects：直接落盘的对象也要出现。
	resp = signedDo(t, ts, http.MethodGet, "/preset-bucket", nil, alice, nil)
	body = string(readBody(t, resp))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<Key>docs/hello.txt</Key>") {
		t.Fatalf("ListObjects 状态/内容不符: %d %s", resp.StatusCode, body)
	}

	// GetObject：读回内容一致。
	resp = signedDo(t, ts, http.MethodGet, "/preset-bucket/docs/hello.txt", nil, alice, nil)
	got := string(readBody(t, resp))
	if resp.StatusCode != http.StatusOK || got != content {
		t.Fatalf("GetObject = %d %q，期望 200 %q", resp.StatusCode, got, content)
	}

	// HeadObject：Content-Length 与 ETag（MD5 带引号）正确。
	head := signedDo(t, ts, http.MethodHead, "/preset-bucket/docs/hello.txt", nil, alice, nil)
	head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HeadObject 状态 = %d，期望 200", head.StatusCode)
	}
	if cl := head.Header.Get("Content-Length"); cl != "12" {
		t.Fatalf("HEAD Content-Length = %q，期望 12", cl)
	}
	sum := md5.Sum([]byte(content))
	wantETag := `"` + hex.EncodeToString(sum[:]) + `"`
	if etag := head.Header.Get("ETag"); etag != wantETag {
		t.Fatalf("HEAD ETag = %q，期望 %q", etag, wantETag)
	}
}

// TestReadonlyAccountPresetBucket 只读账号专项：预置桶与对象后，
// 读操作（ListBuckets/ListObjects/GetObject/HeadObject）全部放行；
// 写操作（CreateBucket/PutObject/DeleteObject/DeleteBucket）逐个 403 AccessDenied。
func TestReadonlyAccountPresetBucket(t *testing.T) {
	ts, ro := newSingleAccountServer(t, "reader", true, func(root string) {
		presetDisk(t, root, "ro-bucket", "hello.txt", "hi")
	})

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
		want   int
	}{
		{"ListBuckets 读正常", http.MethodGet, "/", nil, http.StatusOK},
		{"ListObjects 读正常", http.MethodGet, "/ro-bucket", nil, http.StatusOK},
		{"GetObject 读正常", http.MethodGet, "/ro-bucket/hello.txt", nil, http.StatusOK},
		{"HeadObject 读正常", http.MethodHead, "/ro-bucket/hello.txt", nil, http.StatusOK},
		{"CreateBucket 被拒", http.MethodPut, "/new-bucket", nil, http.StatusForbidden},
		{"PutObject 被拒", http.MethodPut, "/ro-bucket/new.txt", []byte("x"), http.StatusForbidden},
		{"DeleteObject 被拒", http.MethodDelete, "/ro-bucket/hello.txt", nil, http.StatusForbidden},
		{"DeleteBucket 被拒", http.MethodDelete, "/ro-bucket", nil, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := signedDo(t, ts, c.method, c.path, c.body, ro, nil)
			resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("%s %s = %d，期望 %d", c.method, c.path, resp.StatusCode, c.want)
			}
		})
	}

	// 被拒的写操作不得改变只读账号的数据。
	resp := signedDo(t, ts, http.MethodGet, "/ro-bucket/hello.txt", nil, ro, nil)
	if got := string(readBody(t, resp)); resp.StatusCode != http.StatusOK || got != "hi" {
		t.Fatalf("只读账号对象应保持不变，实际 %d %q", resp.StatusCode, got)
	}
}

// TestDeleteBucketNotEmptyErrorCode 删除非空桶必须返回 409 且响应 XML 的 Code 为
// BucketNotEmpty（而不是统一 403 AccessDenied）；删空对象后能成功删桶。
func TestDeleteBucketNotEmptyErrorCode(t *testing.T) {
	ts, alice := newSingleAccountServer(t, "alice", false, func(root string) {
		presetDisk(t, root, "preset-bucket", "docs/hello.txt", "hello")
	})

	// 非空 → 409 BucketNotEmpty。
	resp := signedDo(t, ts, http.MethodDelete, "/preset-bucket", nil, alice, nil)
	body := string(readBody(t, resp))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("删非空桶状态 = %d，期望 409；body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "<Code>BucketNotEmpty</Code>") {
		t.Fatalf("错误码应为 BucketNotEmpty，实际 body=%s", body)
	}
	if strings.Contains(body, "AccessDenied") {
		t.Fatalf("非空桶不应被统一 403 吞掉，实际 body=%s", body)
	}

	// 桶仍在，且对象可读。
	if get := signedDo(t, ts, http.MethodGet, "/preset-bucket/docs/hello.txt", nil, alice, nil); get.StatusCode != http.StatusOK {
		t.Fatalf("删桶失败后对象应仍在，实际 %d", get.StatusCode)
	} else {
		get.Body.Close()
	}

	// 删空对象 → 删桶成功。
	del := signedDo(t, ts, http.MethodDelete, "/preset-bucket/docs/hello.txt", nil, alice, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("DeleteObject 状态 = %d，期望 204", del.StatusCode)
	}
	rm := signedDo(t, ts, http.MethodDelete, "/preset-bucket", nil, alice, nil)
	rm.Body.Close()
	if rm.StatusCode != http.StatusNoContent {
		t.Fatalf("空桶删除状态 = %d，期望 204", rm.StatusCode)
	}
	// 删桶后 ListBuckets 不再包含它。
	resp = signedDo(t, ts, http.MethodGet, "/", nil, alice, nil)
	if body := string(readBody(t, resp)); strings.Contains(body, "<Name>preset-bucket</Name>") {
		t.Fatalf("删桶后不应再列出: %s", body)
	}
}
