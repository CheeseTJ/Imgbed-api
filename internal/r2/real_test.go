package r2

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"imgbed/internal/config"
)

// TestRealR2 是对**真实 R2** 的联调测试，只在提供了凭据时才运行。
//
// 之所以默认跳过：单元测试不该依赖网络和真实凭据，否则在任何 CI 上都会红。
// 但「代码通过了假 S3」和「代码能在真 R2 上跑」是两回事——
// 签名、分块、错误码这些细节只有真环境才能验证。
//
// 运行方式：
//
//	. ./.env.sh && IMGBED_R2_ACCESS_KEY_ID=... IMGBED_R2_SECRET_ACCESS_KEY=... \
//	  IMGBED_R2_ACCOUNT_ID=... go test ./internal/r2/ -run TestRealR2 -v
func TestRealR2(t *testing.T) {
	ak := os.Getenv("IMGBED_R2_ACCESS_KEY_ID")
	sk := os.Getenv("IMGBED_R2_SECRET_ACCESS_KEY")
	acct := os.Getenv("IMGBED_R2_ACCOUNT_ID")
	bucket := os.Getenv("IMGBED_R2_BUCKET")
	if ak == "" || sk == "" || acct == "" {
		t.Skip("未提供 R2 凭据，跳过真实环境测试")
	}
	if bucket == "" {
		bucket = "image"
	}

	c, err := New(config.R2{
		AccountID: acct, AccessKeyID: ak, SecretAccessKey: sk,
		Bucket: bucket, PublicBase: "https://img.june-t.top",
		// 用独立前缀，避免污染真实命名空间
		Prefix: ".selftest",
	})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping 失败（凭据或 bucket 名可能不对）: %v", err)
	}
	t.Log("✓ 凭据可用，bucket 可达")

	key := "probe/roundtrip.txt"
	body := []byte("imgbed r2 roundtrip probe " + time.Now().UTC().Format(time.RFC3339))
	defer func() {
		// 无论断言成败都清掉探针对象
		cleanupCtx, cc := context.WithTimeout(context.Background(), 20*time.Second)
		defer cc()
		if err := c.Delete(cleanupCtx, key); err != nil {
			t.Logf("清理探针对象失败（请手工删除）: %v", err)
		}
	}()

	if err := c.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "text/plain"); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	t.Log("✓ 上传成功")

	exists, err := c.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists 失败: %v", err)
	}
	if !exists {
		t.Fatal("刚上传的对象却判定为不存在")
	}
	t.Log("✓ 存在性判断正确")

	obj, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	got, _ := io.ReadAll(obj.Body)
	obj.Body.Close()

	if !bytes.Equal(got, body) {
		t.Fatalf("读回内容与写入不一致：\n写入 %q\n读回 %q", body, got)
	}
	if obj.ContentType != "text/plain" {
		t.Errorf("ContentType = %q，期望 text/plain", obj.ContentType)
	}
	t.Log("✓ 内容与类型都正确往返")

	// 公开直出验证：这是「读图不经过后端」整条设计的最后一环。
	// S3 侧一切正常但自定义域名没生效的情况是存在的（DNS、证书、路由各可能出问题），
	// 只有真的发一次 HTTP 请求才能确认。
	publicURL := c.PublicURL(key)
	resp, err := http.Get(publicURL)
	if err != nil {
		t.Fatalf("通过自定义域名访问失败（检查域名绑定与 DNS）: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("自定义域名返回 %d，期望 200（地址 %s）", resp.StatusCode, publicURL)
	}
	pubBody, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(pubBody, body) {
		t.Errorf("公开地址返回的内容与写入不一致：%q", pubBody)
	}
	if cc := resp.Header.Get("Cache-Control"); cc == "" {
		t.Error("响应缺少 Cache-Control —— CDN 会反复回源，失去直出的意义")
	} else {
		t.Logf("✓ 自定义域名直出正常，Cache-Control=%q", cc)
	}

	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	exists, err = c.Exists(ctx, key)
	if err != nil {
		t.Fatalf("删除后 Exists 失败: %v", err)
	}
	if exists {
		t.Error("删除后对象仍然存在")
	}
	t.Log("✓ 删除生效")

	t.Logf("公开访问地址形如：%s", c.PublicURL("u/<owner>/<ulid>.jpg"))
}
