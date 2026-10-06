package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func newTestServer(t *testing.T) (ts *httptest.Server, store *account.Store, alice, bob *account.Account) {
	t.Helper()
	store, err := account.Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	alice, err = store.Add("alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	bob, err = store.Add("bob", "", false)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(store)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ts = httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
	})
	return ts, store, alice, bob
}

// signedDo 用账号 SK 做 SigV4 签名后发原始 HTTP 请求。
func signedDo(t *testing.T, ts *httptest.Server, method, path string, body []byte, acct *account.Account, hdrs map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("x-amz-content-sha256", payload)

	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: acct.AK, SecretAccessKey: acct.SK}
	if err := signer.SignHTTP(context.Background(), creds, req, payload, "s3", testRegion, time.Now()); err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return b
}
