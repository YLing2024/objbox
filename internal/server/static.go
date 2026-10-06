package server

import (
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/YLing2024/objbox/web"
)

// adminAssets 指向内嵌的 dist 目录。
var adminAssets = mustSub(web.Dist, "dist")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("server: 内嵌前端资源缺失: " + err.Error())
	}
	return sub
}

// serveAdminStatic 处理管理页静态资源，命中返回 true。
//
// 带 S3 认证（Authorization 头或预签名 query）的请求一律交回协议层，
// 避免把名为 assets / index.html 之类的桶请求误当成静态资源。
func (s *Server) serveAdminStatic(w http.ResponseWriter, r *http.Request) bool {
	if hasS3Auth(r) {
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/assets/") {
		s.serveEmbedded(w, r, strings.TrimPrefix(p, "/"))
		return true
	}
	if p == "/index.html" || (p == "/" && wantsHTML(r)) {
		s.serveAdminIndex(w, r)
		return true
	}
	return false
}

// serveAdminIndex 返回管理页 HTML，并把 AUTH_MODE 注入页面供前端选择登录/未认证视图。
func (s *Server) serveAdminIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	data, err := fs.ReadFile(adminAssets, "index.html")
	if err != nil {
		http.Error(w, "管理页未构建", http.StatusInternalServerError)
		return
	}
	mode := "builtin"
	if s.admin != nil {
		mode = s.admin.AuthMode()
	}
	html := strings.ReplaceAll(string(data), "__OBJBOX_AUTH_MODE_VALUE__", mode)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(html)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, html)
}

// serveEmbedded 从内嵌资源里取文件；资源名带内容哈希，可长缓存。
func (s *Server) serveEmbedded(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+name), "/")
	if !strings.HasPrefix(rel, "assets/") {
		http.NotFound(w, r)
		return
	}
	f, err := adminAssets.Open(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	ctype := mime.TypeByExtension(path.Ext(rel))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

// wantsHTML 判断客户端是否期望 HTML。
//
// Go 默认客户端不发 Accept 头，因此匿名 GET / 仍走 S3 协议层（403），
// 保留 M0「匿名请求一律 403」的语义；浏览器与 curl 默认 Accept 含 */*，
// 会拿到管理页 HTML。
func wantsHTML(r *http.Request) bool {
	a := r.Header.Get("Accept")
	if a == "" {
		return false
	}
	return strings.Contains(a, "text/html") ||
		strings.Contains(a, "application/xhtml") ||
		strings.Contains(a, "*/*")
}

func hasS3Auth(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || r.URL.Query().Get("X-Amz-Signature") != ""
}
