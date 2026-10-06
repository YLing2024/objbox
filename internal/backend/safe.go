package backend

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// MaxKeyBytes 是对象 key 的最大字节数（S3 限制）。
	MaxKeyBytes = 1024

	// bucketMinLen / bucketMaxLen 是桶名长度限制。
	bucketMinLen = 3
	bucketMaxLen = 63
)

// bucketRe 是桶名允许的字符集合：小写字母、数字、点、连字符。
var bucketRe = regexp.MustCompile(`^[a-z0-9.-]+$`)

// PathError 表示一次路径/名称校验失败，Reason 说明原因。
type PathError struct {
	Reason string
}

func (e *PathError) Error() string { return e.Reason }

func pathErr(format string, args ...any) error {
	return &PathError{Reason: fmt.Sprintf(format, args...)}
}

// ValidateBucket 校验桶名：
//   - 长度 3～63；
//   - 仅含小写字母、数字、点、连字符；
//   - 不能以点或连字符开头/结尾；
//   - 不能包含 ".."（避免与路径上跳混淆）。
func ValidateBucket(bucket string) error {
	if bucket == "" {
		return pathErr("bucket 名为空")
	}
	if len(bucket) < bucketMinLen || len(bucket) > bucketMaxLen {
		return pathErr("bucket 名长度需在 %d～%d 之间，实际 %d", bucketMinLen, bucketMaxLen, len(bucket))
	}
	if !bucketRe.MatchString(bucket) {
		return pathErr("bucket 名 %q 含非法字符（只允许小写字母、数字、. 和 -）", bucket)
	}
	if bucket[0] == '.' || bucket[0] == '-' || bucket[len(bucket)-1] == '.' || bucket[len(bucket)-1] == '-' {
		return pathErr("bucket 名 %q 不能以 . 或 - 开头/结尾", bucket)
	}
	if strings.Contains(bucket, "..") {
		return pathErr("bucket 名 %q 不能包含 ..", bucket)
	}
	return nil
}

// ValidateKey 校验对象 key，拒绝一切可能的路径逃逸：
//   - 空 key、超过 1024 字节；
//   - 空字节、反斜杠；
//   - 以 "/" 开头的绝对路径、含 "//" 或任一空段；
//   - 任一段为 "." 或 ".."；
//   - 编码后的 "%2e"/"%2f"/"%5c"/"%00"（防二次编码绕过）。
func ValidateKey(key string) error {
	if key == "" {
		return pathErr("key 为空")
	}
	if len(key) > MaxKeyBytes {
		return pathErr("key 过长：%d 字节，上限 %d", len(key), MaxKeyBytes)
	}
	if strings.IndexByte(key, 0) >= 0 {
		return pathErr("key 含空字节")
	}
	if strings.Contains(key, "\\") {
		return pathErr("key 含反斜杠")
	}
	if strings.HasPrefix(key, "/") {
		return pathErr("key 不能以 / 开头（绝对路径）")
	}
	if strings.Contains(key, "//") {
		return pathErr("key 含连续 /")
	}

	lower := strings.ToLower(key)
	for _, bad := range []string{"%2e", "%2f", "%5c", "%00"} {
		if strings.Contains(lower, bad) {
			return pathErr("key 含编码后的危险序列 %q", bad)
		}
	}

	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "":
			return pathErr("key 含空路径段")
		case ".", "..":
			return pathErr("key 含路径段 %q", seg)
		}
	}
	return nil
}

// SafeJoin 是访问账号隔离空间内路径的唯一入口。
//
// 它只接受合法的 bucket 与 key，并把它们拼接到 root 之下，
// 最后再用 filepath.Rel 复核结果确实落在 root/<bucket> 之内。
// 任何失败都返回非 nil 错误，调用方应转成 400 InvalidRequest。
func SafeJoin(root, bucket, key string) (string, error) {
	if root == "" {
		return "", pathErr("root 为空")
	}
	if err := ValidateBucket(bucket); err != nil {
		return "", err
	}
	if err := ValidateKey(key); err != nil {
		return "", err
	}

	base := filepath.Join(root, bucket)
	full := filepath.Join(base, filepath.FromSlash(key))

	rel, err := filepath.Rel(base, full)
	if err != nil {
		return "", pathErr("无法计算相对路径: %v", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", pathErr("路径逃逸：%q 落在 bucket 根之外", key)
	}
	return full, nil
}
