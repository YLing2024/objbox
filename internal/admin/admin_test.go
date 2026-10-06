package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	s, err := New(t.TempDir(), ModeBuiltin)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatalf("响应中没有 %s cookie", CookieName)
	return nil
}

// §5b.7：HTTPS（TLS 或 X-Forwarded-Proto: https）下会话 cookie 带 Secure，
// 本地 http 调试不带。
func TestSessionCookieSecure(t *testing.T) {
	s := newTestService(t)

	xffReq := httptest.NewRequest(http.MethodPost, "http://example.com/api/admin/login", nil)
	xffReq.Header.Set("X-Forwarded-Proto", "https")
	xffMixed := httptest.NewRequest(http.MethodPost, "http://example.com/api/admin/login", nil)
	xffMixed.Header.Set("X-Forwarded-Proto", "https, http")

	cases := []struct {
		name string
		req  *http.Request
		want bool
	}{
		{"local-http", httptest.NewRequest(http.MethodPost, "http://example.com/api/admin/login", nil), false},
		{"direct-tls", httptest.NewRequest(http.MethodPost, "https://example.com/api/admin/login", nil), true},
		{"xff-https", xffReq, true},
		{"xff-mixed-first-https", xffMixed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.SetSessionCookie(rec, tc.req, "admin")
			c := sessionCookie(t, rec)
			if c.Secure != tc.want {
				t.Fatalf("Secure = %v，期望 %v", c.Secure, tc.want)
			}
			if !c.HttpOnly {
				t.Fatal("会话 cookie 必须 HttpOnly")
			}
		})
	}
}

// §5b.4：InvalidateSessions 递增版本号，使旧会话 token 失效。
func TestInvalidateSessions(t *testing.T) {
	s := newTestService(t)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/", nil)
	rec := httptest.NewRecorder()
	s.SetSessionCookie(rec, req, "admin")
	token := sessionCookie(t, rec).Value

	if user, ok := s.verifySession(token); !ok || user != "admin" {
		t.Fatalf("刚签发的会话应有效，user=%q ok=%v", user, ok)
	}
	if err := s.InvalidateSessions(); err != nil {
		t.Fatalf("InvalidateSessions 失败: %v", err)
	}
	if _, ok := s.verifySession(token); ok {
		t.Fatal("会话版本号提升后旧 token 必须失效")
	}
}
