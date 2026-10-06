package randstr

import (
	"strings"
	"testing"
)

func TestBase32Upper(t *testing.T) {
	got, err := Base32Upper(20)
	if err != nil {
		t.Fatalf("Base32Upper 返回错误: %v", err)
	}
	// 20 字节 -> 32 个 base32 字符
	if len(got) != 32 {
		t.Fatalf("Base32Upper 长度 = %d，期望 32", len(got))
	}
	if got != strings.ToUpper(got) {
		t.Fatalf("Base32Upper 应为大写，实际 %q", got)
	}
	if strings.ContainsAny(got, "=") {
		t.Fatalf("Base32Upper 不应包含填充，实际 %q", got)
	}
}

func TestToken(t *testing.T) {
	got, err := Token(32)
	if err != nil {
		t.Fatalf("Token 返回错误: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Token 结果为空")
	}
	if strings.ContainsAny(got, "+/=") {
		t.Fatalf("base64url 无填充不应包含 +、/、=，实际 %q", got)
	}
}

func TestUnique(t *testing.T) {
	a, err := Token(32)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Token(32)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("两次生成的随机串不应相同")
	}
}

func TestBytesRejectsNonPositive(t *testing.T) {
	for _, n := range []int{0, -1} {
		if _, err := Bytes(n); err == nil {
			t.Fatalf("Bytes(%d) 应返回错误", n)
		}
	}
}
