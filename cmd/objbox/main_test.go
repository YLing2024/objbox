package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YLing2024/objbox/internal/account"
)

// setDefaultDataDir 覆盖默认数据目录，避免测试写入系统路径。
func setDefaultDataDir(t *testing.T, dir string) {
	t.Helper()
	old := defaultDataDir
	defaultDataDir = dir
	t.Cleanup(func() { defaultDataDir = old })
}

// §5.1：非法账号名（含把 -data 当名字）必须被拒、打印用法、非零退出且绝不建账号。
func TestAccountAddRejectsInvalidName(t *testing.T) {
	for _, name := range []string{"-data", "-foo", "Alpha", "a b", "../evil", ""} {
		t.Run(name, func(t *testing.T) {
			defDir := t.TempDir()
			setDefaultDataDir(t, defDir)
			explicit := t.TempDir()

			var out, errBuf bytes.Buffer
			code := run([]string{"account", "add", name, "-data", explicit}, &out, &errBuf)
			if code == 0 {
				t.Fatalf("非法账号名 %q 不应成功，stdout=%q", name, out.String())
			}
			if code != 2 {
				t.Fatalf("非法账号名 %q 退出码应为 2，实际 %d", name, code)
			}
			if !strings.Contains(errBuf.String(), "不合法") {
				t.Fatalf("应打印明确错误，实际 stderr=%q", errBuf.String())
			}
			if !strings.Contains(errBuf.String(), "objbox account add <name> [-note ...] [-readonly] [-bucket NAME] [-no-bucket] [-data DIR]") {
				t.Fatalf("应原样提示用法，实际 stderr=%q", errBuf.String())
			}
			// 绝不创建账号：默认目录与显式目录都不应出现账号表。
			for _, dir := range []string{defDir, explicit} {
				if _, err := os.Stat(filepath.Join(dir, account.FileName)); !os.IsNotExist(err) {
					t.Fatalf("非法名 %q 不应创建账号表（dir=%s, err=%v）", name, dir, err)
				}
			}
		})
	}
}

