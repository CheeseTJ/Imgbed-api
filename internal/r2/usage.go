package r2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cfAPIBase 是 Cloudflare 官方 API 的基址。
const cfAPIBase = "https://api.cloudflare.com/client/v4"

// usageCacheTTL 缓存官方接口的结果。
//
// 这个接口每次调用计 1 次 Class B；侧栏的用量条不需要秒级实时，
// 缓存一分钟既省额度，也避免前端在上传后连续刷新时打出突发请求。
const usageCacheTTL = time.Minute

// cfHTTP 是访问 Cloudflare 官方 API 的专用客户端。
//
// 刻意不复用 http.DefaultClient：没有超时的客户端在对方不响应时
// 会把调用方（这里是 /api/v1/usage 请求）一起拖死。
var cfHTTP = &http.Client{Timeout: 15 * time.Second}

// BucketUsage 是 R2 bucket 的真实占用。
//
// UsedBytes 采用 Cloudflare 的**计费口径**：对象本体 + 元数据。
// 这与 dashboard「指标」页那个数、也与账单一致；
// 自己遍历对象求和会漏掉元数据，而且要多花一次 Class A。
type BucketUsage struct {
	UsedBytes     int64
	PayloadBytes  int64
	MetadataBytes int64
	ObjectCount   int64
	// QuotaBytes 是配置的配额，原样透传给前端做进度条分母
	QuotaBytes  int64
	MeasuredAt  string
}

// BucketUsageRow 是账户里单个 bucket 的占用快照。
type BucketUsageRow struct {
	Name          string
	UsedBytes     int64 // 计费口径：本体 + 元数据
	PayloadBytes  int64
	MetadataBytes int64
	ObjectCount   int64
}

// AccountUsage 是整个账户的 R2 占用汇总——**所有 bucket**，不止图床这一个。
//
// 侧栏的存储条只统计当前 bucket（图床自己的账）；这个汇总给 hover 面板用，
// 让用户能对上 dashboard 上那个账户级数字（比如 music 桶的 3.9 GB）。
// 两边口径不同，差出来不是 bug，是「这个桶」和「整个账户」的区别。
type AccountUsage struct {
	Buckets []BucketUsageRow

	TotalUsed     int64
	TotalPayload  int64
	TotalMetadata int64
	TotalObjects  int64
}

// cfUsageResult 是官方 usage 接口的返回体。
//
// **所有数值都是字符串**，需要自己转——照注释里那句话抄错就会拿到 0。
type cfUsageResult struct {
	End           string `json:"end"`
	PayloadSize   string `json:"payloadSize"`
	MetadataSize  string `json:"metadataSize"`
	ObjectCount   string `json:"objectCount"`
}

// BucketUsage 查询当前 bucket 的真实占用。
//
// 走 Cloudflare 官方的 usage 接口（dashboard「指标」页那个数的来源），
// 而不是自己遍历对象求和：官方口径与账单一致、结果是实时的，
// 还省掉「遍历 N 个对象」的开销与一次 Class A。
//
// 结果带一分钟缓存；需要 r2.cf_api_token 与 r2.account_id 两项配置。
func (c *Client) BucketUsage(ctx context.Context) (*BucketUsage, error) {
	if c.cfToken == "" {
		return nil, errors.New("未配置 r2.cf_api_token，无法查询真实占用")
	}
	if c.accountID == "" {
		return nil, errors.New("未配置 r2.account_id，无法查询真实占用")
	}

	c.usageMu.Lock()
	if c.usageCache != nil && time.Since(c.usageCachedAt) < usageCacheTTL {
		u := *c.usageCache
		c.usageMu.Unlock()
		return &u, nil
	}
	c.usageMu.Unlock()

	u, err := c.fetchUsage(ctx)
	if err != nil {
		return nil, err
	}

	c.usageMu.Lock()
	c.usageCache = u
	c.usageCachedAt = time.Now()
	c.usageMu.Unlock()
	return u, nil
}

