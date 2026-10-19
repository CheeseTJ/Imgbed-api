package api

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"imgbed/internal/auth"
	"imgbed/internal/httpx"
	"imgbed/internal/r2"
)

// r2Usage 尽力查询 R2 的真实占用（Cloudflare 口径）。
//
// 没配 token 或上游出错时返回 nil 并记日志——存储条退回数据库口径。
// 这只是个辅助信息，不值得让整个用量接口因为它而失败。
func r2Usage(c *gin.Context, d Deps) *r2.BucketUsage {
	if d.R2 == nil {
		return nil
	}
	u, err := d.R2.BucketUsage(c.Request.Context())
	if err != nil {
		log.Printf("查询 R2 真实占用失败（存储条回退到数据库口径）: %v", err)
		return nil
	}
	return u
}

// r2AccountUsage 尽力查询账户级 R2 占用（全部 bucket 汇总）。
//
// 供 hover 面板展示「整个 Cloudflare 账户」的用量，让用户能对上
// dashboard 的账户级数字。失败只记日志并返回 nil，前端隐藏浮层即可——
// 它是装饰性数据，绝不能拖垮 /me。
func r2AccountUsage(c *gin.Context, d Deps) *r2.AccountUsage {
	if d.R2 == nil {
		return nil
	}
	u, err := d.R2.AccountUsage(c.Request.Context())
	if err != nil {
		log.Printf("查询账户级 R2 占用失败（全量面板隐藏，存储条不受影响）: %v", err)
		return nil
	}
	return u
}

// health 返回各外部依赖的连通性。
//
// 只报告「是否配置 / 是否可达」，不回显任何密钥值。
func health(c *gin.Context, d Deps) {
	ctx := c.Request.Context()

	keys, fetchedAt, jwksErr := d.Fetcher.Status()
	jwks := gin.H{"keys": keys, "ok": keys > 0 && jwksErr == nil}
	if !fetchedAt.IsZero() {
		jwks["fetched_at"] = fetchedAt.Unix()
	}
	if jwksErr != nil {
		jwks["error"] = jwksErr.Error()
	}

	dbOK := d.Store.DB().PingContext(ctx) == nil

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"ucenter": gin.H{
			"base_url": d.Cfg.Ucenter.BaseURL,
			"app":      d.Cfg.Ucenter.App,
			"jwks":     jwks,
		},
		"r2": r2Status(c, d),
		"db": gin.H{"ok": dbOK},
	})
}

// r2Status 汇总对象存储状态。
//
// 关键在于它趁请求还活着时**真的探一次**：只报「配置里填了值」没有意义——
// 凭据写错、bucket 名拼错、token 没授权，在配置层面看起来都是「已配置」。
func r2Status(c *gin.Context, d Deps) gin.H {
	info := gin.H{
		"configured":  d.Cfg.R2Ready(),
		"bucket":      d.Cfg.R2.Bucket,
		"public_base": d.Cfg.R2.PublicBase,
		"endpoint":    d.Cfg.R2.Endpoint(),
	}
	if d.R2 != nil {
		if err := d.R2.Ping(c.Request.Context()); err != nil {
			info["ok"] = false
			info["error"] = err.Error()
		} else {
			info["ok"] = true
		}
	}
	return info
}

// me 返回当前登录用户的信息与用量。
func me(c *gin.Context, d Deps) {
	ctx := c.Request.Context()
	uid := auth.UserID(c)

	u, err := d.Store.GetUser(ctx, uid)
	if err != nil {
		httpx.Internal(c, err, "读取用户失败")
		return
	}
	usage, err := d.Store.GetUsage(ctx, uid)
	if err != nil {
		httpx.Internal(c, err, "统计用量失败")
		return
	}

	user := gin.H{
		"id":    uid,
		"email": c.GetString(auth.CtxEmail),
		"role":  c.GetString(auth.CtxRole),
	}
	if u != nil {
		user["created_at"] = u.CreatedAt
		user["last_seen_at"] = u.LastSeenAt
	}

	c.JSON(http.StatusOK, gin.H{
		"user":  user,
		"usage": usagePayload(usage, r2Usage(c, d), r2AccountUsage(c, d), r2Bucket(d)),
	})
}

// usage 返回当前用户的用量。
func usage(c *gin.Context, d Deps) {
	u, err := d.Store.GetUsage(c.Request.Context(), auth.UserID(c))
	if err != nil {
		httpx.Internal(c, err, "统计用量失败")
		return
	}
	c.JSON(http.StatusOK, usagePayload(u, r2Usage(c, d), r2AccountUsage(c, d), r2Bucket(d)))
}

// r2Bucket 返回当前配置的桶名；R2 未启用时为空串。
func r2Bucket(d Deps) string {
	if d.R2 == nil {
		return ""
	}
	return d.R2.Bucket()
}
