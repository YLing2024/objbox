package account

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	return s, dir
}

func TestAddGeneratesAKSK(t *testing.T) {
	s, dir := newStore(t)

	a, err := s.Add("demo", "示例账号", false)
	if err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	if !strings.HasPrefix(a.AK, AKPrefix) {
		t.Fatalf("AK 应以 %q 开头，实际 %q", AKPrefix, a.AK)
	}
	if n := len(a.AK); n < 32 || n > 40 {
		t.Fatalf("AK 长度应在 32～40，实际 %d (%q)", n, a.AK)
	}
	if len(a.SK) == 0 {
		t.Fatal("SK 不应为空")
	}
	wantRoot := filepath.Join(dir, "roots", "demo")
	if a.Root != wantRoot {
		t.Fatalf("root 推导错误，得到 %q，期望 %q", a.Root, wantRoot)
	}
	if fi, err := os.Stat(a.Root); err != nil || !fi.IsDir() {
		t.Fatalf("root 目录应已创建: err=%v", err)
	}

	// 文件权限必须为 0600
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("账号表应存在: %v", err)
	}
	if fi.Mode().Perm() != FileMode {
		t.Fatalf("账号表权限 = %v，期望 %v", fi.Mode().Perm(), FileMode)
	}
}

func TestAddDuplicateName(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Add("demo", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("demo", "", false); err == nil {
		t.Fatal("重复账号名应报错")
	}
}

func TestAddInvalidName(t *testing.T) {
	s, _ := newStore(t)
	for _, name := range []string{"", "../evil", "a/b", "有中文", strings.Repeat("x", 65)} {
		if _, err := s.Add(name, "", false); err == nil {
			t.Fatalf("非法账号名 %q 应报错", name)
		}
	}
}

func TestLoadRoundTripAndUniqueness(t *testing.T) {
	s, dir := newStore(t)
	a1, err := s.Add("one", "", false)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.Add("two", "", true)
	if err != nil {
		t.Fatal(err)
	}

	re, err := Load(dir)
	if err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	got, ok := re.GetByAK(a1.AK)
	if !ok || got.Name != "one" {
		t.Fatalf("AK 索引重建失败: %+v ok=%v", got, ok)
	}
	got2, ok := re.Find("two")
	if !ok || !got2.Readonly || got2.AK != a2.AK {
		t.Fatalf("账号二重建失败: %+v ok=%v", got2, ok)
	}
}

func TestLoadRejectsDuplicateAK(t *testing.T) {
	dir := t.TempDir()
	raw := `{"version":1,"accounts":[
		{"name":"a","ak":"AKAAAA","sk":"s1","root":""},
		{"name":"b","ak":"AKAAAA","sk":"s2","root":""}
	]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(raw), FileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("重复 AK 应被拒绝")
	}
}

func TestRotateInvalidatesOldSK(t *testing.T) {
	s, _ := newStore(t)
	a, err := s.Add("demo", "", false)
	if err != nil {
		t.Fatal(err)
	}
	newSK, err := s.Rotate("demo")
	if err != nil {
		t.Fatalf("Rotate 失败: %v", err)
	}
	if newSK == a.SK {
		t.Fatal("轮换后 SK 应变化")
	}
	cur, _ := s.Find("demo")
	if cur.SK != newSK {
		t.Fatal("内存中的 SK 未更新")
	}
	// 旧 AK 不变，SK 更新
	if cur.AK != a.AK {
		t.Fatal("轮换不应改变 AK")
	}
}

func TestDisableEnableRemove(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Add("demo", "", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled("demo", true); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Find("demo"); !a.Disabled {
		t.Fatal("账号应已停用")
	}
	if err := s.SetDisabled("demo", false); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Find("demo"); a.Disabled {
		t.Fatal("账号应已启用")
	}
	if err := s.Remove("demo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Find("demo"); ok {
		t.Fatal("账号应已移除")
	}
	if err := s.Remove("demo"); err == nil {
		t.Fatal("移除不存在的账号应报错")
	}
}

func TestMaskSecret(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"SKEXAMPLE32BYTESRANDOMSTRINGVALUE0001", "SKEX****0001"},
		{"short", "*****"},
		{"", ""},
	}
	for _, c := range cases {
		if got := MaskSecret(c.in); got != c.want {
			t.Fatalf("MaskSecret(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// M5：旧账号表（无 autoCreateBucket / bucket 字段）加载后按缺省值生效。
func TestLoadOldAccountsAppliesM5Defaults(t *testing.T) {
	dir := t.TempDir()
	raw := `{"version":1,"accounts":[
		{"name":"legacy","ak":"AKLEGACY0001","sk":"s1","root":""}
	]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(raw), FileMode); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("旧账号表应能加载: %v", err)
	}
	a, ok := s.Find("legacy")
	if !ok {
		t.Fatal("应能找到 legacy 账号")
	}
	if !a.AutoCreateBucket {
		t.Fatal("旧表缺省 autoCreateBucket 应为 true")
	}
	if a.Bucket != "legacy" {
		t.Fatalf("旧表缺省 bucket 应为账号名，实际 %q", a.Bucket)
	}
}

// M5：显式 false / 指定桶名应被记录，且落盘后可再次读取。
func TestAddAccountM5FieldsRoundTrip(t *testing.T) {
	s, dir := newStore(t)

	no := false
	a, err := s.AddAccount("manual", "", false, AddOptions{Bucket: "my-bucket", AutoCreateBucket: &no})
	if err != nil {
		t.Fatalf("AddAccount 失败: %v", err)
	}
	if a.AutoCreateBucket {
		t.Fatal("显式 false 时 AutoCreateBucket 应为 false")
	}
	if a.Bucket != "my-bucket" {
		t.Fatalf("bucket = %q，期望 my-bucket", a.Bucket)
	}

	// 默认账号：自动建桶、桶名 = 账号名。
	b, err := s.AddAccount("auto", "", false, AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !b.AutoCreateBucket || b.Bucket != "auto" {
		t.Fatalf("默认应为 autoCreateBucket=true bucket=auto，实际 %+v", b)
	}

	// 显式落盘：文件里必须能看到两个字段。
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"autoCreateBucket"`, `"bucket"`, `"my-bucket"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("账号表应显式包含 %s，实际：%s", want, raw)
		}
	}

	re, err := Load(dir)
	if err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	got, ok := re.Find("manual")
	if !ok || got.AutoCreateBucket || got.Bucket != "my-bucket" {
		t.Fatalf("再读取字段不一致: %+v ok=%v", got, ok)
	}
}
