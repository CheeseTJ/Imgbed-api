// Package config 加载 imgbed 的 YAML 配置，并支持用环境变量覆盖敏感项。
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"gopkg.in/yaml.v3"
)

// Config 是服务的全部配置。
type Config struct {
	Server  Server  `yaml:"server"`
	Ucenter Ucenter `yaml:"ucenter"`
	R2      R2      `yaml:"r2"`
	Limits  Limits  `yaml:"limits"`
	DB      DB      `yaml:"db"`
}

// Server 是 HTTP 服务与 CORS 配置。
type Server struct {
	Addr string `yaml:"addr"`
	// AllowedOrigins 是浏览器直连白名单，留空表示放行所有来源。
	AllowedOrigins []string `yaml:"allowed_origins"`
	// WebDir 是前端构建产物目录；存在时本进程同时托管 SPA（缺省 web/dist）。
	// 目录不存在则纯 API 模式，开发期前端继续走 Vite。
	WebDir string `yaml:"web_dir"`
}

// Ucenter 是接入用户中心所需的参数。
type Ucenter struct {
	BaseURL string `yaml:"base_url"`
	// App 必须与 Ucenter Worker 的 APPS 白名单一致，用于校验 JWT 的 aud。
	App     string `yaml:"app"`
	JwksTTL string `yaml:"jwks_ttl"`
}

