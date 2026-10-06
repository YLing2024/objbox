package backend

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
)

// TestMultipartFullFlow 覆盖分片上传全流程：初始化 → 传 3 片（其中一片 ≥5MiB）
// → ListParts → Complete → GetObject 校验内容与 MD5 一致。
func TestMultipartFullFlow(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("mp-bucket"); err != nil {
		t.Fatal(err)
	}

	big := bytes.Repeat([]byte("A"), 5*1024*1024) // 5MiB，满足最小分片
	parts := map[int][]byte{
		1: big,
		2: []byte("hello-"),
		3: []byte("world"),
	}
	var want bytes.Buffer
	for n := 1; n <= 3; n++ {
		want.Write(parts[n])
	}

	id, err := b.CreateMultipartUpload("mp-bucket", "deep/dir/big.bin", map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		t.Fatalf("CreateMultipartUpload 失败: %v", err)
	}

	// ListMultipartUploads 应能看到该未完成任务。
	lu, err := b.ListMultipartUploads("mp-bucket", nil, gofakes3.Prefix{HasPrefix: true, Prefix: "deep/"}, 1000)
	if err != nil {
		t.Fatalf("ListMultipartUploads 失败: %v", err)
	}
	if len(lu.Uploads) != 1 || lu.Uploads[0].UploadID != id {
		t.Fatalf("ListMultipartUploads = %+v，期望 1 个", lu.Uploads)
	}

	etags := map[int]string{}
	for n := 1; n <= 3; n++ {
		etag, err := b.UploadPart("mp-bucket", "deep/dir/big.bin", id, n, int64(len(parts[n])), bytes.NewReader(parts[n]))
		if err != nil {
			t.Fatalf("UploadPart(%d) 失败: %v", n, err)
		}
		etags[n] = etag
	}

	// ListParts 应看到 3 个分片且大小/ETag 正确。
	lp, err := b.ListParts("mp-bucket", "deep/dir/big.bin", id, 0, 1000)
	if err != nil {
		t.Fatalf("ListParts 失败: %v", err)
	}
	if len(lp.Parts) != 3 {
		t.Fatalf("ListParts 应 3 片，实际 %d", len(lp.Parts))
	}
	for _, p := range lp.Parts {
		if p.Size != int64(len(parts[p.PartNumber])) {
			t.Fatalf("分片 %d 大小 = %d，期望 %d", p.PartNumber, p.Size, len(parts[p.PartNumber]))
		}
		if p.ETag != etags[p.PartNumber] {
			t.Fatalf("分片 %d ETag = %q，期望 %q", p.PartNumber, p.ETag, etags[p.PartNumber])
		}
	}

	req := &gofakes3.CompleteMultipartUploadRequest{Parts: []gofakes3.CompletedPart{
		{PartNumber: 1, ETag: etags[1]},
		{PartNumber: 2, ETag: etags[2]},
		{PartNumber: 3, ETag: etags[3]},
	}}
	_, etag, err := b.CompleteMultipartUpload("mp-bucket", "deep/dir/big.bin", id, req)
	if err != nil {
		t.Fatalf("CompleteMultipartUpload 失败: %v", err)
	}
	sum := md5.Sum(want.Bytes())
	wantETag := `"` + hex.EncodeToString(sum[:]) + `"`
	if etag != wantETag {
		t.Fatalf("Complete ETag = %q，期望 %q", etag, wantETag)
	}

	obj, err := b.GetObject("mp-bucket", "deep/dir/big.bin", nil)
	if err != nil {
		t.Fatalf("GetObject 失败: %v", err)
	}
	got, _ := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("拼接内容不一致：got=%d bytes want=%d bytes", len(got), want.Len())
	}
	if `"`+hex.EncodeToString(obj.Hash)+`"` != wantETag {
		t.Fatalf("GetObject ETag 与 Complete 不一致")
	}

	// 完成后临时分片目录应被清理，且不再出现在未完成任务列表里。
	if _, err := os.Stat(b.uploadDir(string(id))); !os.IsNotExist(err) {
		t.Fatalf("完成后分片目录应被删除，实际 err=%v", err)
	}
	lu2, err := b.ListMultipartUploads("mp-bucket", nil, gofakes3.Prefix{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(lu2.Uploads) != 0 {
		t.Fatalf("完成后不应有未完成任务，实际 %+v", lu2.Uploads)
	}
	// 未完成的分片任务不能出现在 ListObjects 里。
	lo, err := b.ListBucket("mp-bucket", nil, gofakes3.ListBucketPage{MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(lo.Contents) != 1 || lo.Contents[0].Key != "deep/dir/big.bin" {
		t.Fatalf("ListObjects 应只含最终对象，实际 %+v", lo.Contents)
	}
}

// TestMultipartAbortCleansUp 校验 Abort 后无残留：ListObjects 看不到、
// 临时目录被清理、ListMultipartUploads 不再返回该任务。
func TestMultipartAbortCleansUp(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("abort-bucket"); err != nil {
		t.Fatal(err)
	}
	id, err := b.CreateMultipartUpload("abort-bucket", "temp.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.UploadPart("abort-bucket", "temp.bin", id, 1, 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if err := b.AbortMultipartUpload("abort-bucket", "temp.bin", id); err != nil {
		t.Fatalf("AbortMultipartUpload 失败: %v", err)
	}
	if _, err := os.Stat(b.uploadDir(string(id))); !os.IsNotExist(err) {
		t.Fatalf("Abort 后分片目录应被删除，实际 err=%v", err)
	}
	lo, err := b.ListBucket("abort-bucket", nil, gofakes3.ListBucketPage{MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(lo.Contents) != 0 {
		t.Fatalf("Abort 后 ListObjects 应为空，实际 %+v", lo.Contents)
	}
	lu, err := b.ListMultipartUploads("abort-bucket", nil, gofakes3.Prefix{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(lu.Uploads) != 0 {
		t.Fatalf("Abort 后不应有未完成任务，实际 %+v", lu.Uploads)
	}
}

// TestMultipartErrors 表驱动覆盖异常：ETag 不符 → InvalidPart；
// uploadId 不存在 → NoSuchUpload。
func TestMultipartErrors(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("err-bucket"); err != nil {
		t.Fatal(err)
	}
	id, err := b.CreateMultipartUpload("err-bucket", "x.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.UploadPart("err-bucket", "x.bin", id, 1, 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}

	t.Run("ETag 不符", func(t *testing.T) {
		req := &gofakes3.CompleteMultipartUploadRequest{Parts: []gofakes3.CompletedPart{
			{PartNumber: 1, ETag: `"deadbeef"`},
		}}
		_, _, err := b.CompleteMultipartUpload("err-bucket", "x.bin", id, req)
		if !gofakes3.HasErrorCode(err, gofakes3.ErrInvalidPart) {
			t.Fatalf("期望 InvalidPart，实际 %v", err)
		}
	})

	t.Run("uploadId 不存在", func(t *testing.T) {
		_, _, err := b.CompleteMultipartUpload("err-bucket", "x.bin", "no-such-id", &gofakes3.CompleteMultipartUploadRequest{
			Parts: []gofakes3.CompletedPart{{PartNumber: 1, ETag: `"x"`}},
		})
		if !gofakes3.HasErrorCode(err, gofakes3.ErrNoSuchUpload) {
			t.Fatalf("期望 NoSuchUpload，实际 %v", err)
		}
	})

	// 分片号乱序 → InvalidPartOrder
	t.Run("分片号乱序", func(t *testing.T) {
		if _, err := b.UploadPart("err-bucket", "x.bin", id, 2, 3, strings.NewReader("def")); err != nil {
			t.Fatal(err)
		}
		req := &gofakes3.CompleteMultipartUploadRequest{Parts: []gofakes3.CompletedPart{
			{PartNumber: 2, ETag: `"x"`},
			{PartNumber: 1, ETag: `"y"`},
		}}
		_, _, err := b.CompleteMultipartUpload("err-bucket", "x.bin", id, req)
		if !gofakes3.HasErrorCode(err, gofakes3.ErrInvalidPartOrder) {
			t.Fatalf("期望 InvalidPartOrder，实际 %v", err)
		}
	})
}

// TestMultipartCleanupExpired 启动清理：超过 24 小时未完成的任务被清理，未过期的保留。
func TestMultipartCleanupExpired(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("cleanup-bucket"); err != nil {
		t.Fatal(err)
	}
	oldID, err := b.CreateMultipartUpload("cleanup-bucket", "old.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	newID, err := b.CreateMultipartUpload("cleanup-bucket", "new.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 把 old 的 Initiated 改成 25 小时前。
	if _, ok, err := b.meta.UpdateUpload("cleanup-bucket", "old.bin", string(oldID), func(u *uploadMeta) error {
		u.Initiated = b.now().Add(-25 * time.Hour).UnixNano()
		return nil
	}); err != nil || !ok {
		t.Fatalf("改写 Initiated 失败: ok=%v err=%v", ok, err)
	}

	n, err := b.CleanupExpiredUploads(multipartExpiry)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应清理 1 个，实际 %d", n)
	}
	if _, ok, _ := b.meta.GetUpload("cleanup-bucket", "old.bin", string(oldID)); ok {
		t.Fatal("过期任务元数据应被清理")
	}
	if _, ok, _ := b.meta.GetUpload("cleanup-bucket", "new.bin", string(newID)); !ok {
		t.Fatal("未过期任务应保留")
	}
}

// TestMultipartTempOutsideBuckets 确认分片临时目录位于 .objbox 内部，不属于任何桶。
func TestMultipartTempOutsideBuckets(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	id, err := b.CreateMultipartUpload("bkt", "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.uploadDir(string(id)), filepath.Join(b.Root(), internalDirName)) {
		t.Fatalf("分片目录应在 %s 下，实际 %s", internalDirName, b.uploadDir(string(id)))
	}
}
