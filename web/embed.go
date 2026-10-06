// Package web 内嵌前端构建产物（web/dist）。
package web

import "embed"

// Dist 是 Vite 构建产物，由 Makefile 的 build 目标先执行 npm run build 生成。
// 使用 all: 前缀以便把 assets 下带哈希的文件一并打进二进制。
//
//go:embed all:dist
var Dist embed.FS
