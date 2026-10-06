package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// TestPresignedGetPut 预签名：GET 立即可用且内容正确；过期/篡改 → 403；PUT 预签名可上传。
func TestPresignedGetPut(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	ctx := context.Background()
	cl := newS3Client(t, ts, alice.AK, alice.SK)
	presign := s3.NewPresignClient(cl)

	const bucket = "presign-bucket"
	const key = "dir/hello.txt"
	const content = "presigned content"

	if _, err := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		Body: strings.NewReader(content), ContentLength: aws.Int64(int64(len(content))),
	}); err != nil {
		t.Fatal(err)
	}

	getURL, err := presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	}, func(o *s3.PresignOptions) { o.Expires = 60 * time.Second })
	if err != nil {
		t.Fatalf("PresignGetObject 失败: %v", err)
	}

	// 立即访问 → 200 内容正确。
	resp, err := http.Get(getURL.URL)
	if err != nil {
		t.Fatalf("预签名 GET 失败: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || string(body) != content {
		t.Fatalf("预签名 GET = %d %q，期望 200 %q", resp.StatusCode, body, content)
	}

	// 预签名 GET 配合 Range → 206 且只返回区间内容。
	rangeReq, err := http.NewRequest(http.MethodGet, getURL.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	rangeReq.Header.Set("Range", "bytes=0-4")
	rangeResp, err := http.DefaultClient.Do(rangeReq)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody := readBody(t, rangeResp)
	if rangeResp.StatusCode != http.StatusPartialContent || string(rangeBody) != content[:5] {
		t.Fatalf("预签名 Range = %d %q，期望 206 %q", rangeResp.StatusCode, rangeBody, content[:5])
	}

	// 过期：把 X-Amz-Date 改成 20 分钟前（expires=60）→ 403。
	expired := withQueryParam(t, getURL.URL, "X-Amz-Date", time.Now().UTC().Add(-20*time.Minute).Format("20060102T150405Z"))
	resp2, err := http.Get(expired)
	if err != nil {
		t.Fatal(err)
	}
	expiredBody := readBody(t, resp2)
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("过期预签名应 403，实际 %d", resp2.StatusCode)
	}
	if !strings.Contains(string(expiredBody), "expired") {
		t.Fatalf("过期响应应含 expired 文案，实际 %s", expiredBody)
	}

	// 篡改 signature → 403。
	tampered := withQueryParam(t, getURL.URL, "X-Amz-Signature", "deadbeef")
	resp3, err := http.Get(tampered)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("篡改签名应 403，实际 %d", resp3.StatusCode)
	}

	// 篡改 key（路径变了但签名没变）→ 403。
	tamperedKey := strings.Replace(getURL.URL, key, "dir/other.txt", 1)
	resp4, err := http.Get(tamperedKey)
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusForbidden {
		t.Fatalf("篡改 key 应 403，实际 %d", resp4.StatusCode)
	}

	// PUT 预签名能成功上传。
	const putKey = "dir/uploaded.txt"
	const putContent = "uploaded via presign"
	putURL, err := presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(putKey),
	}, func(o *s3.PresignOptions) { o.Expires = 60 * time.Second })
	if err != nil {
		t.Fatalf("PresignPutObject 失败: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, putURL.URL, strings.NewReader(putContent))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(putContent))
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("预签名 PUT 失败: %v", err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("预签名 PUT 应 200，实际 %d", putResp.StatusCode)
	}

	got, err := cl.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(putKey)})
	if err != nil {
		t.Fatalf("读取预签名上传对象失败: %v", err)
	}
	gotBody, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if string(gotBody) != putContent {
		t.Fatalf("预签名上传内容 = %q，期望 %q", gotBody, putContent)
	}
}

func withQueryParam(t *testing.T, raw, name, value string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set(name, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// TestE2EMultipartSDK 用真 HTTP + SDK 覆盖 CreateMultipartUpload/UploadPart/Complete/GetObject，
// 对象 ≥8MiB，确保走的是分片路径。
func TestE2EMultipartSDK(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	ctx := context.Background()
	cl := newS3Client(t, ts, alice.AK, alice.SK)

	const bucket = "sdk-mp-bucket"
	const key = "big/object.bin"
	if _, err := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}

	part1 := bytes.Repeat([]byte("A"), 5*1024*1024) // 5MiB
	part2 := bytes.Repeat([]byte("B"), 3*1024*1024) // 3MiB → 合计 8MiB
	create, err := cl.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload 失败: %v", err)
	}

	var completed []types.CompletedPart
	for i, part := range [][]byte{part1, part2} {
		n := int32(i + 1)
		up, err := cl.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(key),
			UploadId:      create.UploadId,
			PartNumber:    aws.Int32(n),
			Body:          bytes.NewReader(part),
			ContentLength: aws.Int64(int64(len(part))),
		})
		if err != nil {
			t.Fatalf("UploadPart(%d) 失败: %v", n, err)
		}
		completed = append(completed, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(n)})
	}

	if _, err := cl.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(bucket),
		Key:             aws.String(key),
		UploadId:        create.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload 失败: %v", err)
	}

	got, err := cl.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject 失败: %v", err)
	}
	data, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if len(data) != len(part1)+len(part2) {
		t.Fatalf("大对象大小 = %d，期望 %d", len(data), len(part1)+len(part2))
	}
	if !bytes.Equal(data[:len(part1)], part1) || !bytes.Equal(data[len(part1):], part2) {
		t.Fatal("大对象内容与上传分片不一致")
	}
}

