package backend

import (
	"io"
	"testing"

	"github.com/johannesboyne/gofakes3"
)

// TestDeleteMulti 批量删除：3 个 key（含 1 个不存在）→ Deleted 3 条、无 Error。
func TestDeleteMulti(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("del-bucket"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a.txt", "dir/b.txt", "c.txt"} {
		put(t, b, "del-bucket", k, "x")
	}

	res, err := b.DeleteMulti("del-bucket", "a.txt", "dir/b.txt", "not-exist.txt")
	if err != nil {
		t.Fatalf("DeleteMulti 失败: %v", err)
	}
	if len(res.Error) != 0 {
		t.Fatalf("不应有 Error，实际 %+v", res.Error)
	}
	if len(res.Deleted) != 3 {
		t.Fatalf("Deleted 应 3 条，实际 %d（%+v）", len(res.Deleted), res.Deleted)
	}

	lo, err := b.ListBucket("del-bucket", nil, gofakes3.ListBucketPage{MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(lo.Contents) != 1 || lo.Contents[0].Key != "c.txt" {
		t.Fatalf("删除后应只剩 c.txt，实际 %+v", lo.Contents)
	}
}

// TestDeleteMultiRejectsInvalidKey 非法 key 记入 Error，且不越过账号 root。
func TestDeleteMultiRejectsInvalidKey(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("del-bucket"); err != nil {
		t.Fatal(err)
	}
	res, err := b.DeleteMulti("del-bucket", "../escape")
	if err != nil {
		t.Fatalf("DeleteMulti 失败: %v", err)
	}
	if len(res.Error) != 1 {
		t.Fatalf("非法 key 应产生 1 条 Error，实际 %+v", res.Error)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("非法 key 不应出现在 Deleted，实际 %+v", res.Deleted)
	}
}

// TestCopyObject 复制后两份内容一致；源不存在 → NoSuchKey；0 字节与深层 key 均可用。
func TestCopyObject(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("copy-src"); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateBucket("copy-dst"); err != nil {
		t.Fatal(err)
	}

	t.Run("普通复制", func(t *testing.T) {
		put(t, b, "copy-src", "dir/hello.txt", "copy me")
		res, err := b.CopyObject("copy-src", "dir/hello.txt", "copy-dst", "deep/new/hello.txt", map[string]string{"Content-Type": "text/plain"})
		if err != nil {
			t.Fatalf("CopyObject 失败: %v", err)
		}
		if res.ETag != `"`+md5hex("copy me")+`"` {
			t.Fatalf("CopyObject ETag = %q", res.ETag)
		}
		obj, err := b.GetObject("copy-dst", "deep/new/hello.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(obj.Contents)
		obj.Contents.Close()
		if string(got) != "copy me" {
			t.Fatalf("复制内容 = %q", got)
		}
	})

	t.Run("0 字节", func(t *testing.T) {
		put(t, b, "copy-src", "empty", "")
		if _, err := b.CopyObject("copy-src", "empty", "copy-dst", "empty-copy", nil); err != nil {
			t.Fatalf("复制 0 字节对象失败: %v", err)
		}
		obj, err := b.GetObject("copy-dst", "empty-copy", nil)
		if err != nil {
			t.Fatal(err)
		}
		obj.Contents.Close()
		if obj.Size != 0 {
			t.Fatalf("复制后大小 = %d，期望 0", obj.Size)
		}
	})

	t.Run("源不存在", func(t *testing.T) {
		_, err := b.CopyObject("copy-src", "missing", "copy-dst", "x", nil)
		if !gofakes3.HasErrorCode(err, gofakes3.ErrNoSuchKey) {
			t.Fatalf("期望 NoSuchKey，实际 %v", err)
		}
	})

	t.Run("目标已存在则覆盖", func(t *testing.T) {
		put(t, b, "copy-src", "s", "new-content")
		put(t, b, "copy-dst", "d", "old-content")
		if _, err := b.CopyObject("copy-src", "s", "copy-dst", "d", nil); err != nil {
			t.Fatal(err)
		}
		obj, err := b.GetObject("copy-dst", "d", nil)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(obj.Contents)
		obj.Contents.Close()
		if string(got) != "new-content" {
			t.Fatalf("覆盖后内容 = %q", got)
		}
	})
}

// TestAccountUsageExcludesInternal 确认用量统计不包含 .objbox 内部目录。
func TestAccountUsageExcludesInternal(t *testing.T) {
	b := newBackend(t)
	if err := b.CreateBucket("usage-bucket"); err != nil {
		t.Fatal(err)
	}
	put(t, b, "usage-bucket", "a", "12345")
	// 制造一个分片临时文件，它不应计入用量。
	if _, err := b.CreateMultipartUpload("usage-bucket", "k", nil); err != nil {
		t.Fatal(err)
	}
	size, err := b.AccountUsage()
	if err != nil {
		t.Fatal(err)
	}
	if size != 5 {
		t.Fatalf("用量 = %d，期望 5（不含内部目录）", size)
	}
}
