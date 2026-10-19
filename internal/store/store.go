// Package store 封装元数据库：连接、建表与查询。
//
// 元数据存放在 OCI HeatWave（MySQL 兼容）中；对象二进制存在 Cloudflare R2 里。
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql" // 注册 "mysql" 驱动

	"imgbed/internal/config"
)

//go:embed schema.sql
var schemaSQL string

// touchInterval 是影子用户表 last_seen_at 的写入节流间隔，
// 避免每个已认证请求都产生一次写事务。
const touchInterval = 10 * time.Minute

// pingTimeout 是启动时探测数据库的等待上限。
const pingTimeout = 10 * time.Second

// Store 持有数据库连接与少量内存状态。
type Store struct {
	db *sql.DB

	mu      sync.Mutex
	touched map[string]time.Time
}

// Open 建立到 HeatWave 的连接，确保表结构存在后返回。
func Open(cfg config.DB) (*Store, error) {
	dsn, err := cfg.ConnDSN()
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConnsOrDefault())
	db.SetMaxIdleConns(cfg.MaxIdleConnsOrDefault())
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime())

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 HeatWave 失败（检查 db.host / db.name / db.user 与出网白名单）: %w", err)
	}
	if err := initSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db, touched: make(map[string]time.Time)}, nil
}

// initSchema 逐条执行内嵌的 DDL。
//
// MySQL 驱动默认不允许一次发送多条语句，因此这里手工切分；
// 建表语句全部是 CREATE TABLE IF NOT EXISTS，重复执行是安全的。
func initSchema(ctx context.Context, db *sql.DB) error {
	for _, stmt := range splitStatements(schemaSQL) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("初始化表结构失败: %w\n语句: %.120s", err, stmt)
		}
	}
	return nil
}

// splitStatements 去掉注释行后，按行尾分号把脚本切分成独立语句。
//
// 只支持本项目的 DDL 子集：没有存储过程、没有触发器、没有字符串里带分号的语句。
func splitStatements(script string) []string {
	var (
		out []string
		cur strings.Builder
	)
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cur.String()), ";"))
			if stmt != "" {
				out = append(out, stmt)
			}
			cur.Reset()
		}
	}
	if stmt := strings.TrimSpace(cur.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

// DB 暴露底层连接，供后续模块自行查询。
func (s *Store) DB() *sql.DB { return s.db }

// Close 关闭数据库连接池。
func (s *Store) Close() error { return s.db.Close() }