// TestQuotaEnforced 配额：超限写入 403 QuotaExceeded；删除后可继续写。
func TestQuotaEnforced(t *testing.T) {
	ts, store, alice, _ := newTestServer(t)
	ctx := context.Background()
	cl := newS3Client(t, ts, alice.AK, alice.SK)

	const quota = 1 << 20 // 1MiB
	if err := store.SetQuota("alice", quota); err != nil {
		t.Fatal(err)
	}

	const bucket = "quota-bucket"
	if _, err := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}

	half := bytes.Repeat([]byte("x"), 512*1024)
	if _, err := cl.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("half.bin"),
		Body: bytes.NewReader(half), ContentLength: aws.Int64(int64(len(half))),
	}); err != nil {
		t.Fatalf("512KiB 写入应成功: %v", err)
	}

	twoMiB := bytes.Repeat([]byte("y"), 2*1024*1024)
	_, err := cl.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("too-big.bin"),
		Body: bytes.NewReader(twoMiB), ContentLength: aws.Int64(int64(len(twoMiB))),
	})
	if err == nil {
		t.Fatal("超过配额应失败")
	}
	if !strings.Contains(err.Error(), "QuotaExceeded") {
		t.Fatalf("错误应含 QuotaExceeded，实际 %v", err)
	}

	if _, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("half.bin")}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("after-delete.bin"),
		Body: bytes.NewReader(half), ContentLength: aws.Int64(int64(len(half))),
	}); err != nil {
		t.Fatalf("删除后 512KiB 写入应成功: %v", err)
	}
}

// TestRangeInvalidReturns416 越界 Range → 416 且带 Content-Range: bytes */size。
func TestRangeInvalidReturns416(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	if resp := signedDo(t, ts, http.MethodPut, "/range416-bucket", nil, alice, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("建桶失败: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	put := signedDo(t, ts, http.MethodPut, "/range416-bucket/nums", []byte("0123456789"), alice, nil)
	put.Body.Close()

	resp := signedDo(t, ts, http.MethodGet, "/range416-bucket/nums", nil, alice, map[string]string{"Range": "bytes=99999999-"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("越界 Range 应 416，实际 %d", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes */10" {
		t.Fatalf("Content-Range = %q，期望 bytes */10", cr)
	}
}

// TestMultiRangeIgnored 多区间 Range 有意只取第一段（不返回多段 body）。
func TestMultiRangeIgnored(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	if resp := signedDo(t, ts, http.MethodPut, "/multirange-bucket", nil, alice, nil); resp.StatusCode != http.StatusOK {
		t.Fatal("建桶失败")
	} else {
		resp.Body.Close()
	}
	put := signedDo(t, ts, http.MethodPut, "/multirange-bucket/nums", []byte("0123456789"), alice, nil)
	put.Body.Close()

	resp := signedDo(t, ts, http.MethodGet, "/multirange-bucket/nums", nil, alice, map[string]string{"Range": "bytes=0-1,3-4"})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("多区间应退化为单区间 206，实际 %d", resp.StatusCode)
	}
	if string(body) != "01" {
		t.Fatalf("多区间只应返回第一段 01，实际 %q", body)
	}
}

// TestDeleteMultiHTTP 批量删除：3 个 key（含不存在）→ Deleted 3、无 Error；再 ListObjects 只剩预期。
func TestDeleteMultiHTTP(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	ctx := context.Background()
	cl := newS3Client(t, ts, alice.AK, alice.SK)

	const bucket = "batch-del-bucket"
	if _, err := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a.txt", "b.txt", "keep.txt"} {
		if _, err := cl.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(k),
			Body: strings.NewReader("x"), ContentLength: aws.Int64(1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	out, err := cl.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(bucket),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("a.txt")},
			{Key: aws.String("b.txt")},
			{Key: aws.String("not-exist.txt")},
		}},
	})
	if err != nil {
		t.Fatalf("DeleteObjects 失败: %v", err)
	}
	if len(out.Deleted) != 3 {
		t.Fatalf("Deleted 应 3 条，实际 %d", len(out.Deleted))
	}
	if len(out.Errors) != 0 {
		t.Fatalf("不应有 Errors，实际 %+v", out.Errors)
	}

	lo, err := cl.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatal(err)
	}
	if len(lo.Contents) != 1 || aws.ToString(lo.Contents[0].Key) != "keep.txt" {
		t.Fatalf("删除后应只剩 keep.txt，实际 %+v", lo.Contents)
	}
}

