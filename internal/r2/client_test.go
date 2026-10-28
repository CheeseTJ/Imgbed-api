package r2

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"imgbed/internal/config"
)

// fakeS3 是一个够用的假 S3 服务：只实现本项目用到的几个动作。
//
// 之所以自己起一个而不是用 MinIO：测试不该依赖外部容器，
// 而且这里要验证的恰恰是「我们发出的请求长什么样」——
// 路径是否 path-style、key 是否带上配置前缀、404 是否被正确识别。
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	paths   []string // 记录收到的请求路径，用于断言寻址方式
	deleted []string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, types: map[string]string{}}
}

func (f *fakeS3) record(p string) {
	f.mu.Lock()
	f.paths = append(f.paths, p)
	f.mu.Unlock()
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.record(r.URL.Path)

	// path-style：/{bucket}[/{key...}]
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	bucket := parts[0]
	if bucket != "image" && bucket != "prefixed" {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}

	// 桶级操作：ListObjectsV2
	if len(parts) == 1 || parts[1] == "" {
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
				`<ListBucketResult><Name>image</Name><KeyCount>0</KeyCount>`+
				`<MaxKeys>1</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`)
			return
		}
		// 桶级 HEAD = HeadBucket，Ping 用它做可达性探测（Class B，不花 Class A）
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}

	key := parts[1]

	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.objects[key] = body
		f.types[key] = r.Header.Get("Content-Type")
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		f.mu.Lock()
		body, ok := f.objects[key]
		ct := f.types[key]
		f.mu.Unlock()
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", itoa(len(body)))
		w.Write(body)

	case http.MethodHead:
		f.mu.Lock()
		_, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NotFound")
			return
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.deleted = append(f.deleted, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "not implemented", http.StatusNotImplemented)
	}
}

// writeS3Error 返回带 S3 错误码的响应体，让 SDK 能解析出类型化错误。
func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>`+code+`</Code></Error>`)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func newTestClient(t *testing.T, srv *httptest.Server, prefix string) *Client {
	t.Helper()
	c, err := New(config.R2{
		EndpointURL:     srv.URL,
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
		Bucket:          "image",
		Prefix:          prefix,
		PublicBase:      "https://img.june-t.top",
	})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	return c
}

// TestPutGetDeleteRoundTrip 覆盖最核心的一条链路。
func TestPutGetDeleteRoundTrip(t *testing.T) {
	f := newFakeS3()
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newTestClient(t, srv, "")
	ctx := context.Background()
	content := []byte("fake-png-bytes")

	if err := c.Put(ctx, "u/abc/pic.png", bytes.NewReader(content), int64(len(content)), "image/png"); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}

	obj, err := c.Get(ctx, "u/abc/pic.png")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	defer obj.Body.Close()

	got, _ := io.ReadAll(obj.Body)
	if !bytes.Equal(got, content) {
		t.Errorf("读回内容不一致：%q", got)
	}
	if obj.ContentType != "image/png" {
		t.Errorf("ContentType = %q", obj.ContentType)
	}
	if obj.Size != int64(len(content)) {
		t.Errorf("Size = %d，期望 %d", obj.Size, len(content))
	}

	if err := c.Delete(ctx, "u/abc/pic.png"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	exists, err := c.Exists(ctx, "u/abc/pic.png")
	if err != nil {
		t.Fatalf("Exists 失败: %v", err)
	}
	if exists {
		t.Error("删除后对象仍然存在")
	}
}

// TestPrefixAppliedToKey 确认配置前缀真的进了请求路径。
func TestPrefixAppliedToKey(t *testing.T) {
	f := newFakeS3()
	srv := httptest.NewServer(f)
	defer srv.Close()

	// 前缀 image/ 会让实际路径变成 /image/image/u/... （bucket 名与前缀相同只是巧合）
	c := newTestClient(t, srv, "image")
	ctx := context.Background()
	body := []byte("x")

	if err := c.Put(ctx, "u/a.png", bytes.NewReader(body), 1, "image/png"); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for k := range f.objects {
		if k == "image/u/a.png" {
			found = true
		}
	}
	if !found {
		t.Errorf("前缀未生效，实际收到 key：%v", keysOf(f.objects))
	}
}

// TestPathStyleAddressing 锁定寻址方式。
//
// R2 不支持 virtual-hosted-style。一旦有人把 UsePathStyle 改成 false，
// 请求路径会变成 /{key} 而 bucket 挪到 Host 头里——这里就会挂。
func TestPathStyleAddressing(t *testing.T) {
	f := newFakeS3()
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newTestClient(t, srv, "")
	ctx := context.Background()
	body := []byte("y")

	if err := c.Put(ctx, "u/b.png", bytes.NewReader(body), 1, "image/png"); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) == 0 {
		t.Fatal("没有记录到任何请求")
	}
	first := f.paths[0]
	if !strings.HasPrefix(first, "/image/") {
		t.Errorf("请求路径 %q 不是 path-style（应以 /image/ 开头）", first)
	}
}

// TestExistsNotFound 确认 404 被识别为「不存在」，而不是报错。
//
// 这条分支很关键：目录迁移要靠 Exists 判断某个对象是否已搬过，
// 若把 404 当成错误往外抛，重入逻辑就会直接失败。
func TestExistsNotFound(t *testing.T) {
	f := newFakeS3()
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newTestClient(t, srv, "")
	exists, err := c.Exists(context.Background(), "u/never.png")
	if err != nil {
		t.Fatalf("Exists 对不存在的对象不该报错: %v", err)
	}
	if exists {
		t.Error("不存在的对象被判定为存在")
	}
}

// TestDeleteMissingIsOK 确认删除不存在的对象也算成功（幂等）。
func TestDeleteMissingIsOK(t *testing.T) {
	f := newFakeS3()
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newTestClient(t, srv, "")
	if err := c.Delete(context.Background(), "u/ghost.png"); err != nil {
		t.Errorf("删除不存在的对象应当成功，实际报错: %v", err)
	}
}

// TestPingReportsBucketProblem 确认 bucket 不可达时 Ping 会报错——
// 健康检查靠它回答「R2 到底通不通」。
func TestPingReportsBucketProblem(t *testing.T) {
	f := newFakeS3()
	srv := httptest.NewServer(f)
	defer srv.Close()

	// bucket 名不在 fake 的允许列表里，List 会返回 NoSuchBucket
	c, err := New(config.R2{
		EndpointURL: srv.URL, AccessKeyID: "k", SecretAccessKey: "s",
		Bucket: "does-not-exist", PublicBase: "https://img.june-t.top",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err == nil {
		t.Error("bucket 不可用时 Ping 应当报错")
	}

	// 正常 bucket 应当通过
	ok := newTestClient(t, srv, "")
	if err := ok.Ping(context.Background()); err != nil {
		t.Errorf("正常 bucket 的 Ping 不该失败: %v", err)
	}
}

// TestPutSetsImmutableCacheControl 确认上传时写入了长缓存头。
// 这是「读图几乎不消耗 R2 额度」的前提，改错了会让 CDN 反复回源。
func TestPutSetsImmutableCacheControl(t *testing.T) {
	var gotCC string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCC = r.Header.Get("Cache-Control")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	body := []byte("z")
	if err := c.Put(context.Background(), "u/c.png", bytes.NewReader(body), 1, "image/png"); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	if !strings.Contains(gotCC, "immutable") {
		t.Errorf("Cache-Control = %q，应含 immutable", gotCC)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
