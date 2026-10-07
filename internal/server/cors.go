package server

import (
	"net/http"
	"strings"
)

// CORS 响应头常量。白名单命中时按需求原样回给浏览器。
const (
	corsAllowMethods = "OPTIONS, GET, HEAD, PUT, POST, DELETE"

	// corsAllowHeaders 必须覆盖 SigV4 会用到的请求头：x-amz-date、
	// x-amz-content-sha256、x-amz-meta-*、x-amz-copy-source 等。
	corsAllowHeaders = "Authorization, Content-Type, Content-MD5, Content-Length, " +
		"ETag, Range, If-Match, If-None-Match, " +
		"x-amz-date, x-amz-content-sha256, x-amz-security-token, " +
		"x-amz-meta-*, x-amz-copy-source, x-amz-*"

	// corsExposeHeaders 必须暴露 ETag，否则浏览器端拿不到校验 / 断点续传所需头。
	corsExposeHeaders = "ETag, Content-Length, Content-Type, Content-Range, " +
		"Accept-Ranges, Last-Modified, x-amz-*"

	corsMaxAge = "3600"
)

// isCORSPreflight 判断是否为浏览器预检：OPTIONS + Origin + Access-Control-Request-Method。
func isCORSPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions &&
		r.Header.Get("Origin") != "" &&
		r.Header.Get("Access-Control-Request-Method") != ""
}

// serveCORSPreflight 在签名校验之前处理预检请求。
//
//   - 来源命中白名单：直接回 204 并补齐 CORS 头，不做任何签名 / 认证，返回 true；
//   - 来源未命中（或未配置、非预检）：不额外回任何 CORS 头，返回 false，
//     交回正常流程，普通请求的签名校验 / 权限 / 桶策略一律不变。
//
// 生效范围仅 S3 API 路径；管理面在调用本函数前已路由处理。
func (s *Server) serveCORSPreflight(w http.ResponseWriter, r *http.Request) bool {
	if !isCORSPreflight(r) {
		return false
	}
	origin := s.corsOriginFor(r)
	if origin == "" {
		return false
	}
	setCORSHeaders(w.Header(), origin)
	addVaryOrigin(w.Header())
	w.WriteHeader(http.StatusNoContent)
	return true
}

// corsOriginFor 返回请求 Origin 命中白名单时的原值，否则空串。
func (s *Server) corsOriginFor(r *http.Request) string {
	if s.settings == nil {
		return ""
	}
	origin := r.Header.Get("Origin")
	if origin != "" && s.settings.Allows(origin) {
		return origin
	}
	return ""
}

// setCORSHeaders 写入命中白名单时的全部 CORS 响应头。
func setCORSHeaders(h http.Header, origin string) {
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Allow-Methods", corsAllowMethods)
	h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
	h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
	h.Set("Access-Control-Max-Age", corsMaxAge)
}

// stripCORSHeaders 清掉当前响应头里的所有 Access-Control-*（用于覆盖
// gofakes3 自带的那套通配 CORS 头）。
func stripCORSHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "Access-Control-") {
			h.Del(k)
		}
	}
}

// addVaryOrigin 保证响应带 Vary: Origin（已存在时不重复添加）。
func addVaryOrigin(h http.Header) {
	for _, v := range h.Values("Vary") {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "Origin") {
				return
			}
		}
	}
	h.Add("Vary", "Origin")
}
