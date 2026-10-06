package backend

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
)

func newBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func put(t *testing.T, b *Backend, bucket, key, body string) {
	t.Helper()
	hdr := map[string]string{"Content-Type": "text/plain", "X-Amz-Meta-Owner": "alice"}
	if _, err := b.PutObject(bucket, key, hdr, strings.NewReader(body), int64(len(body)), nil); err != nil {
		t.Fatalf("PutObject(%s/%s) 失败: %v", bucket, key, err)
	}
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestBucketCRUD(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("my-bucket"); err != nil {
		t.Fatalf("CreateBucket 失败: %v", err)
	}
	if ok, _ := b.BucketExists("my-bucket"); !ok {
		t.Fatal("桶应存在")
	}
	if err := b.CreateBucket("my-bucket"); !gofakes3.IsAlreadyExists(err) {
		t.Fatalf("重复建桶应返回 BucketAlreadyExists，实际 %v", err)
	}

	buckets, err := b.ListBuckets()
	if err != nil || len(buckets) != 1 || buckets[0].Name != "my-bucket" {
		t.Fatalf("ListBuckets = %+v err=%v", buckets, err)
	}

	put(t, b, "my-bucket", "obj", "data")
	if err := b.DeleteBucket("my-bucket"); !gofakes3.HasErrorCode(err, gofakes3.ErrBucketNotEmpty) {
		t.Fatalf("非空桶删除应返回 BucketNotEmpty，实际 %v", err)
	}

	if _, err := b.DeleteObject("my-bucket", "obj"); err != nil {
		t.Fatalf("DeleteObject 失败: %v", err)
	}
	if err := b.DeleteBucket("my-bucket"); err != nil {
		t.Fatalf("空桶删除失败: %v", err)
	}
	if err := b.DeleteBucket("my-bucket"); !gofakes3.HasErrorCode(err, gofakes3.ErrNoSuchBucket) {
		t.Fatalf("删除不存在的桶应返回 NoSuchBucket，实际 %v", err)
	}
}

func TestObjectPutGetHeadDelete(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	const body = "hello objbox"
	put(t, b, "bkt", "dir/hello.txt", body)

	obj, err := b.GetObject("bkt", "dir/hello.txt", nil)
	if err != nil {
		t.Fatalf("GetObject 失败: %v", err)
	}
	got, _ := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if string(got) != body {
		t.Fatalf("读回内容 = %q，期望 %q", got, body)
	}
	if obj.Metadata["Content-Type"] != "text/plain" {
		t.Fatalf("Content-Type 丢失: %+v", obj.Metadata)
	}
	if obj.Metadata["X-Amz-Meta-Owner"] != "alice" {
		t.Fatalf("自定义元数据丢失: %+v", obj.Metadata)
	}

	head, err := b.HeadObject("bkt", "dir/hello.txt")
	if err != nil {
		t.Fatalf("HeadObject 失败: %v", err)
	}
	if head.Size != int64(len(body)) {
		t.Fatalf("HEAD size = %d，期望 %d", head.Size, len(body))
	}
	if gotETag := `"` + hex.EncodeToString(head.Hash) + `"`; gotETag != `"`+md5hex(body)+`"` {
		t.Fatalf("HEAD ETag = %s，期望 %q", gotETag, `"`+md5hex(body)+`"`)
	}

	if _, err := b.DeleteObject("bkt", "dir/hello.txt"); err != nil {
		t.Fatalf("DeleteObject 失败: %v", err)
	}
	if _, err := b.GetObject("bkt", "dir/hello.txt", nil); !gofakes3.HasErrorCode(err, gofakes3.ErrNoSuchKey) {
		t.Fatalf("删除后读取应返回 NoSuchKey，实际 %v", err)
	}
}

func TestGetObjectRange(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	put(t, b, "bkt", "nums", "0123456789")

	cases := []struct {
		name string
		req  *gofakes3.ObjectRangeRequest
		want string
	}{
		{"闭区间", &gofakes3.ObjectRangeRequest{Start: 2, End: 5}, "2345"},
		{"到结尾", &gofakes3.ObjectRangeRequest{Start: 5, End: gofakes3.RangeNoEnd}, "56789"},
		{"末尾 N", &gofakes3.ObjectRangeRequest{FromEnd: true, End: 3}, "789"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, err := b.GetObject("bkt", "nums", c.req)
			if err != nil {
				t.Fatalf("GetObject 失败: %v", err)
			}
			defer obj.Contents.Close()
			got, _ := io.ReadAll(obj.Contents)
			if string(got) != c.want {
				t.Fatalf("Range 内容 = %q，期望 %q", got, c.want)
			}
			if obj.Range == nil {
				t.Fatal("Range 字段应被设置")
			}
		})
	}
}

