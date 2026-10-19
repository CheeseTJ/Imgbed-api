// Package r2 封装 Cloudflare R2 的对象读写。
//
// R2 兼容 S3 协议，但有三个必须注意的点，配错任何一个都会得到 403 或签名错误：
//
//  1. region 固定填 "auto" —— R2 不分区，随便填别的会被签名校验拒绝。
//  2. 必须用 path-style 寻址 —— R2 不支持 virtual-hosted-style
//     （即 <bucket>.<account>.r2.cloudflarestorage.com 那种写法）。
//  3. endpoint 形如 https://<account-id>.r2.cloudflarestorage.com。
//
// 本项目对 R2 只做五件事：Put / Get / Delete / Copy / List，
// 因此这里只暴露最小接口，不把 SDK 透出去。
package r2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"imgbed/internal/config"
)

// immutableCacheControl 是公开对象的缓存策略。
//
// key 里含不可变的 ULID，内容一旦写入就不会再变，所以可以放心长缓存：
// 图片由 CDN 边缘直接返回，既快又几乎不消耗 R2 的读请求额度。
const immutableCacheControl = "public, max-age=31536000, immutable"

// Client 是对 R2 的最小封装。
type Client struct {
	s3         *s3.Client
	bucket     string
	prefix     string
	publicBase string
	privatePfx string

	// ---- Cloudflare 官方 API（查 bucket 真实用量）----
	accountID  string
	cfToken    string
	quotaBytes int64

	// 用量结果的短时缓存。官方接口每次调用计 1 次 Class B，
	// 缓存一分钟既省额度，也挡住前端在上传后连续刷新造成的突发请求。
	usageMu       sync.Mutex
	usageCache    *BucketUsage
	usageCachedAt time.Time

	// 账户级用量（全部 bucket 汇总）的缓存，与上面共用 usageMu。
	// 构造成本更高（1 次列举 + N 次 usage），缓存价值也更大。
	acctCache    *AccountUsage
	acctCachedAt time.Time
}

// New 按配置构造客户端。凭据不全时返回错误，调用方据此决定是否禁用上传能力。
func New(cfg config.R2) (*Client, error) {
	if cfg.AccountID == "" && cfg.EndpointURL == "" {
		return nil, errors.New("R2 account_id 未配置")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("R2 access_key_id / secret_access_key 未配置")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("R2 bucket 未配置")
	}

	awsCfg := aws.Config{
		Region:      "auto", // R2 不分区，必须填 auto
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint())
		o.UsePathStyle = true // R2 不支持 virtual-hosted-style
	})

	return &Client{
		s3:         client,
		bucket:     cfg.Bucket,
		prefix:     normalizePrefix(cfg.Prefix),
		publicBase: strings.TrimRight(cfg.PublicBase, "/"),
		privatePfx: normalizePrefix(cfg.PrivatePrefix),
		accountID:  strings.TrimSpace(cfg.AccountID),
		cfToken:    strings.TrimSpace(cfg.CFAPIToken),
		quotaBytes: cfg.QuotaBytes,
	}, nil
}

// normalizePrefix 把前缀规整成「要么为空、要么以 / 结尾」的形态，
// 这样调用方拼接时不必操心分隔符重复或缺失。
func normalizePrefix(p string) string {
	p = strings.TrimSpace(p)
	// 先折叠重复斜杠再裁首尾：只做 Trim 的话 "//x//y//" 会留下中间的 "//"，
	// 拼出来的 key 就会带上一段空路径。
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// Key 把「业务相对路径」拼上配置前缀，得到真实的 R2 对象 key。
func (c *Client) Key(rel string) string {
	return c.prefix + strings.TrimPrefix(rel, "/")
}

// PublicURL 返回对象的对外访问地址（走 R2 自定义域名，经 Cloudflare CDN）。
// 私有对象不该走这个地址——它没有鉴权。
func (c *Client) PublicURL(rel string) string {
	return c.publicBase + "/" + c.Key(rel)
}

// PrivatePrefix 返回私有对象前缀（不含 bucket 前缀）。
func (c *Client) PrivatePrefix() string { return c.privatePfx }

// Bucket 返回 bucket 名，供日志与健康检查展示。
func (c *Client) Bucket() string { return c.bucket }

// Object 是一次读取的结果。
type Object struct {
	Body        io.ReadCloser
	ContentType string
	Size        int64
}

// Put 上传一个对象。size 必须准确给出，R2 会用它做长度校验。
func (c *Client) Put(ctx context.Context, rel string, body io.Reader, size int64, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(c.Key(rel)),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
		CacheControl:  aws.String(immutableCacheControl),
	})
	if err != nil {
		return fmt.Errorf("上传对象 %s 失败: %w", rel, err)
	}
	return nil
}

// Get 读取一个对象。调用方负责关闭返回的 Body。
func (c *Client) Get(ctx context.Context, rel string) (*Object, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.Key(rel)),
	})
	if err != nil {
		return nil, fmt.Errorf("读取对象 %s 失败: %w", rel, err)
	}

	obj := &Object{Body: out.Body, Size: aws.ToInt64(out.ContentLength)}
	if out.ContentType != nil {
		obj.ContentType = *out.ContentType
	}
	return obj, nil
}

// Delete 删除一个对象。对象本来就不存在也算成功——删除天然幂等，
// 报错只会让「清理回收站」这类批处理变得难以重入。
func (c *Client) Delete(ctx context.Context, rel string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.Key(rel)),
	})
	if err != nil {
		return fmt.Errorf("删除对象 %s 失败: %w", rel, err)
	}
	return nil
}

// Copy 复制对象（同 bucket 内）。用于目录重命名/移动时的批量迁移。
func (c *Client) Copy(ctx context.Context, srcRel, dstRel string) error {
	src := c.bucket + "/" + c.Key(srcRel)
	_, err := c.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(c.bucket),
		Key:               aws.String(c.Key(dstRel)),
		CopySource:        aws.String(src),
		MetadataDirective: types.MetadataDirectiveCopy,
	})
	if err != nil {
		return fmt.Errorf("复制对象 %s → %s 失败: %w", srcRel, dstRel, err)
	}
	return nil
}

// Exists 判断对象是否存在。
//
// 用 HeadObject 而不是 List：一次 HEAD 就能确定单个对象的存在性，
// 而 List 在目录很大时要翻很多页。
func (c *Client) Exists(ctx context.Context, rel string) (bool, error) {
	_, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.Key(rel)),
	})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// Ping 用一次最小代价的调用验证凭据与 bucket 是否真的可用。
// 健康检查用它来回答「R2 到底通不通」，而不是只看配置里有没有填值。
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(c.bucket),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return fmt.Errorf("访问 R2 bucket %s 失败: %w", c.bucket, err)
	}
	return nil
}

// isNotFound 判断错误是否是「对象不存在」。
//
// 这里要同时看 S3 的类型化错误和裸 HTTP 状态码：R2 在部分路径上
// 只返回 404 而不带标准的 NoSuchKey 错误码。
func isNotFound(err error) bool {
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return true
		}
	}

	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404 {
		return true
	}
	return false
}
