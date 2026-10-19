package imageinfo

import (
	"bytes"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("生成测试 PNG 失败: %v", err)
	}
	return buf.Bytes()
}

// TestInspectRecognizesFormats 覆盖支持的类型，并验证尺寸被正确读出。
func TestInspectRecognizesFormats(t *testing.T) {
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, image.NewRGBA(image.Rect(0, 0, 7, 3)), nil); err != nil {
		t.Fatal(err)
	}
	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, image.NewRGBA(image.Rect(0, 0, 5, 4)), nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		data      []byte
		wantMime  string
		wantExt   string
		wantW     int
		wantH     int
		wantAllow bool
	}{
		{"PNG 12x9", encodePNG(t, 12, 9), "image/png", ".png", 12, 9, true},
		{"JPEG 7x3", jpegBuf.Bytes(), "image/jpeg", ".jpg", 7, 3, true},
		{"GIF 5x4", gifBuf.Bytes(), "image/gif", ".gif", 5, 4, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Inspect(tc.data)
			if got.Mime != tc.wantMime {
				t.Errorf("Mime = %q，期望 %q", got.Mime, tc.wantMime)
			}
			if got.Ext != tc.wantExt {
				t.Errorf("Ext = %q，期望 %q", got.Ext, tc.wantExt)
			}
			if got.Width != tc.wantW || got.Height != tc.wantH {
				t.Errorf("尺寸 = %dx%d，期望 %dx%d", got.Width, got.Height, tc.wantW, tc.wantH)
			}
			if Allowed(got.Mime) != tc.wantAllow {
				t.Errorf("Allowed(%q) = %v，期望 %v", got.Mime, !tc.wantAllow, tc.wantAllow)
			}
		})
	}
}

// TestInspectRejectsNonImage 确认类型判断基于内容魔数而非文件名。
//
// 这是安全边界：若按客户端声称的类型决定如何提供文件，
// 攻击者就能把 HTML/脚本伪装成 .png 传上来再诱导访问。
func TestInspectRejectsNonImage(t *testing.T) {
	payloads := [][]byte{
		[]byte("<html><script>alert(1)</script></html>"),
		[]byte("<?xml version=\"1.0\"?><svg xmlns=\"http://www.w3.org/2000/svg\"><script/></svg>"),
		[]byte("just plain text pretending to be a picture"),
		[]byte{},
	}

	for i, p := range payloads {
		info := Inspect(p)
		if Allowed(info.Mime) {
			t.Errorf("第 %d 个载荷被误判为图片：mime=%q", i, info.Mime)
		}
		if info.Ext != "" {
			t.Errorf("第 %d 个载荷不该有扩展名，得到 %q", i, info.Ext)
		}
	}
}

// TestInspectStripsMimeParameters 确认带参数的 MIME 会被规整。
func TestInspectStripsMimeParameters(t *testing.T) {
	info := Inspect(encodePNG(t, 2, 2))
	if info.Mime != "image/png" {
		t.Fatalf("Mime 应被规整为 image/png，实际 %q", info.Mime)
	}
}
