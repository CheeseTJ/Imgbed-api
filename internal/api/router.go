// Package api 组装 HTTP 路由。
package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"imgbed/internal/auth"
	"imgbed/internal/config"
	"imgbed/internal/httpx"
	"imgbed/internal/r2"
	"imgbed/internal/store"
)

// Deps 是路由所需的依赖。
type Deps struct {
	Cfg     *config.Config
	Store   *store.Store
	Fetcher *auth.Fetcher
	// R2 为 nil 表示对象存储未配置。相关接口返回 503 而不是 panic，
	// 让「只读功能可用、上传暂时不可用」成为一种可接受的状态。
	R2 *r2.Client
}

// R2Ready 报告对象存储是否可用。
func (d Deps) R2Ready() bool { return d.R2 != nil }

// Register 挂载全部路由。
//
// 分层约定：
//   - /healthz        存活探针，不查任何外部依赖
//   - /api/health     详细诊断，公开但只暴露「是否配置」，不回显任何密钥
//   - /api/v1/*       全部需要 Ucenter JWT
func Register(r *gin.Engine, d Deps) {
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.GET("/api/health", func(c *gin.Context) { health(c, d) })

	v1 := r.Group("/api/v1")
	v1.Use(auth.Middleware(d.Fetcher, d.Cfg.Ucenter.App))
	v1.Use(ensureUser(d.Store))
	{
		v1.GET("/me", func(c *gin.Context) { me(c, d) })
		v1.GET("/usage", func(c *gin.Context) { usage(c, d) })

		v1.POST("/images/upload", func(c *gin.Context) { uploadImage(c, d) })
		v1.GET("/images", func(c *gin.Context) { listImages(c, d) })
		v1.GET("/images/:id", func(c *gin.Context) { getImage(c, d) })
		// PATCH 承载图片的「移动目录」与「改名」，二者可任意组合
		v1.PATCH("/images/:id", func(c *gin.Context) { patchImage(c, d) })
		v1.DELETE("/images/:id", func(c *gin.Context) { deleteImage(c, d) })

		v1.GET("/folders", func(c *gin.Context) { listFolders(c, d) })
		v1.POST("/folders", func(c *gin.Context) { createFolder(c, d) })
		// PATCH = 改名/移动（走异步迁移任务），DELETE = 删除空目录
		v1.PATCH("/folders/:id", func(c *gin.Context) { renameFolder(c, d) })
		v1.DELETE("/folders/:id", func(c *gin.Context) { deleteFolder(c, d) })

		// 回收站。恢复与彻底删除都挂在 /trash 下面而不是 /images/:id/restore，
		// 因为 Gin 的路由树里 POST /images/upload（静态段）与 POST /images/:id/...
		// （参数段）会因通配冲突而 panic——把它们挪开最省事也最不容易踩。
		v1.GET("/trash", func(c *gin.Context) { listTrash(c, d) })
		v1.DELETE("/trash", func(c *gin.Context) { emptyTrash(c, d) })
		v1.POST("/trash/images/:id/restore", func(c *gin.Context) { restoreImage(c, d) })
		v1.DELETE("/trash/images/:id", func(c *gin.Context) { purgeImage(c, d) })

		v1.GET("/jobs/:id", func(c *gin.Context) { getJob(c, d) })
		v1.POST("/batch/images", func(c *gin.Context) { batchImages(c, d) })
	}

	// 非 /api 路径：web_dir（前端构建产物）存在时由本进程托管 SPA，
	// 一个端口同时出 UI 与 API，隧道/反代只需指过来。
	// 目录不存在（开发期前端走 Vite、或尚未构建）则保持纯 API 行为。
	webRoot := d.Cfg.Server.WebDir
	if st, err := os.Stat(webRoot); err == nil && st.IsDir() {
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				httpx.NotFound(c, "接口不存在")
				return
			}
			// 先拼「/」再 Clean，杜绝 ../ 目录穿越；只放行真实存在的文件，
			// 其余（/login /trash 等前端路由）统一回退 index.html 交给前端路由器。
			// index.html 必须禁缓存，否则发版后浏览器还拿着旧壳加载旧 hash 资源。
			p := filepath.Join(webRoot, filepath.Clean("/"+c.Request.URL.Path))
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				c.File(p)
				return
			}
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.File(filepath.Join(webRoot, "index.html"))
		})
	} else {
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				httpx.NotFound(c, "接口不存在")
				return
			}
			c.String(http.StatusNotFound, "not found")
		})
	}
}

// ensureUser 把 JWT 中的身份落到影子用户表，后续 handler 就能直接查库。
// 写入受 store 内部的节流控制，不会每个请求都产生写事务。
func ensureUser(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		err := st.EnsureUser(
			c.Request.Context(),
			auth.UserID(c),
			c.GetString(auth.CtxEmail),
			c.GetString(auth.CtxRole),
		)
		if err != nil {
			httpx.Internal(c, err, "用户信息写入失败")
			return
		}
		c.Next()
	}
}