// TestCopyObjectHTTP 复制后内容一致；源不存在 → NoSuchKey；跨账号源 → 403。
func TestCopyObjectHTTP(t *testing.T) {
	ts, _, alice, bob := newTestServer(t)
	ctx := context.Background()
	cl := newS3Client(t, ts, alice.AK, alice.SK)
	bobCl := newS3Client(t, ts, bob.AK, bob.SK)

	const srcBucket = "copy-src-bucket"
	const dstBucket = "copy-dst-bucket"
	if _, err := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(srcBucket)}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(dstBucket)}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(srcBucket), Key: aws.String("deep/hello.txt"),
		Body: strings.NewReader("copy content"), ContentLength: aws.Int64(12),
	}); err != nil {
		t.Fatal(err)
	}

	// 正常复制
	if _, err := cl.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(dstBucket), Key: aws.String("deep/copied.txt"),
		CopySource: aws.String("/" + srcBucket + "/deep/hello.txt"),
	}); err != nil {
		t.Fatalf("CopyObject 失败: %v", err)
	}
	got, err := cl.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(dstBucket), Key: aws.String("deep/copied.txt")})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if string(body) != "copy content" {
		t.Fatalf("复制内容 = %q", body)
	}

	// 源不存在 → NoSuchKey
	_, err = cl.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(dstBucket), Key: aws.String("x"),
		CopySource: aws.String("/" + srcBucket + "/missing.txt"),
	})
	if err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Fatalf("源不存在应 NoSuchKey，实际 %v", err)
	}

	// 跨账号：bob 的桶里放对象，alice 复制它 → 403
	const bobBucket = "bob-only-bucket"
	if _, err := bobCl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bobBucket)}); err != nil {
		t.Fatal(err)
	}
	if _, err := bobCl.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bobBucket), Key: aws.String("secret.txt"),
		Body: strings.NewReader("secret"), ContentLength: aws.Int64(6),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = cl.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(dstBucket), Key: aws.String("stolen.txt"),
		CopySource: aws.String("/" + bobBucket + "/secret.txt"),
	})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("跨账号复制应 403 AccessDenied，实际 %v", err)
	}
}

// TestResponseHeadersAndServer 校验 GET/HEAD 语义头与统一 Server/x-amz-request-id。
func TestResponseHeadersAndServer(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	if resp := signedDo(t, ts, http.MethodPut, "/hdr-bucket", nil, alice, nil); resp.StatusCode != http.StatusOK {
		t.Fatal("建桶失败")
	} else {
		resp.Body.Close()
	}
	put := signedDo(t, ts, http.MethodPut, "/hdr-bucket/obj", []byte("hello"), alice, map[string]string{"Content-Type": "text/plain"})
	put.Body.Close()

	get := signedDo(t, ts, http.MethodGet, "/hdr-bucket/obj", nil, alice, nil)
	body := readBody(t, get)
	if string(body) != "hello" {
		t.Fatalf("内容 = %q", body)
	}
	for _, h := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified", "Accept-Ranges"} {
		if get.Header.Get(h) == "" {
			t.Fatalf("GET 响应缺少 %s", h)
		}
	}
	if get.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q", get.Header.Get("Accept-Ranges"))
	}
	if get.Header.Get("Server") != "objbox" {
		t.Fatalf("Server = %q，期望 objbox", get.Header.Get("Server"))
	}
	if get.Header.Get("x-amz-request-id") == "" {
		t.Fatal("缺少 x-amz-request-id")
	}

	head := signedDo(t, ts, http.MethodHead, "/hdr-bucket/obj", nil, alice, nil)
	head.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified", "Accept-Ranges"} {
		if head.Header.Get(h) == "" {
			t.Fatalf("HEAD 响应缺少 %s", h)
		}
	}
}

// TestErrorResponseRequestID 协议层错误响应为 S3 XML，含 RequestId 与 Resource。
func TestErrorResponseRequestID(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	if resp := signedDo(t, ts, http.MethodPut, "/xml-bucket", nil, alice, nil); resp.StatusCode != http.StatusOK {
		t.Fatal("建桶失败")
	} else {
		resp.Body.Close()
	}
	resp := signedDo(t, ts, http.MethodGet, "/xml-bucket/missing.txt", nil, alice, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("缺失对象应 404，实际 %d", resp.StatusCode)
	}
	for _, tag := range []string{"<Error>", "<Code>", "<Message>", "<RequestId>", "<Resource>"} {
		if !strings.Contains(string(body), tag) {
			t.Fatalf("错误体缺少 %s：%s", tag, body)
		}
	}
}
