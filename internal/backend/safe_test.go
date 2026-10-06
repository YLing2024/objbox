package backend

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSafeJoinRejectsEscapes 是需求 §4.2 的路径逃逸表：每条都必须被拒绝。
func TestSafeJoinRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		key  string
	}{
		{"相对上跳", "../etc/passwd"},
		{"编码斜杠上跳", "..%2f..%2fetc"},
		{"编码点号", "%2e%2e/"},
		{"中段上跳", "a/../../b"},
		{"反斜杠上跳", `\..\..\x`},
		{"绝对路径", "/abs/path"},
		{"空字节", "key\x00.txt"},
		{"连续斜杠", "a//b"},
		{"当前目录段", "a/./b"},
		{"空段结尾", "a/"},
		{"编码反斜杠", "a%5cb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := SafeJoin(root, "bucket", c.key); err == nil {
				t.Fatalf("key %q 应被拒绝", c.key)
			}
		})
	}
}

func TestSafeJoinAcceptsNormalKeys(t *testing.T) {
	root := t.TempDir()
	cases := []string{
		"a.txt",
		"dir/sub/file.bin",
		"a-b_c.d/e",
		"中文/目录/文件.txt",
	}
	for _, key := range cases {
		full, err := SafeJoin(root, "bucket", key)
		if err != nil {
			t.Fatalf("key %q 应被接受，却报错 %v", key, err)
		}
		want := filepath.Join(root, "bucket", filepath.FromSlash(key))
		if full != want {
			t.Fatalf("SafeJoin(%q) = %q，期望 %q", key, full, want)
		}
	}
}

func TestSafeJoinResultStaysInBucket(t *testing.T) {
	root := t.TempDir()
	full, err := SafeJoin(root, "bucket", "dir/sub/file")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "bucket")
	if !strings.HasPrefix(full, base+string(filepath.Separator)) {
		t.Fatalf("结果 %q 未落在 %q 之内", full, base)
	}
}

func TestValidateBucket(t *testing.T) {
	for _, ok := range []string{"abc", "a-b.c", "bucket-123", "aaa"} {
		if err := ValidateBucket(ok); err != nil {
			t.Fatalf("bucket %q 应合法，却报错 %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ab", "-abc", "abc-", ".abc", "abc.", "AB", "a_b", "a..b", strings.Repeat("x", 64)} {
		if err := ValidateBucket(bad); err == nil {
			t.Fatalf("bucket %q 应非法", bad)
		}
	}
}

func TestValidateKeyTooLong(t *testing.T) {
	key := strings.Repeat("a", MaxKeyBytes+1)
	if err := ValidateKey(key); err == nil {
		t.Fatal("超长 key 应被拒绝")
	}
	if err := ValidateKey(strings.Repeat("a", MaxKeyBytes)); err != nil {
		t.Fatalf("上限长度 key 应被接受: %v", err)
	}
}
