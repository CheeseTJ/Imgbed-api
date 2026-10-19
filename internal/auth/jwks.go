// Package auth 实现 Ucenter 签发 JWT（EdDSA / Ed25519）的本地验签，
// 以及把校验结果注入 Gin context 的中间件。
//
// Ucenter 不使用 OAuth2 / OIDC：它直接用「邮箱 + 密码」换一对令牌，
// 其中 access_token 是 Ed25519 签名的 JWT，refresh_token 是不透明随机串。
// 各业务方拉取 /.well-known/jwks.json 后本地验签——不回源、不共享私钥。
package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// jwk 是 JWKS 中的单个公钥项，只关心 Ed25519 需要的字段。
type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"` // base64url 编码的 32 字节公钥
}

// Fetcher 拉取并缓存 Ucenter 的 JWKS 公钥，支持按 kid 查询。
//
// 缓存命中时完全不产生网络请求；kid 未知时会强制刷新一次，以应对密钥轮换。
type Fetcher struct {
	baseURL string
	ttl     time.Duration
	client  *http.Client

	mu        sync.Mutex
	keys      map[string]ed25519.PublicKey
	fetchedAt time.Time
	lastErr   error
	now       func() time.Time
}

// NewFetcher 创建 JWKS 获取器，ttl 为公钥缓存时长（<=0 时回退 1 小时）。
func NewFetcher(baseURL string, ttl time.Duration) *Fetcher {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &Fetcher{
		baseURL: strings.TrimRight(baseURL, "/"),
		ttl:     ttl,
		client:  &http.Client{Timeout: 10 * time.Second},
		keys:    make(map[string]ed25519.PublicKey),
		now:     time.Now,
	}
}

// GetKey 返回 kid 对应的公钥。
//
// 缓存过期、或 kid 未知（可能发生了密钥轮换）时重新拉取；
// 拉取失败时若仍有可用旧缓存则继续使用，否则返回错误。
func (f *Fetcher) GetKey(kid string) (ed25519.PublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fresh := f.now().Sub(f.fetchedAt) < f.ttl
	if pub, ok := f.keys[kid]; ok && fresh {
		return pub, nil
	}

	if err := f.refreshLocked(); err != nil {
		if pub, ok := f.keys[kid]; ok {
			return pub, nil // 回退到仍可用的旧缓存
		}
		return nil, err
	}

	pub, ok := f.keys[kid]
	if !ok {
		return nil, fmt.Errorf("jwks 中不存在 kid=%q（公钥可能已轮换）", kid)
	}
	return pub, nil
}

// Warmup 主动拉取一次 JWKS，用于启动时提前暴露配置问题。
func (f *Fetcher) Warmup() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshLocked()
}

// Status 报告 JWKS 的当前状态，供健康检查使用。
func (f *Fetcher) Status() (keys int, fetchedAt time.Time, lastErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.keys), f.fetchedAt, f.lastErr
}

// refreshLocked 重新拉取 JWKS 并替换缓存，调用方须已持锁。
func (f *Fetcher) refreshLocked() error {
	resp, err := f.client.Get(f.baseURL + "/.well-known/jwks.json")
	if err != nil {
		f.lastErr = fmt.Errorf("拉取 JWKS 失败: %w", err)
		return f.lastErr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		f.lastErr = fmt.Errorf("拉取 JWKS 返回状态码 %d", resp.StatusCode)
		return f.lastErr
	}

	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		f.lastErr = fmt.Errorf("解析 JWKS 失败: %w", err)
		return f.lastErr
	}

	keys := make(map[string]ed25519.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		// 忽略非 OKP/Ed25519 或字段缺失的条目
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" || k.X == "" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		keys[k.Kid] = ed25519.PublicKey(raw)
	}
	if len(keys) == 0 {
		f.lastErr = fmt.Errorf("jwks 中没有可用的 Ed25519 公钥")
		return f.lastErr
	}

	f.keys = keys
	f.fetchedAt = f.now()
	f.lastErr = nil
	return nil
}
