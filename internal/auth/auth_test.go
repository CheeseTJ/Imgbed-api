package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// kidFor 由公钥派生 kid，与 Ucenter 的算法无关，测试里只需前后一致。
const testKid = "test-kid-1"

// newJWKSServer 起一个返回指定公钥的假 JWKS 服务，并统计被访问次数。
func newJWKSServer(t *testing.T, pub ed25519.PublicKey, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		*hits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "OKP",
				"crv": "Ed25519",
				"alg": "EdDSA",
				"use": "sig",
				"kid": testKid,
				"x":   base64.RawURLEncoding.EncodeToString(pub),
			}},
		})
	}))
}

// signToken 用私钥签发一个测试令牌。
func signToken(t *testing.T, priv ed25519.PrivateKey, aud string, exp time.Time) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss":   "https://ucenter.june-t.top",
		"sub":   "user-123",
		"aud":   aud,
		"exp":   exp.Unix(),
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"email": "me@example.com",
		"role":  "user",
	})
	tok.Header["kid"] = testKid

	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("签发测试令牌失败: %v", err)
	}
	return signed
}

// TestFetcherCachesKeys 同一 kid 在 TTL 内只应触发一次网络请求。
func TestFetcherCachesKeys(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	hits := 0
	srv := newJWKSServer(t, pub, &hits)
	defer srv.Close()

	f := NewFetcher(srv.URL, time.Hour)
	if _, err := f.GetKey(testKid); err != nil {
		t.Fatalf("首次取公钥失败: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := f.GetKey(testKid); err != nil {
			t.Fatalf("第 %d 次取公钥失败: %v", i, err)
		}
	}

	if hits != 1 {
		t.Fatalf("期望只拉取 1 次 JWKS，实际 %d 次", hits)
	}
}

// TestMiddleware 覆盖令牌校验的三条关键路径。
func TestMiddleware(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	hits := 0
	srv := newJWKSServer(t, pub, &hits)
	defer srv.Close()

	f := NewFetcher(srv.URL, time.Hour)
	mw := Middleware(f, "imgbed")

	cases := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"合法令牌", "Bearer " + signToken(t, priv, "imgbed", time.Now().Add(time.Hour)), http.StatusOK},
		{"缺少令牌", "", http.StatusUnauthorized},
		{"aud 不匹配", "Bearer " + signToken(t, priv, "other-app", time.Now().Add(time.Hour)), http.StatusUnauthorized},
		{"已过期", "Bearer " + signToken(t, priv, "imgbed", time.Now().Add(-time.Hour)), http.StatusUnauthorized},
		{"非 Bearer 格式", "Token abc", http.StatusUnauthorized},
		{"垃圾串", "Bearer not-a-jwt", http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotUID string
			r := newTestEngine(mw, func(c *ginCtx) {
				gotUID = UserID(c)
			})

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			r.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("期望状态码 %d，实际 %d，响应体 %s", tc.wantStatus, w.Code, w.Body.String())
			}
			if tc.wantStatus == http.StatusOK && gotUID != "user-123" {
				t.Fatalf("期望 uid=user-123，实际 %q", gotUID)
			}
		})
	}
}

// TestMiddlewareRejectsNoneAlg 确认算法混淆攻击被挡住。
//
// 攻击手法：把 header.alg 改成 none 并去掉签名，若服务端只用 kid 查公钥、
// 不限定算法，就会接受这个未签名的令牌。
func TestMiddlewareRejectsNoneAlg(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	hits := 0
	srv := newJWKSServer(t, pub, &hits)
	defer srv.Close()

	f := NewFetcher(srv.URL, time.Hour)
	mw := Middleware(f, "imgbed")

	// 手工拼一个 alg=none 的令牌
	unsafe := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "attacker",
		"aud": "imgbed",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	unsafe.Header["kid"] = testKid

	raw, err := unsafe.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	r := newTestEngine(mw, func(*ginCtx) {})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("alg=none 的令牌必须被拒绝，实际状态码 %d", w.Code)
	}
}
