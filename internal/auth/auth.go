// Package auth 实现 S3 SigV4 请求校验与身份注入。
//
// 它不做任何密码学自研：用 aws-sdk-go-v2 的 v4 signer 以账号明文 SK
// 重算签名，再与请求头中的签名做常量时间比对。
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/YLing2024/objbox/internal/account"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	// Algorithm 是 SigV4 的算法标识。
	Algorithm = "AWS4-HMAC-SHA256"
	// Service 是本服务固定使用的 SigV4 service 名。
	Service = "s3"
	// MaxSkew 是允许的时钟偏移。
	MaxSkew = 15 * time.Minute
	// MaxPresignExpires 是预签名 URL 的最长有效期（7 天，S3 上限）。
	MaxPresignExpires = 7 * 24 * time.Hour
	// UnsignedPayload 是预签名请求使用的 payload hash（S3 预签名不签 body）。
	UnsignedPayload = "UNSIGNED-PAYLOAD"

	amzDateFormat = "20060102T150405Z"
	terminator    = "aws4_request"
)

// Code 是 S3 错误码。
type Code string

// 本服务用到的错误码。
const (
	CodeAccessDenied          Code = "AccessDenied"
	CodeSignatureDoesNotMatch Code = "SignatureDoesNotMatch"
	CodeRequestTimeTooSkewed  Code = "RequestTimeTooSkewed"
	CodeInvalidRequest        Code = "InvalidRequest"
	CodeInternalError         Code = "InternalError"
	CodeQuotaExceeded         Code = "QuotaExceeded"
)

// Error 是一次认证/校验失败，携带 S3 错误码、消息与 HTTP 状态。
type Error struct {
	Code    Code
	Message string
	Status  int
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// AccessDenied 返回统一的 403 AccessDenied。
func AccessDenied() *Error {
	return &Error{Code: CodeAccessDenied, Message: "Access Denied", Status: http.StatusForbidden}
}

// SignatureDoesNotMatch 返回 403 SignatureDoesNotMatch。
func SignatureDoesNotMatch() *Error {
	return &Error{
		Code:    CodeSignatureDoesNotMatch,
		Message: "The request signature we calculated does not match the signature you provided. Check your key and signing method.",
		Status:  http.StatusForbidden,
	}
}

// RequestTimeTooSkewed 返回 403 RequestTimeTooSkewed。
func RequestTimeTooSkewed() *Error {
	return &Error{
		Code:    CodeRequestTimeTooSkewed,
		Message: "The difference between the request time and the server's time is too large.",
		Status:  http.StatusForbidden,
	}
}

// RequestExpired 返回 403 AccessDenied，文案为 S3 的 "Request has expired"。
func RequestExpired() *Error {
	return &Error{Code: CodeAccessDenied, Message: "Request has expired", Status: http.StatusForbidden}
}

// InvalidRequestf 返回 400 InvalidRequest，原因写入消息。
func InvalidRequestf(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf(format, args...), Status: http.StatusBadRequest}
}

// QuotaExceeded 返回 403 QuotaExceeded。
func QuotaExceeded() *Error {
	return &Error{Code: CodeQuotaExceeded, Message: "Quota exceeded", Status: http.StatusForbidden}
}

// InternalErrorf 返回 500 InternalError。
func InternalErrorf(format string, args ...any) *Error {
	return &Error{Code: CodeInternalError, Message: fmt.Sprintf(format, args...), Status: http.StatusInternalServerError}
}

// Authenticator 校验入站请求并解析出账号。
type Authenticator struct {
	store *account.Store
	now   func() time.Time
}

// NewAuthenticator 构造 Authenticator。
func NewAuthenticator(store *account.Store) *Authenticator {
	return &Authenticator{store: store, now: time.Now}
}

