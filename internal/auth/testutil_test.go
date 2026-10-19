package auth

import "github.com/gin-gonic/gin"

// ginCtx 是 gin.Context 的别名，仅为了让测试文件的辅助函数签名简短。
type ginCtx = gin.Context

// newTestEngine 组装一个只挂载指定中间件与终端处理器的 Gin 引擎。
func newTestEngine(mw gin.HandlerFunc, handler func(*gin.Context)) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/me", mw, func(c *gin.Context) {
		if handler != nil {
			handler(c)
		}
		c.JSON(200, gin.H{"ok": true})
	})
	return r
}
