package r2

import (
	"testing"

	"imgbed/internal/config"
)

// TestNormalizePrefix 锁定前缀规整规则。
//
// 前缀要么为空、要么以 / 结尾，这样 Key() 拼接时不会出现
// 「image//u/...」或「imageu/...」这两种经典错误。
func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"":         "",
		"/":        "",
		"image":    "image/",
		"image/":   "image/",
		"/image/":  "image/",
		"  a/b  ":  "a/b/",
		"//x//y//": "x/y/",
	}
	for in, want := range cases {
		if got := normalizePrefix(in); got != want {
			t.Errorf("normalizePrefix(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestKeyAndPublicURL 验证 key 拼接与前缀的组合。
func TestKeyAndPublicURL(t *testing.T) {
	c := &Client{prefix: normalizePrefix("image"), publicBase: "https://img.june-t.top"}

	if got := c.Key("u/abc/x.jpg"); got != "image/u/abc/x.jpg" {
		t.Errorf("Key = %q", got)
	}
	if got := c.Key("/u/abc/x.jpg"); got != "image/u/abc/x.jpg" {
		t.Errorf("前导斜杠应被归一，实际 %q", got)
	}
	if got := c.PublicURL("u/abc/x.jpg"); got != "https://img.june-t.top/image/u/abc/x.jpg" {
		t.Errorf("PublicURL = %q", got)
	}

	// 前缀为空时不该凭空多出斜杠
	root := &Client{prefix: normalizePrefix(""), publicBase: "https://img.june-t.top"}
	if got := root.Key("u/abc/x.jpg"); got != "u/abc/x.jpg" {
		t.Errorf("空前缀 Key = %q", got)
	}
}

// TestEndpoint 覆盖端点推导与覆盖。
func TestEndpoint(t *testing.T) {
	acct := config.R2{AccountID: "abc123"}
	if got := acct.Endpoint(); got != "https://abc123.r2.cloudflarestorage.com" {
		t.Errorf("Endpoint = %q", got)
	}

	// 显式给出端点时（例如指向本地 MinIO）应优先使用，且去掉尾斜杠
	custom := config.R2{AccountID: "abc123", EndpointURL: "http://127.0.0.1:9000/"}
	if got := custom.Endpoint(); got != "http://127.0.0.1:9000" {
		t.Errorf("自定义 Endpoint = %q", got)
	}
}

// TestNewRejectsIncompleteConfig 确认凭据不全时明确报错，
// 而不是构造出一个每次调用都 403 的客户端。
func TestNewRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]config.R2{
		"缺 account 与 endpoint": {AccessKeyID: "k", SecretAccessKey: "s", Bucket: "b"},
		"缺 access key":         {AccountID: "a", SecretAccessKey: "s", Bucket: "b"},
		"缺 secret":             {AccountID: "a", AccessKeyID: "k", Bucket: "b"},
		"缺 bucket":             {AccountID: "a", AccessKeyID: "k", SecretAccessKey: "s"},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s：应当报错但成功了", name)
		}
	}

	// 配齐后可正常构造
	if _, err := New(config.R2{
		AccountID: "a", AccessKeyID: "k", SecretAccessKey: "s", Bucket: "image",
	}); err != nil {
		t.Errorf("配置完整时不该报错: %v", err)
	}
}
