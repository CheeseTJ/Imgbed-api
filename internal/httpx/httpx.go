// Package httpx 统一 HTTP 响应格式。
//
// 错误体的形状与 Ucenter 保持一致：{"code": "...", "message": "..."}，
// 这样前端可以只写一套错误处理逻辑。
package httpx

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Error 是统一错误响应体。
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// 错误码常量。
const (
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeTooLarge     = "payload_too_large"
	CodeInternal     = "internal_error"
)

// Abort 中止请求链并返回统一格式的错误。
func Abort(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, Error{Code: code, Message: message})
}

// BadRequest 返回 400。
func BadRequest(c *gin.Context, message string) {
	Abort(c, http.StatusBadRequest, CodeBadRequest, message)
}

// Unauthorized 返回 401。
func Unauthorized(c *gin.Context, message string) {
	Abort(c, http.StatusUnauthorized, CodeUnauthorized, message)
}

// Forbidden 返回 403。
func Forbidden(c *gin.Context, message string) {
	Abort(c, http.StatusForbidden, CodeForbidden, message)
}

// NotFound 返回 404。
func NotFound(c *gin.Context, message string) {
	Abort(c, http.StatusNotFound, CodeNotFound, message)
}

// Conflict 返回 409。
func Conflict(c *gin.Context, message string) {
	Abort(c, http.StatusConflict, CodeConflict, message)
}

// TooLarge 返回 413。
func TooLarge(c *gin.Context, message string) {
	Abort(c, http.StatusRequestEntityTooLarge, CodeTooLarge, message)
}

// Internal 记录日志后返回 500。内部错误细节只进日志，不回给客户端。
func Internal(c *gin.Context, err error, message string) {
	if err != nil {
		log.Printf("内部错误 %s %s: %v", c.Request.Method, c.Request.URL.Path, err)
	}
	Abort(c, http.StatusInternalServerError, CodeInternal, message)
}
