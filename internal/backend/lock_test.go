package backend

import (
	"strings"
	"testing"
)

// §5b.5：同一数据目录起第二个实例应给出可读的“目录已被占用”错误，而非裸 timeout。
func TestNewDataDirInUseError(t *testing.T) {
	root := t.TempDir()
	b, err := New(root)
	if err != nil {
		t.Fatalf("首个后端应打开成功: %v", err)
	}
	defer b.Close()

	if _, err := New(root); err == nil {
		t.Fatal("同一目录起第二个后端应报错")
	} else if !strings.Contains(err.Error(), "已被另一个 objbox 进程占用") {
		t.Fatalf("错误应可读地提示目录被占用，实际: %v", err)
	}
}