// R2 是对象存储配置。
type R2 struct {
	AccountID       string `yaml:"account_id"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	Bucket          string `yaml:"bucket"`
	// Prefix 留空表示使用 bucket 根目录。
	Prefix string `yaml:"prefix"`
	// PublicBase 是图片对外直出的基址（R2 自定义域名）。
	PublicBase string `yaml:"public_base"`
	// PrivatePrefix 存放私有对象，不绑定自定义域名。
	PrivatePrefix string `yaml:"private_prefix"`
	// CFAPIToken 用于查询 bucket 的真实占用（Cloudflare 官方 usage 接口）。
	//
	// 需要一个至少有「R2 读」权限的 API Token（Cloudflare 控制台 → My Profile →
	// API Tokens 创建）。留空时 /api/v1/usage 里的 r2 字段为 null，
	// 前端回退到按图片元数据汇总的数据库口径，功能不受影响。
	CFAPIToken string `yaml:"cf_api_token"`
	// QuotaBytes 是存储配额（字节），只用作占用进度条的分母。
	// 留空按 R2 免费档的 10 GiB 计。
	QuotaBytes int64 `yaml:"quota_bytes"`
	// EndpointURL 可覆盖 S3 兼容端点，留空则按 AccountID 推导。
	// 指向本地 MinIO 做联调时会用到。
	EndpointURL string `yaml:"endpoint"`
}

// Endpoint 返回 S3 兼容端点地址。未配置时返回空串，
// 避免在诊断信息里拼出 "https://.r2.cloudflarestorage.com" 这种误导性的地址。
func (r R2) Endpoint() string {
	if v := strings.TrimSpace(r.EndpointURL); v != "" {
		return strings.TrimRight(v, "/")
	}
	if strings.TrimSpace(r.AccountID) == "" {
		return ""
	}
	return fmt.Sprintf("https://%s.r2.cloudflarestorage.com", r.AccountID)
}

// Limits 是上传与业务限制。
type Limits struct {
	MaxUploadMB       int `yaml:"max_upload_mb"`
	MaxBatch          int `yaml:"max_batch"`
	FolderMigrateWarn int `yaml:"folder_migrate_warn"`
	TrashRetainDays   int `yaml:"trash_retain_days"`
}

// DB 是元数据库（OCI HeatWave / MySQL）配置。
type DB struct {
	// DSN 直接给出时优先使用，可绕过下面的分字段拼装。
	DSN string `yaml:"dsn"`
	// Host / Port / Name / User / Password 用于拼装 DSN。
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	// TLSMode 控制是否加密连接，取值与 go-sql-driver 对齐。
	//
	// HeatWave 的内部域名用的是自签证书，所以不能配成 "true"（会强制用系统根证书校验而必然失败）。
	// 默认 preferred：有机会就用 TLS，但不校验证书 —— 等价于 mysql 客户端的 --ssl-mode=PREFERRED。
	TLSMode            string `yaml:"tls_mode"`
	MaxOpenConns       int    `yaml:"max_open_conns"`
	MaxIdleConns       int    `yaml:"max_idle_conns"`
	ConnMaxLifetimeSec int    `yaml:"conn_max_lifetime_sec"`
}

// JwksDuration 把配置里的时长字符串解析为 time.Duration，非法值回退 1 小时。
func (u Ucenter) JwksDuration() time.Duration {
	d, err := time.ParseDuration(u.JwksTTL)
	if err != nil || d <= 0 {
		return time.Hour
	}
	return d
}

// MaxUploadBytes 返回单文件大小上限（字节）。
func (l Limits) MaxUploadBytes() int64 {
	if l.MaxUploadMB <= 0 {
		return 20 << 20
	}
	return int64(l.MaxUploadMB) << 20
}

// TrashRetention 返回回收站保留时长。
func (l Limits) TrashRetention() time.Duration {
	days := l.TrashRetainDays
	if days <= 0 {
		days = 7
	}
	return time.Duration(days) * 24 * time.Hour
}

// R2Ready 报告 R2 凭据是否齐全。缺失时服务仍可启动，只是上传相关接口不可用。
func (c *Config) R2Ready() bool {
	return c.R2.AccountID != "" && c.R2.AccessKeyID != "" && c.R2.SecretAccessKey != ""
}

// ConnDSN 返回实际使用的 MySQL 连接串。
//
// 用驱动自带的 Config 拼装而不是字符串拼接，避免密码里的特殊字符破坏 DSN。
func (d DB) ConnDSN() (string, error) {
	if s := strings.TrimSpace(d.DSN); s != "" {
		return s, nil
	}
	if d.Host == "" || d.Name == "" || d.User == "" {
		return "", fmt.Errorf("db.host / db.name / db.user 均不能为空（或直接提供 db.dsn）")
	}

	port := d.Port
	if port == 0 {
		port = 3306
	}

	c := mysql.NewConfig()
	c.User = d.User
	c.Passwd = d.Password
	c.Net = "tcp"
	c.Addr = net.JoinHostPort(d.Host, strconv.Itoa(port))
	c.DBName = d.Name
	c.ParseTime = false // 时间戳统一用 BIGINT 秒，不依赖驱动的 time.Time 转换
	c.Loc = time.UTC
	c.Timeout = 10 * time.Second
	c.ReadTimeout = 30 * time.Second
	c.WriteTimeout = 30 * time.Second

	switch strings.ToLower(strings.TrimSpace(d.TLSMode)) {
	case "", "preferred":
		c.TLSConfig = "preferred"
	case "true", "verify", "verify-full":
		c.TLSConfig = "true" // 用系统根证书严格校验服务端
	case "skip-verify":
		c.TLSConfig = "skip-verify" // 用 TLS 但不校验证书
	case "false", "disabled", "off":
		// 保持零值：完全不用 TLS
	default:
		return "", fmt.Errorf("db.tls_mode 取值非法: %q（可选 preferred / true / skip-verify / false）", d.TLSMode)
	}

	return c.FormatDSN(), nil
}

// MaxOpenConnsOrDefault 返回最大连接数，0 表示未配置。
func (d DB) MaxOpenConnsOrDefault() int {
	if d.MaxOpenConns > 0 {
		return d.MaxOpenConns
	}
	return 16
}

// MaxIdleConnsOrDefault 返回最大空闲连接数，0 表示未配置。
func (d DB) MaxIdleConnsOrDefault() int {
	if d.MaxIdleConns > 0 {
		return d.MaxIdleConns
	}
	return 8
}

// ConnMaxLifetime 返回连接最大存活时长。
//
// 托管 MySQL 与中间的负载均衡常在数分钟后掐掉空闲连接，
// 设成 3 分钟可以避免拿到已被对端关闭的连接。
func (d DB) ConnMaxLifetime() time.Duration {
	if d.ConnMaxLifetimeSec > 0 {
		return time.Duration(d.ConnMaxLifetimeSec) * time.Second
	}
	return 3 * time.Minute
}

// Load 读取并校验配置文件，随后用环境变量覆盖敏感项。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	// 先铺默认值再反序列化：yaml 只覆盖文件中出现的字段。
	cfg := defaultConfig()
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	applyEnv(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultConfig() *Config {
	return &Config{
		// 8080 属常用保留端口（见端口使用规则），默认改用 18080
		Server: Server{Addr: ":18080"},
		Ucenter: Ucenter{
			BaseURL: "https://ucenter.june-t.top",
			App:     "imgbed",
			JwksTTL: "1h",
		},
		R2: R2{
			Bucket:        "image",
			PrivatePrefix: "private/",
			// 10 GiB = R2 免费档的存储配额，用作占用进度条的分母
			QuotaBytes: 10 << 30,
		},
		Limits: Limits{
			MaxUploadMB:       20,
			MaxBatch:          50,
			FolderMigrateWarn: 5000,
			TrashRetainDays:   7,
		},
		DB: DB{
			Port:    3306,
			TLSMode: "preferred",
		},
	}
}

// applyEnv 用环境变量覆盖配置，密钥一律走环境变量、不落盘。
func applyEnv(c *Config) {
	setStr(&c.Server.Addr, "IMGBED_SERVER_ADDR")
	setStr(&c.Server.WebDir, "IMGBED_WEB_DIR")
	if c.Server.WebDir == "" {
		c.Server.WebDir = "web/dist" // 缺省托管目录；指到不存在的路径即关闭托管
	}
	setStr(&c.Ucenter.BaseURL, "IMGBED_UCENTER_BASE_URL")
	setStr(&c.Ucenter.App, "IMGBED_UCENTER_APP")
	setStr(&c.R2.AccountID, "IMGBED_R2_ACCOUNT_ID")
	setStr(&c.R2.AccessKeyID, "IMGBED_R2_ACCESS_KEY_ID")
	setStr(&c.R2.SecretAccessKey, "IMGBED_R2_SECRET_ACCESS_KEY")
	setStr(&c.R2.Bucket, "IMGBED_R2_BUCKET")
	setStr(&c.R2.Prefix, "IMGBED_R2_PREFIX")
	setStr(&c.R2.PublicBase, "IMGBED_R2_PUBLIC_BASE")
	setStr(&c.R2.PrivatePrefix, "IMGBED_R2_PRIVATE_PREFIX")
	setStr(&c.R2.CFAPIToken, "IMGBED_R2_CF_API_TOKEN")
	setInt64(&c.R2.QuotaBytes, "IMGBED_R2_QUOTA_BYTES")
	setStr(&c.R2.EndpointURL, "IMGBED_R2_ENDPOINT")
	setStr(&c.DB.DSN, "IMGBED_DB_DSN")
	setStr(&c.DB.Host, "IMGBED_DB_HOST")
	setStr(&c.DB.Name, "IMGBED_DB_NAME")
	setStr(&c.DB.User, "IMGBED_DB_USER")
	setStr(&c.DB.Password, "IMGBED_DB_PASSWORD")

	if v := os.Getenv("IMGBED_ALLOWED_ORIGINS"); v != "" {
		c.Server.AllowedOrigins = splitList(v)
	}
	setInt(&c.Limits.MaxUploadMB, "IMGBED_MAX_UPLOAD_MB")
	setInt(&c.Limits.MaxBatch, "IMGBED_MAX_BATCH")
	setInt(&c.Limits.TrashRetainDays, "IMGBED_TRASH_RETAIN_DAYS")
	setInt(&c.DB.Port, "IMGBED_DB_PORT")
	setInt(&c.DB.MaxOpenConns, "IMGBED_DB_MAX_OPEN_CONNS")
	setStr(&c.DB.TLSMode, "IMGBED_DB_TLS_MODE")
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Server.Addr) == "" {
		return fmt.Errorf("server.addr 不能为空")
	}
	if strings.TrimSpace(c.Ucenter.BaseURL) == "" {
		return fmt.Errorf("ucenter.base_url 不能为空")
	}
	if strings.TrimSpace(c.Ucenter.App) == "" {
		return fmt.Errorf("ucenter.app 不能为空（需与 Ucenter 的 APPS 白名单一致）")
	}
	if strings.TrimSpace(c.R2.Bucket) == "" {
		return fmt.Errorf("r2.bucket 不能为空")
	}
	// 提前校验 DSN 可拼装，避免启动后才发现配置笔误
	if _, err := c.DB.ConnDSN(); err != nil {
		return err
	}
	if c.Limits.MaxUploadMB <= 0 {
		return fmt.Errorf("limits.max_upload_mb 必须为正数")
	}
	if c.Limits.MaxBatch <= 0 {
		return fmt.Errorf("limits.max_batch 必须为正数")
	}
	if c.Limits.TrashRetainDays <= 0 {
		return fmt.Errorf("limits.trash_retain_days 必须为正数")
	}
	return nil
}

// setStr 非空时用环境变量覆盖目标字段。
func setStr(dst *string, key string) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		*dst = v
	}
}

// setInt 可解析为正整数时用环境变量覆盖目标字段。
func setInt(dst *int, key string) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err == nil && n > 0 {
		*dst = n
	}
}

// setInt64 与 setInt 同理，用于需要 64 位范围的配置项（如字节数）。
// R2 免费档的配额是 10 GiB，早已超出 int 的安全范围。
func setInt64(dst *int64, key string) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err == nil && n > 0 {
		*dst = n
	}
}

// splitList 按逗号切分并去掉空白项。
func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
