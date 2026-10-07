package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YLing2024/objbox/internal/account"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// listBucketsBody 用签名请求取 ListBuckets 原始 XML。
func listBucketsBody(t *testing.T, ts *httptest.Server, acct *account.Account) string {
	t.Helper()
	resp := signedDo(t, ts, http.MethodGet, "/", nil, acct, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ListBuckets 状态 = %d，期望 200（%s）", resp.StatusCode, body)
	}
	return string(body)
}

// newNoAutoServer 起一个所有账号 autoCreateBucket=false 的服务。
func newNoAutoServer(t *testing.T) (ts *httptest.Server, alice, bob *account.Account) {
	t.Helper()
	store, err := account.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	no := false
	alice, err = store.AddAccount("alice", "", false, account.AddOptions{AutoCreateBucket: &no})
	if err != nil {
		t.Fatal(err)
	}
	bob, err = store.AddAccount("bob", "", false, account.AddOptions{AutoCreateBucket: &no})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	ts = httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); srv.Close() })
	return ts, alice, bob
}

// 1. autoCreateBucket=true：对不存在的桶 PUT 对象 → 成功且桶被建出。
func TestM5PutObjectCreatesBucket(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)

	const bucket = "m5-auto-bucket"
	resp := signedDo(t, ts, http.MethodPut, "/"+bucket+"/hello.txt", []byte("hi"), alice, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("对不存在桶 PUT 对象应 200，实际 %d（%s）", resp.StatusCode, body)
	}
	if lb := listBucketsBody(t, ts, alice); !strings.Contains(lb, "<Name>"+bucket+"</Name>") {
		t.Fatalf("ListBuckets 应能看到自动建出的桶 %q：%s", bucket, lb)
	}

	// 写进去的对象可以读回。
	get := signedDo(t, ts, http.MethodGet, "/"+bucket+"/hello.txt", nil, alice, nil)
	got := readBody(t, get)
	if get.StatusCode != http.StatusOK || string(got) != "hi" {
		t.Fatalf("读回 = %d %q", get.StatusCode, got)
	}
}

// 2. autoCreateBucket=true：不存在的桶 GET/HEAD/LIST 不得 403。
func TestM5ReadMissingBucketNotForbidden(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)

	// GET 对象：桶被补建，对象不存在 → 404 NoSuchKey。
	get := signedDo(t, ts, http.MethodGet, "/m5-missing-obj-bucket/dir/file.txt", nil, alice, nil)
	body := readBody(t, get)
	if get.StatusCode == http.StatusForbidden {
		t.Fatalf("GET 对象不应 403: %s", body)
	}
	if get.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "NoSuchKey") {
		t.Fatalf("GET 缺失对象应 404 NoSuchKey，实际 %d %s", get.StatusCode, body)
	}

	// HEAD 对象：404，且不得 403。
	head := signedDo(t, ts, http.MethodHead, "/m5-missing-head-bucket/file.txt", nil, alice, nil)
	head.Body.Close()
	if head.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD 缺失对象应 404，实际 %d", head.StatusCode)
	}

	// GET 桶（ListObjects）：200 空列表。
	list := signedDo(t, ts, http.MethodGet, "/m5-missing-list-bucket", nil, alice, nil)
	lb := readBody(t, list)
	if list.StatusCode != http.StatusOK {
		t.Fatalf("GET 缺失桶列表应 200，实际 %d（%s）", list.StatusCode, lb)
	}
	if strings.Contains(string(lb), "<Key>") {
		t.Fatalf("空桶列表不应含对象：%s", lb)
	}

	// HEAD 桶：200。
	hb := signedDo(t, ts, http.MethodHead, "/m5-missing-headbucket", nil, alice, nil)
	hb.Body.Close()
	if hb.StatusCode != http.StatusOK {
		t.Fatalf("HEAD 缺失桶应 200，实际 %d", hb.StatusCode)
	}
}

