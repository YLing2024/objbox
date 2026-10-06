package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YLing2024/objbox/internal/account"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// newReloadTestServer 起一个真实 HTTP 服务，并把热重载节流设为 0，
// 让每次请求都检查 accounts.json 的变化。
func newReloadTestServer(t *testing.T) (ts *httptest.Server, store *account.Store, dir string, alice, bob *account.Account) {
	t.Helper()
	dir = t.TempDir()
	store, err := account.Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	alice, err = store.Add("alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	bob, err = store.Add("bob", "", false)
	if err != nil {
		t.Fatal(err)
	}
	store.SetReloadInterval(0)
	srv, err := New(store)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ts = httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
	})
	return ts, store, dir, alice, bob
}

func statusOf(t *testing.T, ts *httptest.Server, method, path string, acct *account.Account) int {
	t.Helper()
	resp := signedDo(t, ts, method, path, nil, acct, nil)
	resp.Body.Close()
	return resp.StatusCode
}

// TestHotReloadAuthChangesHTTP：运行期通过写 accounts.json 做
// rotate/disable/enable/remove/add，下一次 HTTP 请求立即反映。
func TestHotReloadAuthChangesHTTP(t *testing.T) {
	ts, _, dir, alice, bob := newReloadTestServer(t)

	if got := statusOf(t, ts, http.MethodGet, "/", alice); got != http.StatusOK {
		t.Fatalf("初始 alice 应可用，实际 %d", got)
	}

	// 模拟另一个进程（CLI）改账号表。
	cli, err := account.Load(dir)
	if err != nil {
		t.Fatalf("CLI Load 失败: %v", err)
	}
	newSK, err := cli.Rotate("alice")
	if err != nil {
		t.Fatalf("Rotate 失败: %v", err)
	}
	if err := cli.SetDisabled("bob", true); err != nil {
		t.Fatalf("disable bob 失败: %v", err)
	}
	carol, err := cli.Add("carol", "", false)
	if err != nil {
		t.Fatalf("add carol 失败: %v", err)
	}

	// 旧 SK 立即失效；新 SK（AK 不变）立即可用。
	if got := statusOf(t, ts, http.MethodGet, "/", alice); got != http.StatusForbidden {
		t.Fatalf("轮换后旧 SK 应 403，实际 %d", got)
	}
	rotated := &account.Account{AK: alice.AK, SK: newSK}
	if got := statusOf(t, ts, http.MethodGet, "/", rotated); got != http.StatusOK {
		t.Fatalf("轮换后新 SK 应 200，实际 %d", got)
	}

	// 停用账号立即被拒。
	if got := statusOf(t, ts, http.MethodGet, "/", bob); got != http.StatusForbidden {
		t.Fatalf("停用账号应 403，实际 %d", got)
	}

	// 新增账号立即可用。
	if got := statusOf(t, ts, http.MethodGet, "/", carol); got != http.StatusOK {
		t.Fatalf("新增账号应 200，实际 %d", got)
	}

	// 重新启用后立即恢复。
	if err := cli.SetDisabled("bob", false); err != nil {
		t.Fatalf("enable bob 失败: %v", err)
	}
	if got := statusOf(t, ts, http.MethodGet, "/", bob); got != http.StatusOK {
		t.Fatalf("重新启用后应 200，实际 %d", got)
	}

	// 移除后立即失效。
	if err := cli.Remove("carol"); err != nil {
		t.Fatalf("remove carol 失败: %v", err)
	}
	if got := statusOf(t, ts, http.MethodGet, "/", carol); got != http.StatusForbidden {
		t.Fatalf("移除账号应 403，实际 %d", got)
	}
}

// TestHotReloadParseErrorKeepsServingHTTP：文件半写/非法 JSON 时，
// 服务必须继续用旧账号表，不能退化成全部 403。
func TestHotReloadParseErrorKeepsServingHTTP(t *testing.T) {
	ts, _, dir, alice, _ := newReloadTestServer(t)

	if err := os.WriteFile(filepath.Join(dir, account.FileName), []byte("{ 半写文件"), account.FileMode); err != nil {
		t.Fatalf("写坏账号表失败: %v", err)
	}

	if got := statusOf(t, ts, http.MethodGet, "/", alice); got != http.StatusOK {
		t.Fatalf("解析失败后旧账号应仍可用，实际 %d", got)
	}
}

// TestE2EHotReloadNewAccountSDK：服务运行中新增账号后，
// 用 aws-sdk-go-v2/service/s3 客户端经真实 HTTP 完成建桶/上传/下载。
func TestE2EHotReloadNewAccountSDK(t *testing.T) {
	ts, _, dir, alice, _ := newReloadTestServer(t)
	ctx := context.Background()

	// 服务运行中用另一个进程的写入方式新增账号。
	cli, err := account.Load(dir)
	if err != nil {
		t.Fatalf("CLI Load 失败: %v", err)
	}
	carol, err := cli.Add("carol", "", false)
	if err != nil {
		t.Fatalf("add carol 失败: %v", err)
	}

	cl := newS3Client(t, ts, carol.AK, carol.SK)
	const bucket = "hot-bucket"
	const key = "hot/hello.txt"
	const content = "hello hot reload"

	if _, err := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket 失败: %v", err)
	}
	if _, err := cl.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          strings.NewReader(content),
		ContentLength: aws.Int64(int64(len(content))),
	}); err != nil {
		t.Fatalf("PutObject 失败: %v", err)
	}
	out, err := cl.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject 失败: %v", err)
	}
	got, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if string(got) != content {
		t.Fatalf("GetObject 内容 = %q，期望 %q", got, content)
	}

	// 未变更的 alice 客户端仍正常，证明热重载没有破坏既有账号。
	if _, err := newS3Client(t, ts, alice.AK, alice.SK).ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
		t.Fatalf("alice ListBuckets 失败: %v", err)
	}
}
