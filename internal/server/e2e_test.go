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
