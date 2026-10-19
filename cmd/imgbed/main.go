// imgbed API 服务入口。
//
// 本服务只提供 REST API、不渲染任何页面；前端是独立的静态站点。
// 元数据存放在 OCI HeatWave（MySQL），图片二进制存放在 Cloudflare R2，
// 图片读取不经过本服务（由 R2 自定义域名经 CDN 直出）。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"imgbed/internal/api"
	"imgbed/internal/auth"
	"imgbed/internal/config"
	"imgbed/internal/r2"
	"imgbed/internal/store"
)

func main() {
	configPath := flag.String("config", "./config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	// 对象存储：构造失败不算致命错误——服务仍可提供元数据读取，
	// 只是上传接口会明确返回 503，而不是留下一个会 panic 的 nil。
	r2Client, err := r2.New(cfg.R2)
	if err != nil {
		log.Printf("警告: 对象存储未启用（%v），上传相关接口将返回 503", err)
	} else {
		// 启动时探一次，把「凭据到底对不对」写进启动日志，
		// 而不是等第一个用户上传失败才发现。
		probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := r2Client.Ping(probeCtx); err != nil {
			log.Printf("警告: R2 连通性检查失败（%v）", err)
		} else {
			log.Printf("R2 就绪: bucket=%s endpoint=%s", r2Client.Bucket(), cfg.R2.Endpoint())
		}
		cancel()
	}

	st, err := store.Open(cfg.DB)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	defer st.Close()

	fetcher := auth.NewFetcher(cfg.Ucenter.BaseURL, cfg.Ucenter.JwksDuration())
	// 启动时预热 JWKS：失败只告警，首次鉴权时会重试
	if err := fetcher.Warmup(); err != nil {
		log.Printf("警告: 预热 JWKS 失败（%v），将在首次鉴权时重试", err)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	r.Use(corsMiddleware(cfg.Server.AllowedOrigins))

	api.Register(r, api.Deps{Cfg: cfg, Store: st, Fetcher: fetcher, R2: r2Client})

	// 回收站的过期清理：每小时扫一次，把过了保留期的记录真正删掉。
	// 这一行是「保留 7 天后自动删除」能成立的前提——没有它，
	// 软删除的图会永久留在库里和 R2 里，界面上的承诺就是假的。
	cleanupCtx, stopCleanup := context.WithCancel(context.Background())
	defer stopCleanup()
	api.StartTrashCleanup(cleanupCtx,
		api.Deps{Cfg: cfg, Store: st, R2: r2Client}, time.Hour)

	srv := &http.Server{Addr: cfg.Server.Addr, Handler: r}
	go func() {
		log.Printf("imgbed 启动，监听 %s（ucenter=%s app=%s bucket=%s）",
			cfg.Server.Addr, cfg.Ucenter.BaseURL, cfg.Ucenter.App, cfg.R2.Bucket)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("收到退出信号，正在关闭...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP 服务关闭异常: %v", err)
	}
	log.Println("已退出")
}

// corsMiddleware 允许前端站点跨域直连。
//
// 认证走 Authorization 头、不依赖 Cookie，因此放行来源的风险可控。
// 配置了白名单时只回显命中项；未配置则回显请求来源（等价放行全部）。
func corsMiddleware(allowed []string) gin.HandlerFunc {
	allowAll := len(allowed) == 0
	whitelist := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		whitelist[strings.TrimRight(o, "/")] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := strings.TrimRight(c.GetHeader("Origin"), "/")
		if origin != "" {
			_, hit := whitelist[origin]
			if allowAll || hit {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin") // 避免 CDN 缓存串源
			}
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