func TestListBucketPrefixDelimiterPagination(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a.txt", "dir/b.txt", "dir/c.txt", "e.txt"} {
		put(t, b, "bkt", k, "x")
	}

	// 全部
	all, err := b.ListBucket("bkt", nil, gofakes3.ListBucketPage{MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Contents) != 4 {
		t.Fatalf("全部列表应 4 项，实际 %d", len(all.Contents))
	}

	// prefix
	pre := &gofakes3.Prefix{HasPrefix: true, Prefix: "dir/"}
	only, err := b.ListBucket("bkt", pre, gofakes3.ListBucketPage{MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Contents) != 2 {
		t.Fatalf("prefix 列表应 2 项，实际 %d", len(only.Contents))
	}

	// delimiter 归并
	delim := &gofakes3.Prefix{HasDelimiter: true, Delimiter: "/"}
	grouped, err := b.ListBucket("bkt", delim, gofakes3.ListBucketPage{MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(grouped.Contents) != 2 {
		t.Fatalf("delimiter 列表 contents 应 2 项，实际 %d（%+v）", len(grouped.Contents), grouped.Contents)
	}
	if len(grouped.CommonPrefixes) != 1 || grouped.CommonPrefixes[0].Prefix != "dir/" {
		t.Fatalf("CommonPrefixes 错误: %+v", grouped.CommonPrefixes)
	}

	// 分页：max-keys=2
	pg1, err := b.ListBucket("bkt", nil, gofakes3.ListBucketPage{MaxKeys: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(pg1.Contents) != 2 || !pg1.IsTruncated || pg1.NextMarker == "" {
		t.Fatalf("第一页错误: contents=%d truncated=%v marker=%q", len(pg1.Contents), pg1.IsTruncated, pg1.NextMarker)
	}
	pg2, err := b.ListBucket("bkt", nil, gofakes3.ListBucketPage{MaxKeys: 2, HasMarker: true, Marker: pg1.NextMarker})
	if err != nil {
		t.Fatal(err)
	}
	if len(pg2.Contents) != 2 || pg2.IsTruncated {
		t.Fatalf("第二页错误: contents=%d truncated=%v", len(pg2.Contents), pg2.IsTruncated)
	}
	if pg2.Contents[0].Key <= pg1.NextMarker {
		t.Fatalf("第二页应从 marker 之后开始: %q <= %q", pg2.Contents[0].Key, pg1.NextMarker)
	}
}

func TestReconcileRebuildsIndex(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	put(t, b, "bkt", "a.txt", "hello")

	// 1) 删除元数据后重建
	if err := b.meta.Delete("bkt", "a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := b.Reconcile(); err != nil {
		t.Fatal(err)
	}
	md, ok, err := b.meta.Get("bkt", "a.txt")
	if err != nil || !ok {
		t.Fatalf("重建后元数据应存在: ok=%v err=%v", ok, err)
	}
	if md.ETag != md5hex("hello") {
		t.Fatalf("重建 ETag = %q，期望 %q", md.ETag, md5hex("hello"))
	}

	// 2) 直接落盘的文件也应被纳入索引
	direct := filepath.Join(b.Root(), "bkt", "direct.txt")
	if err := os.WriteFile(direct, []byte("direct"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := b.meta.Get("bkt", "direct.txt"); !ok {
		t.Fatal("磁盘新增文件应在重建后进入索引")
	}

	// 3) 文件消失后元数据应被清除
	if err := os.Remove(direct); err != nil {
		t.Fatal(err)
	}
	if err := b.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := b.meta.Get("bkt", "direct.txt"); ok {
		t.Fatal("文件消失后元数据应被清除")
	}
}
