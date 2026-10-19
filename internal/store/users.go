package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// User 是本地的影子用户，ID 等于 Ucenter 的 sub。
type User struct {
	ID         string
	Email      string
	Role       string
	CreatedAt  int64
	LastSeenAt int64
}

// Usage 是某个用户的用量统计。
type Usage struct {
	ImageCount  int64
	FolderCount int64
	UsedBytes   int64
}

// EnsureUser 保证用户存在于影子表，并刷新 email / role。
//
// 同一用户在 touchInterval 内只写一次库，因此可以放在每个已认证请求的链路上。
// 使用 MySQL 的 INSERT ... ON DUPLICATE KEY UPDATE 做原子 upsert。
func (s *Store) EnsureUser(ctx context.Context, id, email, role string) error {
	if id == "" {
		return errors.New("用户 ID 不能为空")
	}
	if role == "" {
		role = "user"
	}

	s.mu.Lock()
	if at, ok := s.touched[id]; ok && time.Since(at) < touchInterval {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	now := time.Now().Unix()
	// 别名语法（AS new）需要 MySQL 8.0.19+，HeatWave 满足；
	// 旧的 VALUES(col) 写法在 8.0.20 起已被标记废弃。
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, email, role, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?) AS new
		ON DUPLICATE KEY UPDATE
			email        = new.email,
			role         = new.role,
			last_seen_at = new.last_seen_at`,
		id, email, role, now, now)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.touched[id] = time.Now()
	s.mu.Unlock()
	return nil
}

// GetUser 读取影子用户，不存在时返回 (nil, nil)。
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, email, role, created_at, last_seen_at FROM users WHERE id = ?`, id)

	var u User
	if err := row.Scan(&u.ID, &u.Email, &u.Role, &u.CreatedAt, &u.LastSeenAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &u, nil
}

// GetUsage 统计用户当前（不含回收站）的图片数、目录数与占用字节。
func (s *Store) GetUsage(ctx context.Context, ownerID string) (Usage, error) {
	var u Usage
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size), 0)
		FROM images
		WHERE owner_id = ? AND deleted_at IS NULL`, ownerID).
		Scan(&u.ImageCount, &u.UsedBytes)
	if err != nil {
		return u, err
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM folders WHERE owner_id = ?`, ownerID).
		Scan(&u.FolderCount)
	return u, err
}