// Authenticate 校验请求并解析出账号。
//
// 优先使用 Authorization 头（SigV4 header 签名）；没有该头但 query 带
// X-Amz-Signature 时走预签名（query 签名）校验。两条路径复用同一套
// aws-sdk-go-v2/aws/signer/v4 重算逻辑，不另写密码学。
func (a *Authenticator) Authenticate(r *http.Request) (*account.Account, error) {
	if header := r.Header.Get("Authorization"); header != "" {
		return a.authenticateHeader(r, header)
	}
	if r.URL.Query().Get("X-Amz-Signature") != "" {
		return a.authenticatePresigned(r)
	}
	return nil, AccessDenied()
}

// authenticateHeader 校验 Authorization 头签名。
func (a *Authenticator) authenticateHeader(r *http.Request, header string) (*account.Account, error) {
	comp, err := parseAuthorization(header)
	if err != nil {
		return nil, AccessDenied()
	}
	acct, err := a.lookupAccount(comp.Scope.AccessKey)
	if err != nil {
		return nil, err
	}

	t, ok := requestTime(r)
	if !ok {
		return nil, AccessDenied()
	}
	if skew := a.now().Sub(t); skew > MaxSkew || skew < -MaxSkew {
		return nil, RequestTimeTooSkewed()
	}
	if acct.Readonly && isWriteMethod(r.Method) {
		return nil, AccessDenied()
	}

	computed, err := recomputeSignature(r, acct, comp, t)
	if err != nil {
		return nil, SignatureDoesNotMatch()
	}
	if computed == "" || subtle.ConstantTimeCompare([]byte(computed), []byte(comp.Signature)) != 1 {
		return nil, SignatureDoesNotMatch()
	}
	return acct, nil
}

// authenticatePresigned 校验 query 签名（预签名 URL）。
//
// 过期检查先于签名比对：已过期一律 403 "Request has expired"，
// 且由于签名覆盖 X-Amz-Date，任何篡改都仍会被签名比对拒绝。
func (a *Authenticator) authenticatePresigned(r *http.Request) (*account.Account, error) {
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") != Algorithm {
		return nil, AccessDenied()
	}
	comp, err := parseQueryComponents(q)
	if err != nil {
		return nil, AccessDenied()
	}
	acct, err := a.lookupAccount(comp.Scope.AccessKey)
	if err != nil {
		return nil, err
	}

	t, ok := parseAmzDate(q.Get("X-Amz-Date"))
	if !ok {
		return nil, AccessDenied()
	}
	expires, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expires < 0 {
		return nil, AccessDenied()
	}
	if time.Duration(expires)*time.Second > MaxPresignExpires {
		return nil, &Error{
			Code:    CodeAccessDenied,
			Message: "X-Amz-Expires must be less than a week (604800)",
			Status:  http.StatusForbidden,
		}
	}

	now := a.now()
	if now.After(t.Add(time.Duration(expires) * time.Second)) {
		return nil, RequestExpired()
	}
	if t.Sub(now) > MaxSkew {
		return nil, RequestTimeTooSkewed()
	}
	if acct.Readonly && isWriteMethod(r.Method) {
		return nil, AccessDenied()
	}

	ok, err = verifyPresign(r, acct, comp, t)
	if err != nil {
		return nil, SignatureDoesNotMatch()
	}
	if !ok {
		return nil, SignatureDoesNotMatch()
	}
	return acct, nil
}

// lookupAccount 按 AK 查账号并检查停用状态。
func (a *Authenticator) lookupAccount(ak string) (*account.Account, error) {
	acct, ok := a.store.GetByAK(ak)
	if !ok || acct == nil || acct.Disabled {
		return nil, AccessDenied()
	}
	return acct, nil
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPut, http.MethodPost, http.MethodDelete:
		return true
	default:
		return false
	}
}

// credentialScope 是 Credential 的五段式：AK/date/region/service/aws4_request。
type credentialScope struct {
	AccessKey  string
	Date       string
	Region     string
	Service    string
	Terminator string
}

type authComponents struct {
	Scope         credentialScope
	SignedHeaders []string
	Signature     string
}

