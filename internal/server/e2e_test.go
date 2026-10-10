package server

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func staticCredentials(ak, sk string) aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk}, nil
	})
}

func newS3Client(t *testing.T, ts *httptest.Server, ak, sk string) *s3.Client {
	t.Helper()
	cfg := aws.Config{
		Region:                     testRegion,
		Credentials:                staticCredentials(ak, sk),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
	})
}

// TestE2ESDKFlow 用真 HTTP 服务 + aws-sdk-go-v2/service/s3 客户端跑完整链路。
func TestE2ESDKFlow(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	ctx := context.Background()
	cl := newS3Client(t, ts, alice.AK, alice.SK)

	const bucket = "sdk-bucket"
	const key = "docs/hello.txt"
	const content = "hello sdk over real http"

	if _, err := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket 失败: %v", err)
	}

	if _, err := cl.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          strings.NewReader(content),
		ContentLength: aws.Int64(int64(len(content))),
		ContentType:   aws.String("text/plain"),
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

	head, err := cl.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("HeadObject 失败: %v", err)
	}
	sum := md5.Sum([]byte(content))
	wantETag := `"` + hex.EncodeToString(sum[:]) + `"`
	if head.ETag == nil || *head.ETag != wantETag {
		t.Fatalf("HeadObject ETag = %v，期望 %q", head.ETag, wantETag)
	}
	if head.ContentLength == nil || *head.ContentLength != int64(len(content)) {
		t.Fatalf("HeadObject Content-Length = %v，期望 %d", head.ContentLength, len(content))
	}

	lb, err := cl.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets 失败: %v", err)
	}
	found := false
	for _, b := range lb.Buckets {
		if b.Name != nil && *b.Name == bucket {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListBuckets 未包含 %q: %+v", bucket, lb.Buckets)
	}

	lo, err := cl.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("ListObjectsV2 失败: %v", err)
	}
	if lo.KeyCount == nil || *lo.KeyCount != 1 || len(lo.Contents) != 1 || aws.ToString(lo.Contents[0].Key) != key {
		t.Fatalf("ListObjectsV2 结果错误: keyCount=%v contents=%+v", lo.KeyCount, lo.Contents)
	}

	if _, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
		t.Fatalf("DeleteObject 失败: %v", err)
	}
	if _, err := cl.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("DeleteBucket 失败: %v", err)
	}
}

// TestE2EWrongSKRejected 错 SK 的客户端必须被拒。
func TestE2EWrongSKRejected(t *testing.T) {
	ts, _, alice, _ := newTestServer(t)
	ctx := context.Background()
	bad := newS3Client(t, ts, alice.AK, "wrong-secret-key")

	_, err := bad.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err == nil {
		t.Fatal("错 SK 的请求应被拒绝")
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if apiErr.ErrorCode() != "SignatureDoesNotMatch" && apiErr.ErrorCode() != "AccessDenied" {
			t.Fatalf("错误码 = %q，期望签名类错误", apiErr.ErrorCode())
		}
	}
}

