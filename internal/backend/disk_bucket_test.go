package backend

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/johannesboyne/gofakes3"
)

// writeDisk 直接在 root 下预置文件，模拟管理员绕过 S3 协议、直接落盘数据。
func writeDisk(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("预置目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}
}

// TestDiscoverDiskPresetBuckets 覆盖「盘上已存在、未经 S3 协议创建」的桶发现：
// 启动前/运行期直接落盘的桶与对象都必须被 ListBuckets / ListBucket / GetObject 看到，
// 且内部目录（.objbox 等隐藏目录、临时文件）与非法桶名目录绝不能当桶暴露。
func TestDiscoverDiskPresetBuckets(t *testing.T) {
	root := t.TempDir()
	// 启动前预置：桶目录 + 对象文件，完全不经过 backend.New / PutObject。
	writeDisk(t, filepath.Join(root, "preset-bucket", "docs", "hello.txt"), "hello disk")

	b, err := New(root)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	// 启动重建索引应已把预置对象纳入（磁盘为准，索引可重建）。
	if md, ok, err := b.meta.Get("preset-bucket", "docs/hello.txt"); err != nil || !ok {
		t.Fatalf("启动后预置对象应进入元数据索引: ok=%v err=%v", ok, err)
	} else if md.ETag != md5hex("hello disk") {
		t.Fatalf("预置对象 ETag = %q，期望 %q", md.ETag, md5hex("hello disk"))
	}

	// 运行期再预置一个桶（后端已就绪，验证不是只在启动时做一次快照）。
	writeDisk(t, filepath.Join(root, "late-bucket", "x.txt"), "late")

	// 干扰项：隐藏内部目录、临时文件、非法桶名目录，均不得出现。
	if err := os.MkdirAll(filepath.Join(root, ".hidden-bucket"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, filepath.Join(root, tmpPrefix+"leftover"), "tmp")
	if err := os.MkdirAll(filepath.Join(root, "UP"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "-bad-name"), 0o700); err != nil {
		t.Fatal(err)
	}

	buckets, err := b.ListBuckets()
	if err != nil {
		t.Fatalf("ListBuckets 失败: %v", err)
	}
	got := make([]string, 0, len(buckets))
	for _, x := range buckets {
		got = append(got, x.Name)
	}
	want := []string{"late-bucket", "preset-bucket"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ListBuckets = %v，期望 %v（内部/非法目录不得出现）", got, want)
	}

	// 对象内容与 ETag 均以磁盘为准。
	obj, err := b.GetObject("preset-bucket", "docs/hello.txt", nil)
	if err != nil {
		t.Fatalf("GetObject 失败: %v", err)
	}
	body, _ := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if string(body) != "hello disk" {
		t.Fatalf("GetObject 内容 = %q，期望 %q", body, "hello disk")
	}

	lo, err := b.ListBucket("preset-bucket", nil, gofakes3.ListBucketPage{MaxKeys: 1000})
	if err != nil {
		t.Fatalf("ListBucket 失败: %v", err)
	}
	if len(lo.Contents) != 1 || lo.Contents[0].Key != "docs/hello.txt" {
		t.Fatalf("ListBucket = %+v", lo.Contents)
	}
	if lo.Contents[0].ETag != `"`+md5hex("hello disk")+`"` {
		t.Fatalf("ETag = %s，期望 %q", lo.Contents[0].ETag, `"`+md5hex("hello disk")+`"`)
	}
}

// TestDeleteBucketCountsOnlyObjects 验证「桶为空」只看对象：
//   - 内部临时文件不计入内容，桶内仅有它时视为空桶；
//   - 桶内确有对象 → BucketNotEmpty（合法业务错误，不是统一 403 语义）；
//   - 删空后删桶成功，且该桶的元数据记录被一并清理。
func TestDeleteBucketCountsOnlyObjects(t *testing.T) {
	root := t.TempDir()
	writeDisk(t, filepath.Join(root, "preset-bucket", "docs", "hello.txt"), "hello disk")
	b, err := New(root)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	// 非空桶：返回 BucketNotEmpty，且目录保留。
	if err := b.DeleteBucket("preset-bucket"); !gofakes3.HasErrorCode(err, gofakes3.ErrBucketNotEmpty) {
		t.Fatalf("非空桶删除应 BucketNotEmpty，实际 %v", err)
	}
	if fi, err := os.Stat(filepath.Join(root, "preset-bucket")); err != nil || !fi.IsDir() {
		t.Fatalf("非空桶不应被删除: err=%v", err)
	}

	// 只剩内部临时文件（模拟写入中断残留）时应视为空桶，可删除。
	writeDisk(t, filepath.Join(root, "preset-bucket", tmpPrefix+"crash"), "tmp")
	if _, err := b.DeleteObject("preset-bucket", "docs/hello.txt"); err != nil {
		t.Fatalf("DeleteObject 失败: %v", err)
	}
	if err := b.DeleteBucket("preset-bucket"); err != nil {
		t.Fatalf("只含内部临时文件的桶应可删: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "preset-bucket")); !os.IsNotExist(err) {
		t.Fatalf("桶目录应已删除: err=%v", err)
	}
	// 内部元数据目录在 root 级，删桶不应误删它；但该桶的元数据记录要被清理。
	if _, err := os.Stat(filepath.Join(root, internalDirName)); err != nil {
		t.Fatalf("内部元数据目录不应随桶删除: %v", err)
	}
	if _, ok, err := b.meta.Get("preset-bucket", "docs/hello.txt"); err != nil || ok {
		t.Fatalf("删桶后该桶元数据应被清理: ok=%v err=%v", ok, err)
	}
}
