package backend

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// DefaultBucketFor：合法短名回退到 <name>-bucket；显式桶名原样返回；回退名也不合法时跳过建桶。
func TestDefaultBucketFor(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		wantAuto bool
		bucket   string
		auto     bool
		reason   bool
	}{
		{name: "demo", wantAuto: true, bucket: "demo", auto: true},
		{name: "ab", wantAuto: true, bucket: "ab-bucket", auto: true},
		{name: "a", wantAuto: true, bucket: "a-bucket", auto: true},
		{name: "demo", explicit: "team-bucket", wantAuto: true, bucket: "team-bucket", auto: true},
		{name: "demo", explicit: "keep-bucket", wantAuto: false, bucket: "keep-bucket", auto: false},
		{name: "demo", wantAuto: false, bucket: "demo", auto: false},
		// 环境变量/路径名这类不可能来自账号名的名字，回退也不合法 → 跳过建桶。
		{name: "UP", wantAuto: true, bucket: "UP", auto: false, reason: true},
		{name: "a_b", wantAuto: true, bucket: "a_b", auto: false, reason: true},
	}
	for _, c := range cases {
		bucket, auto, reason := DefaultBucketFor(c.name, c.explicit, c.wantAuto)
		if bucket != c.bucket || auto != c.auto {
			t.Fatalf("DefaultBucketFor(%q,%q,%v) = (%q,%v)，期望 (%q,%v)",
				c.name, c.explicit, c.wantAuto, bucket, auto, c.bucket, c.auto)
		}
		if (reason != "") != c.reason {
			t.Fatalf("DefaultBucketFor(%q,%q,%v) reason=%q，期望有原因=%v",
				c.name, c.explicit, c.wantAuto, reason, c.reason)
		}
	}
}

// CreateBucketAt / ListBucketNames / BucketStats / DeleteBucketAt 的 root 级行为。
func TestBucketAdminHelpers(t *testing.T) {
	root := t.TempDir()
	if err := CreateBucketAt(root, "alpha-bucket"); err != nil {
		t.Fatalf("建桶失败: %v", err)
	}
	if err := CreateBucketAt(root, "alpha-bucket"); !errors.Is(err, ErrBucketExists) {
		t.Fatalf("重复建桶应 ErrBucketExists，实际 %v", err)
	}
	if err := CreateBucketAt(root, "ab"); err == nil {
		t.Fatal("非法桶名应被拒绝")
	}

	if err := os.WriteFile(filepath.Join(root, "alpha-bucket", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	objects, bytes, err := BucketStats(root, "alpha-bucket")
	if err != nil {
		t.Fatal(err)
	}
	if objects != 1 || bytes != 5 {
		t.Fatalf("BucketStats = (%d,%d)，期望 (1,5)", objects, bytes)
	}

	names, err := ListBucketNames(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "alpha-bucket" {
		t.Fatalf("ListBucketNames = %v", names)
	}

	// 非空桶不可删。
	if err := DeleteBucketAt(root, "alpha-bucket"); !errors.Is(err, ErrBucketNotEmpty) {
		t.Fatalf("非空桶应 ErrBucketNotEmpty，实际 %v", err)
	}
	if err := os.Remove(filepath.Join(root, "alpha-bucket", "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := DeleteBucketAt(root, "alpha-bucket"); err != nil {
		t.Fatalf("空桶应可删: %v", err)
	}
	if err := DeleteBucketAt(root, "alpha-bucket"); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("不存在桶应 ErrBucketNotFound，实际 %v", err)
	}
}
