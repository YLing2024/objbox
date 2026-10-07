package backend

import (
	"os"
	"path/filepath"
	"testing"
)

// EnsureBucket 幂等：不存在则建，重复调用不报错，非法桶名照旧拒绝。
func TestEnsureBucketIdempotent(t *testing.T) {
	b := newBackend(t)

	if err := b.EnsureBucket("ensure-bucket"); err != nil {
		t.Fatalf("首次 EnsureBucket 失败: %v", err)
	}
	if ok, _ := b.BucketExists("ensure-bucket"); !ok {
		t.Fatal("EnsureBucket 后桶应存在")
	}
	if err := b.EnsureBucket("ensure-bucket"); err != nil {
		t.Fatalf("重复 EnsureBucket 应幂等成功: %v", err)
	}
	if err := b.EnsureBucket("ab"); err == nil {
		t.Fatal("非法桶名应被拒绝")
	}
}

// EnsureBucketAt 只应在给定 root 下建桶，且已存在时幂等。
func TestEnsureBucketAtStaysInRoot(t *testing.T) {
	root := t.TempDir()
	if err := EnsureBucketAt(root, "default-bucket"); err != nil {
		t.Fatalf("EnsureBucketAt 失败: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(root, "default-bucket")); err != nil || !fi.IsDir() {
		t.Fatalf("默认桶目录应存在: err=%v", err)
	}
	if err := EnsureBucketAt(root, "default-bucket"); err != nil {
		t.Fatalf("已存在应幂等: %v", err)
	}

	other := t.TempDir()
	if err := EnsureBucketAt(root, "../escape"); err == nil {
		t.Fatal("非法桶名 ../escape 应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(other, "escape")); err == nil {
		t.Fatal("不得在 root 之外创建任何目录")
	}
}

// BucketExistsInRoot 是管理面展示用的只读判断。
func TestBucketExistsInRoot(t *testing.T) {
	root := t.TempDir()
	if BucketExistsInRoot(root, "nope-bucket") {
		t.Fatal("不存在的桶应为 false")
	}
	if BucketExistsInRoot(root, "ab") {
		t.Fatal("非法桶名应为 false")
	}
	if err := EnsureBucketAt(root, "here-bucket"); err != nil {
		t.Fatal(err)
	}
	if !BucketExistsInRoot(root, "here-bucket") {
		t.Fatal("已建桶应为 true")
	}
}