// TestE2EPresetBucketSDK 用真 S3 客户端读取管理员绕过协议直接落盘的桶与对象，
// 并断言删除非空桶返回 BucketNotEmpty（而不是统一 403 AccessDenied）。
func TestE2EPresetBucketSDK(t *testing.T) {
	const content = "preset via disk"
	ts, alice := newSingleAccountServer(t, "alice", false, func(root string) {
		presetDisk(t, root, "preset-bucket", "docs/hello.txt", content)
	})
	ctx := context.Background()
	cl := newS3Client(t, ts, alice.AK, alice.SK)

	lb, err := cl.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets 失败: %v", err)
	}
	found := false
	for _, b := range lb.Buckets {
		if aws.ToString(b.Name) == "preset-bucket" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListBuckets 未包含直接落盘的桶: %+v", lb.Buckets)
	}

	lo, err := cl.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("preset-bucket")})
	if err != nil {
		t.Fatalf("ListObjectsV2 失败: %v", err)
	}
	if len(lo.Contents) != 1 || aws.ToString(lo.Contents[0].Key) != "docs/hello.txt" {
		t.Fatalf("ListObjectsV2 = %+v", lo.Contents)
	}

	out, err := cl.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("preset-bucket"), Key: aws.String("docs/hello.txt")})
	if err != nil {
		t.Fatalf("GetObject 失败: %v", err)
	}
	got, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if string(got) != content {
		t.Fatalf("GetObject 内容 = %q，期望 %q", got, content)
	}

	if _, err := cl.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("preset-bucket"), Key: aws.String("docs/hello.txt")}); err != nil {
		t.Fatalf("HeadObject 失败: %v", err)
	}

	// 非空桶删除 → BucketNotEmpty。
	if _, err := cl.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("preset-bucket")}); err == nil {
		t.Fatal("删除非空桶应失败")
	} else {
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "BucketNotEmpty" {
			t.Fatalf("非空桶错误码应 BucketNotEmpty，实际 %v", err)
		}
	}

	// 删空对象后可删桶。
	if _, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("preset-bucket"), Key: aws.String("docs/hello.txt")}); err != nil {
		t.Fatalf("DeleteObject 失败: %v", err)
	}
	if _, err := cl.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("preset-bucket")}); err != nil {
		t.Fatalf("删空桶失败: %v", err)
	}
}

// TestE2EReadonlyPresetBucketSDK 只读账号专项：预置桶与对象后，
// 读接口全部成功，写接口全部 AccessDenied。
func TestE2EReadonlyPresetBucketSDK(t *testing.T) {
	ts, ro := newSingleAccountServer(t, "reader", true, func(root string) {
		presetDisk(t, root, "ro-bucket", "hello.txt", "hi")
	})
	ctx := context.Background()
	cl := newS3Client(t, ts, ro.AK, ro.SK)

	// 读：全部成功。
	if _, err := cl.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
		t.Fatalf("只读账号 ListBuckets 应成功: %v", err)
	}
	if _, err := cl.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("ro-bucket")}); err != nil {
		t.Fatalf("只读账号 ListObjectsV2 应成功: %v", err)
	}
	out, err := cl.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("ro-bucket"), Key: aws.String("hello.txt")})
	if err != nil {
		t.Fatalf("只读账号 GetObject 应成功: %v", err)
	}
	got, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if string(got) != "hi" {
		t.Fatalf("只读账号 GetObject 内容 = %q", got)
	}
	if _, err := cl.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("ro-bucket"), Key: aws.String("hello.txt")}); err != nil {
		t.Fatalf("只读账号 HeadObject 应成功: %v", err)
	}

	// 写：逐个断言被拒且错误码为 AccessDenied。
	writes := []struct {
		name string
		do   func() error
	}{
		{"CreateBucket", func() error {
			_, err := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("new-bucket")})
			return err
		}},
		{"PutObject", func() error {
			_, err := cl.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("ro-bucket"), Key: aws.String("new.txt"),
				Body: strings.NewReader("x"), ContentLength: aws.Int64(1),
			})
			return err
		}},
		{"DeleteObject", func() error {
			_, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("ro-bucket"), Key: aws.String("hello.txt")})
			return err
		}},
		{"DeleteBucket", func() error {
			_, err := cl.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("ro-bucket")})
			return err
		}},
	}
	for _, c := range writes {
		t.Run(c.name, func(t *testing.T) {
			err := c.do()
			if err == nil {
				t.Fatalf("只读账号 %s 应被拒", c.name)
			}
			var apiErr smithy.APIError
			if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "AccessDenied" {
				t.Fatalf("只读账号 %s 错误码应 AccessDenied，实际 %v", c.name, err)
			}
		})
	}

	// 被拒的写操作不得影响数据。
	out, err = cl.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("ro-bucket"), Key: aws.String("hello.txt")})
	if err != nil {
		t.Fatalf("写被拒后 GetObject 仍应成功: %v", err)
	}
	got, _ = io.ReadAll(out.Body)
	out.Body.Close()
	if string(got) != "hi" {
		t.Fatalf("写被拒后对象内容应不变，实际 %q", got)
	}
}