// §5.1：合法账号名应通过，且输出打印实际使用的数据目录。
func TestAccountAddValidName(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	code := run([]string{"account", "add", "demo", "-note", "示例", "-data", dir}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("合法账号名应成功，code=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "name:     demo") {
		t.Fatalf("成功输出应含账号名，实际 stdout=%q", out.String())
	}
	if !strings.Contains(out.String(), "data:     "+dir) {
		t.Fatalf("成功输出应含实际数据目录 %s，实际 stdout=%q", dir, out.String())
	}
	store, err := account.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Find("demo"); !ok {
		t.Fatalf("账号 demo 应写入 %s", dir)
	}
}

// §5.3：未显式指定 -data 时，输出里必须提示正在使用默认数据目录。
func TestAccountAddDefaultDataDirNotice(t *testing.T) {
	dir := t.TempDir()
	setDefaultDataDir(t, dir)

	var out, errBuf bytes.Buffer
	code := run([]string{"account", "add", "alpha"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("默认目录下建账号应成功，code=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "正在使用默认数据目录 "+dir) {
		t.Fatalf("应提示默认数据目录，实际 stderr=%q", errBuf.String())
	}
	if !strings.Contains(out.String(), "data:     "+dir) {
		t.Fatalf("成功输出应含数据目录，实际 stdout=%q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, account.FileName)); err != nil {
		t.Fatalf("账号表应写入默认数据目录 %s: %v", dir, err)
	}
}

// M5：account add 默认建桶，桶名 = 账号名。
func TestM5AccountAddCreatesDefaultBucket(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	code := run([]string{"account", "add", "demo", "-data", dir}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("应成功，code=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "bucket:   demo") {
		t.Fatalf("输出应含默认桶名，实际 stdout=%q", out.String())
	}
	if !strings.Contains(out.String(), "auto-create-bucket: true") {
		t.Fatalf("输出应含自动建桶开关，实际 stdout=%q", out.String())
	}
	if fi, err := os.Stat(filepath.Join(dir, "roots", "demo", "demo")); err != nil || !fi.IsDir() {
		t.Fatalf("默认桶目录应已创建: err=%v", err)
	}
	store, err := account.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := store.Find("demo")
	if !ok || a.Bucket != "demo" || !a.AutoCreateBucket {
		t.Fatalf("账号桶字段错误: %+v ok=%v", a, ok)
	}
}

// M5：-bucket NAME 指定默认桶名并建桶。
func TestM5AccountAddCustomBucket(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	code := run([]string{"account", "add", "demo", "-bucket", "my-bucket", "-data", dir}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("应成功，code=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "bucket:   my-bucket") {
		t.Fatalf("输出应含指定桶名，实际 stdout=%q", out.String())
	}
	if fi, err := os.Stat(filepath.Join(dir, "roots", "demo", "my-bucket")); err != nil || !fi.IsDir() {
		t.Fatalf("指定桶目录应已创建: err=%v", err)
	}
}

// M5：-no-bucket 不建桶，且 autoCreateBucket=false。
func TestM5AccountAddNoBucket(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	code := run([]string{"account", "add", "demo", "-no-bucket", "-data", dir}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("应成功，code=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "auto-create-bucket: false") {
		t.Fatalf("输出应显示关闭自动建桶，实际 stdout=%q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "roots", "demo", "demo")); !os.IsNotExist(err) {
		t.Fatalf("不应建桶，stat err=%v", err)
	}
	store, err := account.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := store.Find("demo")
	if a.AutoCreateBucket {
		t.Fatal("no-bucket 账号 AutoCreateBucket 应为 false")
	}
}

// M5：-bucket / -no-bucket / -data 等旗标绝不能被当成账号名。
func TestM5AccountAddFlagNotTreatedAsName(t *testing.T) {
	for _, name := range []string{"-bucket", "-no-bucket", "-data", "-readonly", "-note"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var out, errBuf bytes.Buffer
			code := run([]string{"account", "add", name, "-data", dir}, &out, &errBuf)
			if code != 2 {
				t.Fatalf("旗标被当账号名时应以 2 退出，实际 %d（stderr=%q）", code, errBuf.String())
			}
			if !strings.Contains(errBuf.String(), "不合法") {
				t.Fatalf("应提示账号名不合法，实际 stderr=%q", errBuf.String())
			}
			if _, err := os.Stat(filepath.Join(dir, account.FileName)); !os.IsNotExist(err) {
				t.Fatalf("不得创建账号表（旗标 %q）", name)
			}
		})
	}
}

// M5：account list 输出体现默认桶名与是否自动建桶，且不泄露明文 SK。
func TestM5AccountListShowsBucket(t *testing.T) {
	dir := t.TempDir()
	var addOut, addErr bytes.Buffer
	if code := run([]string{"account", "add", "demo", "-data", dir}, &addOut, &addErr); code != 0 {
		t.Fatalf("建账号失败: code=%d stderr=%q", code, addErr.String())
	}
	var out, errBuf bytes.Buffer
	if code := run([]string{"account", "list", "-data", dir}, &out, &errBuf); code != 0 {
		t.Fatalf("list 失败: code=%d stderr=%q", code, errBuf.String())
	}
	text := out.String()
	for _, want := range []string{"BUCKET", "AUTO-CREATE", "demo"} {
		if !strings.Contains(text, want) {
			t.Fatalf("list 输出应含 %q，实际=%q", want, text)
		}
	}
	// 默认（不 -show-secret）不得输出完整 SK。
	store, err := account.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := store.Find("demo")
	if strings.Contains(text, a.SK) {
		t.Fatalf("list 默认不得输出明文 SK: %q", text)
	}
}

// M5：非法 -bucket 名应被拒且不建账号。
func TestM5AccountAddInvalidBucketRejected(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	code := run([]string{"account", "add", "demo", "-bucket", "Bad_Bucket", "-data", dir}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("非法桶名应以 2 退出，实际 %d", code)
	}
	if !strings.Contains(errBuf.String(), "不合法") {
		t.Fatalf("应提示桶名不合法，实际 stderr=%q", errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, account.FileName)); !os.IsNotExist(err) {
		t.Fatal("非法桶名不应创建账号")
	}
}
