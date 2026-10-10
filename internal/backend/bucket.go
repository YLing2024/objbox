package backend

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/YLing2024/objbox/internal/usage"
)

// 账号 root 级桶操作的哨兵错误，供管理面映射状态码。
var (
	// ErrBucketExists 表示目标桶已存在。
	ErrBucketExists = errors.New("bucket 已存在")
	// ErrBucketNotFound 表示目标桶不存在。
	ErrBucketNotFound = errors.New("bucket 不存在")
	// ErrBucketNotEmpty 表示桶内还有对象，不能删除。
	ErrBucketNotEmpty = errors.New("bucket 非空")
)

// isInternalEntry 判断账号 root 下的一个目录项是否为实现内部产物，
// 绝不能当作桶对外暴露：
//   - 点开头的隐藏目录（如 .objbox 元数据库/分片目录）；
//   - 写入用的临时文件前缀。
func isInternalEntry(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, tmpPrefix)
}

// discoverBucketDirs 以磁盘为事实来源，扫描账号 root 的顶层目录并返回合法桶目录。
//
// 这是「桶发现」的唯一规则：非目录、内部目录（.objbox 等隐藏目录与临时文件）、
// 以及不合规的桶名一律排除；结果按名称升序。启动重建索引（Reconcile）、
// ListBuckets 与管理面列举都复用它，保证三处口径一致。
//
// 有意不维护桶的内存/元数据索引：每次都以磁盘现状为准，从根本上避免
// 「盘上已存在但未经 S3 协议创建的桶」不可见的问题。root 不存在时返回空列表。
func discoverBucketDirs(root string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("backend: 读取 root 失败: %w", err)
	}
	out := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || isInternalEntry(e.Name()) || ValidateBucket(e.Name()) != nil {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// ListBucketNames 返回账号 root 下所有合法桶名（按名称排序）。
//
// 与 Backend.ListBuckets 同源规则（discoverBucketDirs）：跳过内部目录与不合法桶名，
// 但不打开 bbolt，供管理面在不占用账号元数据库的情况下只读列举。
func ListBucketNames(root string) ([]string, error) {
	entries, err := discoverBucketDirs(root)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

// CreateBucketAt 在账号 root 下新建一个桶。桶名照旧走 ValidateBucket；
// 已存在返回 ErrBucketExists，不覆盖已有目录。
func CreateBucketAt(root, name string) error {
	if root == "" {
		return errors.New("backend: root 不能为空")
	}
	if err := ValidateBucket(name); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(root, name), dirMode); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrBucketExists
		}
		return fmt.Errorf("backend: 创建桶失败: %w", err)
	}
	return nil
}

// DeleteBucketAt 删除账号 root 下的空桶。
//
// 仅当桶内没有任何对象文件（临时写入文件不计）时才删除，避免误删数据；
// 桶不存在返回 ErrBucketNotFound，非空返回 ErrBucketNotEmpty。
func DeleteBucketAt(root, name string) error {
	if root == "" {
		return errors.New("backend: root 不能为空")
	}
	if err := ValidateBucket(name); err != nil {
		return err
	}
	dir := filepath.Join(root, name)
	fi, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrBucketNotFound
		}
		return fmt.Errorf("backend: 读取桶状态失败: %w", err)
	}
	if !fi.IsDir() {
		return ErrBucketNotFound
	}
	empty, err := dirEmpty(dir)
	if err != nil {
		return err
	}
	if !empty {
		return ErrBucketNotEmpty
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("backend: 删除桶失败: %w", err)
	}
	return nil
}

// dirEmpty 判断目录内是否没有任何对象文件（忽略临时写入文件）。
func dirEmpty(dir string) (bool, error) {
	empty := true
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.HasPrefix(d.Name(), tmpPrefix) {
			empty = false
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return empty, err
}

// BucketStats 返回桶内对象数与占用字节数；走 internal/usage 的统一统计。
func BucketStats(root, name string) (objects int, bytes int64, err error) {
	if err := ValidateBucket(name); err != nil {
		return 0, 0, err
	}
	return usage.DirStats(filepath.Join(root, name))
}

// DefaultBucketFor 计算新建账号的默认桶名与是否自动建桶。
//
// explicit 非空表示用户显式指定桶名（其合法性由调用方先行校验并拒绝非法值）。
// 未显式指定且希望自动建桶时：优先用账号名；账号名不足 3 位等原因使其不是合法桶名时，
// 回退到 "<账号名>-bucket"；该回退名仍不合法则跳过建桶（autoCreate=false）并在 reason
// 里说明原因，绝不因此让建号失败。
func DefaultBucketFor(name, explicit string, wantAuto bool) (bucket string, autoCreate bool, reason string) {
	if explicit != "" {
		return explicit, wantAuto, ""
	}
	if !wantAuto {
		return name, false, ""
	}
	if ValidateBucket(name) == nil {
		return name, true, ""
	}
	alt := name + "-bucket"
	if ValidateBucket(alt) == nil {
		return alt, true, ""
	}
	return name, false, fmt.Sprintf("默认桶名 %q 与回退名 %q 均不合法，未自动建桶", name, alt)
}