// 3. autoCreateBucket=false：不存在的桶仍 403，且与跨账号响应逐字节一致。
func TestM5NoAutoStillForbiddenAndIdentical(t *testing.T) {
	ts, alice, bob := newNoAutoServer(t)

	// alice 显式建一个桶。
	if resp := signedDo(t, ts, http.MethodPut, "/m5-false-real", nil, alice, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("建桶失败: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// autoCreateBucket=false：对不存在桶写对象 → 403。
	put := signedDo(t, ts, http.MethodPut, "/m5-false-missing/obj.txt", []byte("x"), alice, nil)
	putBody := readBody(t, put)
	if put.StatusCode != http.StatusForbidden {
		t.Fatalf("false 账号写不存在桶应 403，实际 %d（%s）", put.StatusCode, putBody)
	}

	// bob 跨账号访问 alice 的桶 → 403，且与「桶不存在」逐字节一致。
	cross := signedDo(t, ts, http.MethodGet, "/m5-false-real", nil, bob, nil)
	crossBody := readBody(t, cross)
	missing := signedDo(t, ts, http.MethodGet, "/m5-false-real-missing", nil, bob, nil)
	missingBody := readBody(t, missing)
	if cross.StatusCode != http.StatusForbidden || missing.StatusCode != http.StatusForbidden {
		t.Fatalf("跨账号/不存在都应 403，实际 %d/%d", cross.StatusCode, missing.StatusCode)
	}
	if string(crossBody) != string(missingBody) {
		t.Fatalf("响应应逐字节一致:\n跨账号: %s\n不存在: %s", crossBody, missingBody)
	}

	// 确认没有意外把桶建出来。
	lb := listBucketsBody(t, ts, alice)
	if strings.Contains(lb, "m5-false-missing") {
		t.Fatalf("false 账号不应自动建桶：%s", lb)
	}
}

// 4. DELETE Bucket 对不存在的桶 → NoSuchBucket，不得被自动建桶改写成 200。
func TestM5DeleteMissingBucketStaysNoSuchBucket(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)

	del := signedDo(t, ts, http.MethodDelete, "/m5-del-missing-bucket", nil, alice, nil)
	body := readBody(t, del)
	if del.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE 缺失桶应 404，实际 %d（%s）", del.StatusCode, body)
	}
	if !strings.Contains(string(body), "NoSuchBucket") {
		t.Fatalf("错误码应为 NoSuchBucket：%s", body)
	}
	if lb := listBucketsBody(t, ts, alice); strings.Contains(lb, "m5-del-missing-bucket") {
		t.Fatalf("DELETE 缺失桶不得建桶：%s", lb)
	}
}

// 5. DELETE Object 对不存在的桶 → NoSuchBucket，不建桶。
func TestM5DeleteMissingBucketObjectNoSuchBucket(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)

	del := signedDo(t, ts, http.MethodDelete, "/m5-del-missing-obj/obj.txt", nil, alice, nil)
	body := readBody(t, del)
	if del.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "NoSuchBucket") {
		t.Fatalf("DELETE 缺失桶内对象应 404 NoSuchBucket，实际 %d（%s）", del.StatusCode, body)
	}
	if lb := listBucketsBody(t, ts, alice); strings.Contains(lb, "m5-del-missing-obj") {
		t.Fatalf("DELETE 不得建桶：%s", lb)
	}
}

// 6. CopyObject 目标桶不存在 → 403，且不隐式建桶。
func TestM5CopyToMissingBucketForbidden(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)

	if resp := signedDo(t, ts, http.MethodPut, "/m5-copy-src", nil, alice, nil); resp.StatusCode != http.StatusOK {
		t.Fatal("建源桶失败")
	} else {
		resp.Body.Close()
	}
	if resp := signedDo(t, ts, http.MethodPut, "/m5-copy-src/a.txt", []byte("data"), alice, nil); resp.StatusCode != http.StatusOK {
		t.Fatal("写源对象失败")
	} else {
		resp.Body.Close()
	}

	resp := signedDo(t, ts, http.MethodPut, "/m5-copy-dst/a.txt", nil, alice, map[string]string{
		"x-amz-copy-source": "/m5-copy-src/a.txt",
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CopyObject 目标桶缺失应 403，实际 %d（%s）", resp.StatusCode, body)
	}
	if lb := listBucketsBody(t, ts, alice); strings.Contains(lb, "m5-copy-dst") {
		t.Fatalf("CopyObject 不得隐式建目标桶：%s", lb)
	}
}

// 7. autoCreateBucket=true：分片初始化对不存在的桶同样先建桶。
func TestM5MultipartInitCreatesBucket(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	ctx := context.Background()
	cl := newS3Client(t, ts, alice.AK, alice.SK)

	const bucket = "m5-mp-auto-bucket"
	out, err := cl.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String("big.bin"),
	})
	if err != nil {
		t.Fatalf("对不存在桶初始化分片应成功: %v", err)
	}
	if out.UploadId == nil || *out.UploadId == "" {
		t.Fatal("应返回 UploadId")
	}
	if lb := listBucketsBody(t, ts, alice); !strings.Contains(lb, bucket) {
		t.Fatalf("分片初始化应建出桶：%s", lb)
	}
}

// 8. 非法桶名照旧 400，不因自动建桶放宽。
func TestM5InvalidBucketStillRejected(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)

	resp := signedDo(t, ts, http.MethodPut, "/Invalid_Bucket/obj.txt", []byte("x"), alice, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法桶名应 400，实际 %d（%s）", resp.StatusCode, body)
	}
}