func parseAuthorization(header string) (*authComponents, error) {
	prefix := Algorithm + " "
	if !strings.HasPrefix(header, prefix) {
		return nil, errors.New("auth: 不支持的 Authorization 方案")
	}
	rest := strings.TrimSpace(header[len(prefix):])

	fields := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		i := strings.IndexByte(part, '=')
		if i <= 0 {
			return nil, errors.New("auth: Authorization 字段格式错误")
		}
		fields[part[:i]] = part[i+1:]
	}

	cred := fields["Credential"]
	signedHeaders := fields["SignedHeaders"]
	signature := fields["Signature"]
	if cred == "" || signedHeaders == "" || signature == "" {
		return nil, errors.New("auth: Authorization 缺少必要字段")
	}

	scope, err := parseCredentialScope(cred)
	if err != nil {
		return nil, err
	}

	var headers []string
	for _, h := range strings.Split(strings.ToLower(signedHeaders), ";") {
		if h = strings.TrimSpace(h); h != "" {
			headers = append(headers, h)
		}
	}
	return &authComponents{Scope: scope, SignedHeaders: headers, Signature: signature}, nil
}

// parseCredentialScope 解析五段式 Credential：AK/date/region/service/aws4_request。
func parseCredentialScope(cred string) (credentialScope, error) {
	parts := strings.Split(cred, "/")
	if len(parts) != 5 {
		return credentialScope{}, errors.New("auth: Credential 段数错误")
	}
	scope := credentialScope{
		AccessKey:  parts[0],
		Date:       parts[1],
		Region:     parts[2],
		Service:    parts[3],
		Terminator: parts[4],
	}
	if scope.Service != Service || scope.Terminator != terminator {
		return credentialScope{}, errors.New("auth: Credential scope 不受支持")
	}
	return scope, nil
}

// parseQueryComponents 从预签名 query 中解析出与头签名同构的组件。
func parseQueryComponents(q url.Values) (*authComponents, error) {
	scope, err := parseCredentialScope(q.Get("X-Amz-Credential"))
	if err != nil {
		return nil, err
	}
	signature := q.Get("X-Amz-Signature")
	if signature == "" {
		return nil, errors.New("auth: 缺少 X-Amz-Signature")
	}
	var headers []string
	for _, h := range strings.Split(strings.ToLower(q.Get("X-Amz-SignedHeaders")), ";") {
		if h = strings.TrimSpace(h); h != "" {
			headers = append(headers, h)
		}
	}
	return &authComponents{Scope: scope, SignedHeaders: headers, Signature: signature}, nil
}