// fetchUsage 查询当前配置 bucket 的占用。
//
// 与 BucketUsage 分开是为了让缓存逻辑只在后者的路径上生效，
// 失败的结果**不进缓存**——否则一次 429 会让后续一分钟里所有查询都拿到旧值或空值。
func (c *Client) fetchUsage(ctx context.Context) (*BucketUsage, error) {
	r, err := c.fetchUsageRaw(ctx, c.bucket)
	if err != nil {
		return nil, err
	}

	payload, metadata, objects, err := parseUsage(r)
	if err != nil {
		return nil, err
	}

	quota := c.quotaBytes
	if quota <= 0 {
		quota = defaultQuotaBytes
	}

	return &BucketUsage{
		UsedBytes:     payload + metadata, // 计费口径：本体 + 元数据
		PayloadBytes:  payload,
		MetadataBytes: metadata,
		ObjectCount:   objects,
		QuotaBytes:    quota,
		MeasuredAt:    r.End,
	}, nil
}

// fetchUsageRaw 调用官方 usage 接口并解析出原始字段。
//
// bucket 名由调用方传入而不是写死 c.bucket，这样账户级汇总
// 能对每个桶复用同一条请求路径。失败不缓存，由上层决定怎么处理。
func (c *Client) fetchUsageRaw(ctx context.Context, bucket string) (*cfUsageResult, error) {
	reqURL := fmt.Sprintf("%s/accounts/%s/r2/buckets/%s/usage",
		cfAPIBase, c.accountID, bucket)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfToken)

	res, err := cfHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Cloudflare usage 接口失败: %w", err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 usage 响应失败: %w", err)
	}

	// 状态码单独分流：token 失效（401/403）和限流（429）在日志里长得不一样，
	// 混成一句「接口失败」会让排障多绕一圈。
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("Cloudflare 拒绝了请求（%d）：检查 cf_api_token 是否有 R2 读权限", res.StatusCode)
	case http.StatusTooManyRequests:
		return nil, errors.New("Cloudflare 限流（429），稍后重试")
	case http.StatusNotFound:
		return nil, fmt.Errorf("bucket %q 不存在或不在该账户下", bucket)
	}

	var body struct {
		Success bool           `json:"success"`
		Errors  []cfAPIError   `json:"errors"`
		Result  *cfUsageResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("解析 usage 响应失败（%d）: %v", res.StatusCode, err)
	}
	if res.StatusCode != http.StatusOK || !body.Success || body.Result == nil {
		detail := ""
		if len(body.Errors) > 0 {
			detail = body.Errors[0].Message
		}
		return nil, fmt.Errorf("usage 接口返回异常（%d）: %s", res.StatusCode, detail)
	}
	return body.Result, nil
}

// parseUsage 把官方返回的字符串数字转成 int64。
func parseUsage(r *cfUsageResult) (payload, metadata, objects int64, err error) {
	payload, err = strconv.ParseInt(strings.TrimSpace(r.PayloadSize), 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("payloadSize %q 不是合法数字: %v", r.PayloadSize, err)
	}
	metadata, err = strconv.ParseInt(strings.TrimSpace(r.MetadataSize), 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("metadataSize %q 不是合法数字: %v", r.MetadataSize, err)
	}
	objects, err = strconv.ParseInt(strings.TrimSpace(r.ObjectCount), 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("objectCount %q 不是合法数字: %v", r.ObjectCount, err)
	}
	return payload, metadata, objects, nil
}

// defaultQuotaBytes 是 R2 免费档的存储配额。
// 注意是十进制 10 GB（10^10 字节）——Cloudflare 的计费与免费额度都按 SI 口径，
// 写成 10<<30（10 GiB）会凭空多出 7.4% 的分母，进度条会偏乐观。
const defaultQuotaBytes = 10_000_000_000

// cfAPIError 是官方错误体里的一条错误。
type cfAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

/* ==================== 账户级汇总（全部 bucket） ==================== */

