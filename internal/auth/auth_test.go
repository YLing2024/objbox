package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/YLing2024/objbox/internal/account"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const testRegion = "us-east-1"

type fixture struct {
	auth     *Authenticator
	writable *account.Account
	readonly *account.Account
	disabled *account.Account
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := account.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writable, err := store.Add("writer", "", false)
	if err != nil {
		t.Fatal(err)
	}
	readonly, err := store.Add("reader", "", true)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := store.Add("disabled", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDisabled("disabled", true); err != nil {
		t.Fatal(err)
	}
	return &fixture{
		auth:     NewAuthenticator(store),
		writable: writable,
		readonly: readonly,
		disabled: disabled,
	}
}

func signRequest(t *testing.T, method, target string, body []byte, acct *account.Account, at time.Time) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if body != nil {
		r.Header.Set("Content-Type", "application/octet-stream")
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	r.Header.Set("x-amz-content-sha256", payloadHash)

	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: acct.AK, SecretAccessKey: acct.SK}
	if err := signer.SignHTTP(context.Background(), creds, r, payloadHash, Service, testRegion, at); err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	return r
}

func codeOf(t *testing.T, err error) Code {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("期望 *auth.Error，实际 %v", err)
	}
	return e.Code
}

func TestAuthenticateValid(t *testing.T) {
	f := newFixture(t)
	r := signRequest(t, http.MethodGet, "http://example.com/bucket/key", nil, f.writable, time.Now())
	acct, err := f.auth.Authenticate(r)
	if err != nil {
		t.Fatalf("合法签名应通过，实际 %v", err)
	}
	if acct.Name != f.writable.Name {
		t.Fatalf("解析账号 = %q，期望 %q", acct.Name, f.writable.Name)
	}
}

func TestAuthenticateWrongSK(t *testing.T) {
	f := newFixture(t)
	bad := *f.writable
	bad.SK = "wrong-secret-key"
	r := signRequest(t, http.MethodGet, "http://example.com/bucket/key", nil, &bad, time.Now())
	_, err := f.auth.Authenticate(r)
	if got := codeOf(t, err); got != CodeSignatureDoesNotMatch {
		t.Fatalf("错误码 = %q，期望 %q", got, CodeSignatureDoesNotMatch)
	}
}

func TestAuthenticateTimeSkew(t *testing.T) {
	f := newFixture(t)
	r := signRequest(t, http.MethodGet, "http://example.com/bucket/key", nil, f.writable, time.Now().Add(-20*time.Minute))
	_, err := f.auth.Authenticate(r)
	if got := codeOf(t, err); got != CodeRequestTimeTooSkewed {
		t.Fatalf("错误码 = %q，期望 %q", got, CodeRequestTimeTooSkewed)
	}
}

func TestAuthenticateTamperedPayloadHash(t *testing.T) {
	f := newFixture(t)
	body := []byte("original")
	r := signRequest(t, http.MethodPut, "http://example.com/bucket/key", body, f.writable, time.Now())
	// 篡改载荷哈希但不重新签名，签名应失效。
	tampered := sha256.Sum256([]byte("tampered"))
	r.Header.Set("x-amz-content-sha256", hex.EncodeToString(tampered[:]))
	_, err := f.auth.Authenticate(r)
	if got := codeOf(t, err); got != CodeSignatureDoesNotMatch {
		t.Fatalf("错误码 = %q，期望 %q", got, CodeSignatureDoesNotMatch)
	}
}

func TestAuthenticateMissingOrUnknown(t *testing.T) {
	f := newFixture(t)

	plain := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	if _, err := f.auth.Authenticate(plain); codeOf(t, err) != CodeAccessDenied {
		t.Fatal("无 Authorization 应为 AccessDenied")
	}

	unknown := *f.writable
	unknown.AK = "AKUNKNOWN"
	r := signRequest(t, http.MethodGet, "http://example.com/", nil, &unknown, time.Now())
	if _, err := f.auth.Authenticate(r); codeOf(t, err) != CodeAccessDenied {
		t.Fatal("未知 AK 应为 AccessDenied")
	}
}

func TestAuthenticateDisabled(t *testing.T) {
	f := newFixture(t)
	r := signRequest(t, http.MethodGet, "http://example.com/bucket/key", nil, f.disabled, time.Now())
	if _, err := f.auth.Authenticate(r); codeOf(t, err) != CodeAccessDenied {
		t.Fatal("停用账号应为 AccessDenied")
	}
}

func TestAuthenticateReadonly(t *testing.T) {
	f := newFixture(t)

	get := signRequest(t, http.MethodGet, "http://example.com/bucket/key", nil, f.readonly, time.Now())
	if _, err := f.auth.Authenticate(get); err != nil {
		t.Fatalf("只读账号 GET 应通过，实际 %v", err)
	}

	put := signRequest(t, http.MethodPut, "http://example.com/bucket/key", []byte("x"), f.readonly, time.Now())
	if _, err := f.auth.Authenticate(put); codeOf(t, err) != CodeAccessDenied {
		t.Fatal("只读账号 PUT 应为 AccessDenied")
	}

	del := signRequest(t, http.MethodDelete, "http://example.com/bucket/key", nil, f.readonly, time.Now())
	if _, err := f.auth.Authenticate(del); codeOf(t, err) != CodeAccessDenied {
		t.Fatal("只读账号 DELETE 应为 AccessDenied")
	}
}

func TestWriteErrorIsDeterministic(t *testing.T) {
	render := func() string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://example.com/b", nil)
		WriteError(rec, req, AccessDenied())
		return rec.Body.String() + "|" + rec.Header().Get("Content-Type")
	}
	a, b := render(), render()
	if a != b {
		t.Fatalf("AccessDenied 响应应逐字节一致:\n%q\n%q", a, b)
	}
}