func parseAmzDate(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(amzDateFormat, v)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func requestTime(r *http.Request) (time.Time, bool) {
	if t, ok := parseAmzDate(r.Header.Get("x-amz-date")); ok {
		return t, true
	}
	if v := r.Header.Get("Date"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// verifyPresign 用 v4 signer 的 PresignHTTP 以账号明文 SK 重算预签名，
// 再与请求携带的 X-Amz-Signature 做常量时间比对。
func verifyPresign(r *http.Request, acct *account.Account, comp *authComponents, t time.Time) (bool, error) {
	r2 := r.Clone(r.Context())
	r2.Header = make(http.Header, len(comp.SignedHeaders))
	hasContentLength := false
	for _, h := range comp.SignedHeaders {
		switch h {
		case "host":
			continue
		case "content-length":
			hasContentLength = true
			continue
		}
		if vals, ok := r.Header[textproto.CanonicalMIMEHeaderKey(h)]; ok {
			r2.Header[textproto.CanonicalMIMEHeaderKey(h)] = append([]string(nil), vals...)
		}
	}
	if hasContentLength {
		r2.ContentLength = r.ContentLength
	} else {
		r2.ContentLength = 0
	}
	r2.Host = r.Host

	// 去掉请求携带的签名，交给 signer 重新生成完整签名 URL。
	u := *r.URL
	q := u.Query()
	q.Del("X-Amz-Signature")
	u.RawQuery = q.Encode()
	r2.URL = &u

	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: acct.AK, SecretAccessKey: acct.SK}
	signedURI, _, err := signer.PresignHTTP(r.Context(), creds, r2, UnsignedPayload, Service, comp.Scope.Region, t)
	if err != nil {
		return false, err
	}
	parsed, err := url.Parse(signedURI)
	if err != nil {
		return false, err
	}
	got := parsed.Query().Get("X-Amz-Signature")
	return subtle.ConstantTimeCompare([]byte(got), []byte(comp.Signature)) == 1, nil
}

// recomputeSignature 仅用请求声明的 SignedHeaders 重建规范请求并重算签名。
//
// 只取 SignedHeaders 中列出的头，可避免 Go 传输层自动添加的头
// （如 Accept-Encoding）污染规范请求；content-length 由 signer 依据
// ContentLength 自动纳入，因此按是否在 SignedHeaders 中决定是否传入。
func recomputeSignature(r *http.Request, acct *account.Account, comp *authComponents, t time.Time) (string, error) {
	r2 := r.Clone(r.Context())
	r2.Header = make(http.Header, len(comp.SignedHeaders)+1)

	hasContentLength := false
	for _, h := range comp.SignedHeaders {
		switch h {
		case "host":
			continue
		case "content-length":
			hasContentLength = true
			continue
		}
		if vals, ok := r.Header[textproto.CanonicalMIMEHeaderKey(h)]; ok {
			r2.Header[textproto.CanonicalMIMEHeaderKey(h)] = append([]string(nil), vals...)
		}
	}
	if hasContentLength {
		r2.ContentLength = r.ContentLength
	} else {
		r2.ContentLength = 0
	}

	r2.Host = r.Host

	payloadHash := r.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}

	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: acct.AK, SecretAccessKey: acct.SK}
	if err := signer.SignHTTP(r.Context(), creds, r2, payloadHash, Service, comp.Scope.Region, t); err != nil {
		return "", err
	}
	computed, err := parseAuthorization(r2.Header.Get("Authorization"))
	if err != nil {
		return "", err
	}
	return computed.Signature, nil
}

// ---- 身份注入 context ----

type accountCtxKey struct{}

// WithAccount 把账号写入 context。
func WithAccount(ctx context.Context, acct *account.Account) context.Context {
	return context.WithValue(ctx, accountCtxKey{}, acct)
}

// AccountFromContext 从 context 取回账号。
func AccountFromContext(ctx context.Context) (*account.Account, bool) {
	acct, ok := ctx.Value(accountCtxKey{}).(*account.Account)
	return acct, ok
}

type requestIDCtxKey struct{}

// WithRequestID 把本次请求的 request id 写入 context，供错误响应复用。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDCtxKey{}, id)
}

// RequestIDFromContext 取回本次请求的 request id。
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDCtxKey{}).(string)
	return id
}

// ---- 错误响应 ----

// MaskAuthorization 对 Authorization 头脱敏：只保留 AK 与签名前 8 位。
// 完整头永不落日志。
func MaskAuthorization(header string) string {
	if header == "" {
		return ""
	}
	comp, err := parseAuthorization(header)
	if err != nil {
		return Algorithm + " <malformed>"
	}
	sig := comp.Signature
	if len(sig) > 8 {
		sig = sig[:8]
	}
	return Algorithm + " Credential=" + comp.Scope.AccessKey + "/..., Signature=" + sig + "..."
}

type errorXML struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// WriteError 把认证/校验错误写成 S3 XML 响应。
//
// 有意保持响应体不含随机字段（RequestId/Resource 不写入 body），
// 以延续 M0 的“跨账号访问”与“桶不存在”响应逐字节一致、不泄露存在性的保证；
// request id 仍通过 x-amz-request-id 响应头给出。
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	code := CodeAccessDenied
	message := "Access Denied"
	status := http.StatusForbidden
	var ae *Error
	if errors.As(err, &ae) {
		code = ae.Code
		message = ae.Message
		status = ae.Status
	}

	w.Header().Set("Server", "objbox")
	w.Header().Set("Content-Type", "application/xml")
	if id := RequestIDFromContext(r.Context()); id != "" {
		w.Header().Set("x-amz-request-id", id)
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	body, mErr := xml.Marshal(errorXML{Code: string(code), Message: message})
	if mErr != nil {
		return
	}
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}
