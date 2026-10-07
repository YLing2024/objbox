package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	valid := map[string]string{
		"https://app.example.com":     "https://app.example.com",
		"http://127.0.0.1:5173":       "http://127.0.0.1:5173",
		"  https://app.example.com  ": "https://app.example.com",
		"https://app.example.com/":    "https://app.example.com",
		"HTTPS://app.example.com":     "https://app.example.com",
		"http://localhost:8080":       "http://localhost:8080",
	}
	for in, want := range valid {
		got, err := Normalize(in)
		if err != nil {
			t.Fatalf("Normalize(%q) 意外报错: %v", in, err)
		}
		if got != want {
			t.Fatalf("Normalize(%q) = %q，期望 %q", in, got, want)
		}
	}

	invalid := []string{
		"",
		"app.example.com",
		"ftp://app.example.com",
		"https://",
		"https://app.example.com/path",
		"https://app.example.com?x=1",
		"https://user:pass@app.example.com",
		"https://app.example.com:0",
		"https://app.example.com:70000",
		"https://app.example.com:abc",
	}
	for _, in := range invalid {
		if _, err := Normalize(in); err == nil {
			t.Fatalf("Normalize(%q) 应报错", in)
		}
	}
}

func TestNormalizeAllLineNumber(t *testing.T) {
	_, err := NormalizeAll([]string{"https://ok.example.com", "ftp://bad.example.com"})
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "第 2 行") {
		t.Fatalf("错误应指明第 2 行: %v", err)
	}
}

func TestEnvDefaultWhenNoFile(t *testing.T) {
	t.Setenv(EnvCORSOrigins, "https://a.example.com, https://b.example.com")
	s, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := s.Origins()
	if len(got) != 2 || got[0] != "https://a.example.com" || got[1] != "https://b.example.com" {
		t.Fatalf("环境变量默认值错误: %v", got)
	}
	if s.Source() != SourceEnv {
		t.Fatalf("来源应为 env，实际 %s", s.Source())
	}
	if !s.Allows("https://a.example.com") || s.Allows("https://c.example.com") {
		t.Fatal("Allows 判定错误")
	}
}

func TestFileOverridesEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCORSOrigins, "https://env.example.com")
	body := `{"version":1,"corsOrigins":["https://file.example.com"]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Origins()
	if len(got) != 1 || got[0] != "https://file.example.com" {
		t.Fatalf("设置文件应覆盖环境变量: %v", got)
	}
	if s.Source() != SourceSettings {
		t.Fatalf("来源应为 settings，实际 %s", s.Source())
	}
}

func TestExplicitEmptyDisablesEvenWithEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCORSOrigins, "https://env.example.com")
	body := `{"version":1,"corsOrigins":[]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Origins()) != 0 {
		t.Fatalf("显式空列表应关闭跨域: %v", s.Origins())
	}
	if s.Source() != SourceSettings {
		t.Fatalf("来源应为 settings，实际 %s", s.Source())
	}
}

func TestSetPersistsAndOverridesEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCORSOrigins, "https://env.example.com")
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCORSOrigins([]string{"https://saved.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Origins(); len(got) != 1 || got[0] != "https://saved.example.com" {
		t.Fatalf("保存后内存未更新: %v", got)
	}
	// 重新加载（等价另一进程视角）。
	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Origins(); len(got) != 1 || got[0] != "https://saved.example.com" {
		t.Fatalf("落盘未生效: %v", got)
	}
	// 文件权限 0600。
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != FileMode {
		t.Fatalf("文件权限应为 %o，实际 %o", FileMode, fi.Mode().Perm())
	}
	// 无残留临时文件。
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".settings-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
}

func TestSetValidationKeepsOldValue(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCORSOrigins([]string{"https://ok.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCORSOrigins([]string{"https://ok.example.com", "not-a-url"}); err == nil {
		t.Fatal("非法来源应报错")
	} else if !strings.Contains(err.Error(), "第 2 行") {
		t.Fatalf("应指明第 2 行: %v", err)
	}
	if got := s.Origins(); len(got) != 1 || got[0] != "https://ok.example.com" {
		t.Fatalf("校验失败不应修改旧值: %v", got)
	}
}

func TestCorruptFileQuarantined(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCORSOrigins, "https://env.example.com")
	orig := []byte("{ this is not json")
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("损坏文件不应导致 Load 失败: %v", err)
	}
	if got := s.Origins(); len(got) != 1 || got[0] != "https://env.example.com" {
		t.Fatalf("损坏文件应回退环境变量默认值: %v", got)
	}
	bak := path + ".bak"
	raw, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("应保留 .bak: %v", err)
	}
	if string(raw) != string(orig) {
		t.Fatalf(".bak 内容应与原文件一致: %q", raw)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("原设置文件应已移走: %v", err)
	}
}

func TestCorruptOriginInFileQuarantined(t *testing.T) {
	dir := t.TempDir()
	body := `{"version":1,"corsOrigins":["ftp://bad.example.com"]}`
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Origins()) != 0 {
		t.Fatalf("含非法来源的文件应视为损坏并回退默认: %v", s.Origins())
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("应保留 .bak: %v", err)
	}
}

// 写盘失败时必须返回错误且内存值保持不变（原子性：要么整体成功要么不变）。
func TestSetWriteFailureKeepsMemory(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCORSOrigins([]string{"https://ok.example.com"}); err != nil {
		t.Fatal(err)
	}
	// 用一个普通文件充当父目录，使 MkdirAll/创建临时文件失败。
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(blocker, FileName)
	if err := s.SetCORSOrigins([]string{"https://new.example.com"}); err == nil {
		t.Fatal("写盘失败应报错")
	}
	if got := s.Origins(); len(got) != 1 || got[0] != "https://ok.example.com" {
		t.Fatalf("写盘失败不应修改内存: %v", got)
	}
}

func TestLoadFileIsValidJSONAfterSave(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCORSOrigins([]string{"https://a.example.com", "http://127.0.0.1:5173"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("保存后的文件应为合法 JSON: %v", err)
	}
	if f.CORSOrigins == nil || len(*f.CORSOrigins) != 2 {
		t.Fatalf("保存内容错误: %s", raw)
	}
}
