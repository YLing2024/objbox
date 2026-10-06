// Package randstr 提供随机串生成能力，用于生成 AK/SK、uploadId 等。
//
// 所有随机数均来自 crypto/rand，不用于可预测场景。
package randstr

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"fmt"
)

// base32UpperNoPad 为无填充的大写 base32 编码，AK 生成使用。
var base32UpperNoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// base64URLNoPad 为无填充的 base64url 编码，SK 生成使用。
var base64URLNoPad = base64.RawURLEncoding

// Bytes 返回 n 字节的加密安全随机数据。
func Bytes(n int) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("randstr: 长度必须为正数，实际为 %d", n)
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("randstr: 读取随机数失败: %w", err)
	}
	return b, nil
}

// Base32Upper 返回 n 字节随机数据的大写 base32 编码（无填充）。
//
// n 字节的 base32 编码长度为 ceil(8n/5) 个字符。
func Base32Upper(n int) (string, error) {
	b, err := Bytes(n)
	if err != nil {
		return "", err
	}
	return base32UpperNoPad.EncodeToString(b), nil
}

// Token 返回 n 字节随机数据的 base64url 编码（无填充）。
func Token(n int) (string, error) {
	b, err := Bytes(n)
	if err != nil {
		return "", err
	}
	return base64URLNoPad.EncodeToString(b), nil
}
