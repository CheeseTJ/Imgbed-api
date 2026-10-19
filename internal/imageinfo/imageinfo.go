// Package imageinfo 从上传内容中提取 MIME、扩展名与像素尺寸。
package imageinfo

import (
	"bytes"
	"image"
	"net/http"
	"strings"

	// 注册解码器，供 image.DecodeConfig 识别尺寸
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// Info 是一次识别结果。
type Info struct {
	Mime   string
	Ext    string
	Width  int
	Height int
}

// mimeExt 是允许上传的类型及其规范扩展名。
//
// 刻意不含 image/svg+xml：SVG 可以内嵌脚本，即便走独立域名提供服务，
// 也仍是钓鱼与部分 XSS 场景的常见载体。要支持的话应当单独评估并配 CSP。
var mimeExt = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// Inspect 识别类型与尺寸。
//
// 类型以**魔数嗅探**为准，不信任客户端给的 Content-Type 或文件名后缀——
// 那两者完全由上传者控制，用它们决定文件如何被解析和提供，等于把控制权交出去。
func Inspect(data []byte) Info {
	info := Info{Mime: http.DetectContentType(data)}
	// DetectContentType 可能返回 "image/png; charset=..." 这种带参数的形式
	if i := strings.IndexByte(info.Mime, ';'); i >= 0 {
		info.Mime = strings.TrimSpace(info.Mime[:i])
	}
	info.Ext = mimeExt[info.Mime]

	// 尺寸只是锦上添花：webp / avif 没有标准库解码器，
	// 解不出来就留 0，不该因此让上传失败。
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		info.Width, info.Height = cfg.Width, cfg.Height
	}
	return info
}

// Allowed 判断该 MIME 是否允许上传。
func Allowed(mime string) bool {
	_, ok := mimeExt[mime]
	return ok
}

// AllowedList 返回支持的类型列表，用于错误提示与接口文档。
func AllowedList() []string {
	out := make([]string, 0, len(mimeExt))
	for m := range mimeExt {
		out = append(out, m)
	}
	return out
}