// AccountUsage 汇总账户下**所有** bucket 的占用。
//
// 与 BucketUsage 共用同一个缓存窗口：一次汇总 = 1 次列举 + N 次 usage，
// 不缓存的话每次 /me 都要打这么多 Class B，就太费了。
// 任何桶查询失败都整体报错——宁可让 hover 面板隐藏，也不给一个对不上
// dashboard 的残缺合计数。
func (c *Client) AccountUsage(ctx context.Context) (*AccountUsage, error) {
	if c.cfToken == "" {
		return nil, errors.New("未配置 r2.cf_api_token，无法查询账户级占用")
	}
	if c.accountID == "" {
		return nil, errors.New("未配置 r2.account_id，无法查询账户级占用")
	}

	c.usageMu.Lock()
	if c.acctCache != nil && time.Since(c.acctCachedAt) < usageCacheTTL {
		u := *c.acctCache
		c.usageMu.Unlock()
		return &u, nil
	}
	c.usageMu.Unlock()

	u, err := c.fetchAccountUsage(ctx)
	if err != nil {
		return nil, err
	}

	c.usageMu.Lock()
	c.acctCache = u
	c.acctCachedAt = time.Now()
	c.usageMu.Unlock()
	return u, nil
}

// fetchAccountUsage 列出所有 bucket，并发拉取每个的 usage 后汇总。
func (c *Client) fetchAccountUsage(ctx context.Context) (*AccountUsage, error) {
	names, err := c.listBucketNames(ctx)
	if err != nil {
		return nil, err
	}

	// 并发拉取。个人账户通常只有个位数桶，上限 8 是防御性的：
	// 桶多了也不至于把请求排成长队或触发 429。
	const maxWorkers = 8
	sem := make(chan struct{}, maxWorkers)

	rows := make([]BucketUsageRow, 0, len(names))
	var mu sync.Mutex
	var firstErr error

	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r, err := c.fetchUsageRaw(ctx, name)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("bucket %s: %w", name, err)
				}
				mu.Unlock()
				return
			}
			payload, metadata, objects, err := parseUsage(r)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("bucket %s: %w", name, err)
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			rows = append(rows, BucketUsageRow{
				Name:          name,
				UsedBytes:     payload + metadata,
				PayloadBytes:  payload,
				MetadataBytes: metadata,
				ObjectCount:   objects,
			})
			mu.Unlock()
		}(name)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	// 按名字排序，前端展示顺序稳定（并发 append 的顺序是不确定的）
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	sum := &AccountUsage{Buckets: rows}
	for _, r := range rows {
		sum.TotalUsed += r.UsedBytes
		sum.TotalPayload += r.PayloadBytes
		sum.TotalMetadata += r.MetadataBytes
		sum.TotalObjects += r.ObjectCount
	}
	return sum, nil
}

// listBucketNames 列出账户下所有 bucket 的名字，自动处理游标翻页。
func (c *Client) listBucketNames(ctx context.Context) ([]string, error) {
	var names []string
	cursor := ""

	for page := 0; ; page++ {
		reqURL := fmt.Sprintf("%s/accounts/%s/r2/buckets?per_page=100", cfAPIBase, c.accountID)
		if cursor != "" {
			reqURL += "&cursor=" + url.QueryEscape(cursor)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.cfToken)

		res, err := cfHTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("请求 bucket 列表失败: %w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("读取 bucket 列表失败: %w", readErr)
		}

		// 翻页游标在 result_info 里，is_truncated 决定要不要继续
		var body struct {
			Success bool       `json:"success"`
			Errors  []cfAPIError `json:"errors"`
			Result  struct {
				Buckets []struct {
					Name string `json:"name"`
				} `json:"buckets"`
				ResultInfo struct {
					Cursor      string `json:"cursor"`
					IsTruncated bool   `json:"is_truncated"`
				} `json:"result_info"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("解析 bucket 列表失败（%d）: %v", res.StatusCode, err)
		}
		if res.StatusCode != http.StatusOK || !body.Success {
			detail := ""
			if len(body.Errors) > 0 {
				detail = body.Errors[0].Message
			}
			return nil, fmt.Errorf("bucket 列表返回异常（%d）: %s", res.StatusCode, detail)
		}

		for _, b := range body.Result.Buckets {
			names = append(names, b.Name)
		}
		if !body.Result.ResultInfo.IsTruncated {
			return names, nil
		}
		cursor = body.Result.ResultInfo.Cursor
		if cursor == "" || page > 100 { // 防御：游标异常时别死循环
			return names, nil
		}
	}
}
