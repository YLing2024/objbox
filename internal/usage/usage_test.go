package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("123"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DirSize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != 8 {
		t.Fatalf("DirSize = %d，期望 8", got)
	}
}

func TestDirSizeMissing(t *testing.T) {
	got, err := DirSize(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("不存在的目录不应报错: %v", err)
	}
	if got != 0 {
		t.Fatalf("不存在目录大小应为 0，实际 %d", got)
	}
}

func TestCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("1234"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCache(time.Hour)
	if got, _ := c.DirSize(dir); got != 4 {
		t.Fatalf("首次 = %d，期望 4", got)
	}
	// 增加文件后命中缓存，大小不变
	if err := os.WriteFile(filepath.Join(dir, "b"), []byte("12"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.DirSize(dir); got != 4 {
		t.Fatalf("命中缓存 = %d，期望 4", got)
	}
}
