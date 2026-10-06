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
			if !strings.Contains(errBuf.String(), "objbox account add <name> [-note ...] [-readonly] [-data DIR]") {
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
