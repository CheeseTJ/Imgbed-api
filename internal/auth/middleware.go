package auth

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"imgbed/internal/httpx"
)

// 认证结果在 Gin context 中的键。
const (
	CtxUserID = "uid" // 对应 JWT 的 sub，即 Ucenter 的用户 ID
	CtxEmail  = "email"
	CtxRole   = "role"
)

// bearerToken 从 Authorization 头提取 Bearer 令牌，格式不符时返回空串。
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

// Middleware 校验 Ucenter 签发的 JWT。
//
// 校验项：
//   - alg 必须为 EdDSA（显式限定，防算法混淆攻击）
//   - exp 必须存在且未过期
//   - aud 必须等于本应用的标识
//
// 通过后把 sub / email / role 写入 Gin context。
func Middleware(f *Fetcher, aud string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c.GetHeader("Authorization"))
		if token == "" {
			httpx.Unauthorized(c, "缺少 Bearer 令牌")
			return
		}

		// 先不验签地解析出 header 中的 kid，用于定位公钥
		unverified, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
		if err != nil {
			httpx.Unauthorized(c, "令牌格式非法")
			return
		}
		kid, _ := unverified.Header["kid"].(string)

		pub, err := f.GetKey(kid)
		if err != nil {
			httpx.Unauthorized(c, "无法获取验签公钥")
			return
		}

		parsed, err := jwt.NewParser(
			jwt.WithValidMethods([]string{"EdDSA"}),
			jwt.WithExpirationRequired(),
			jwt.WithAudience(aud),
		).Parse(token, func(*jwt.Token) (any, error) { return pub, nil })
		if err != nil || !parsed.Valid {
			httpx.Unauthorized(c, "令牌无效或已过期")
			return
		}

		claims, ok := parsed.Claims.(jwt.MapClaims)
		if !ok {
			httpx.Unauthorized(c, "令牌载荷非法")
			return
		}
		sub, _ := claims["sub"].(string)
		if sub == "" {
			httpx.Unauthorized(c, "令牌缺少 sub")
			return
		}
		email, _ := claims["email"].(string)
		role, _ := claims["role"].(string)

		c.Set(CtxUserID, sub)
		c.Set(CtxEmail, email)
		c.Set(CtxRole, role)
		c.Next()
	}
}

// UserID 返回当前登录用户 ID，未认证时为空串。
func UserID(c *gin.Context) string { return c.GetString(CtxUserID) }
