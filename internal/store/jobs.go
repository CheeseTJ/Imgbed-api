package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 任务的四种状态。
const (
	JobPending = "pending"
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// Job 是一条异步任务。目前只有 kind = folder_migrate（目录改名/移动）。
type Job struct {
	ID        string
	OwnerID   string
	Kind      string
	Payload   string // JSON，任务的输入参数
	Total     int
	Done      int
	Status    string
	Error     string
	CreatedAt int64
	UpdatedAt int64
}

// CreateJob 插入一条任务记录，状态从 pending 开始。
func (s *Store) CreateJob(ctx context.Context, j Job) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (id, owner_id, kind, payload, total, done, status, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		j.ID, j.OwnerID, j.Kind, j.Payload, j.Total, j.Done, JobPending, j.CreatedAt, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("写入任务失败: %w", err)
	}
	return nil
}

// GetJob 读取任务。ownerID 参与 WHERE，越权直接查不到。
func (s *Store) GetJob(ctx context.Context, ownerID, id string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, owner_id, kind, payload, total, done, status, error, created_at, updated_at
		FROM jobs
		WHERE id = ? AND owner_id = ?`, id, ownerID)

	var j Job
	err := row.Scan(&j.ID, &j.OwnerID, &j.Kind, &j.Payload, &j.Total, &j.Done,
		&j.Status, &j.Error, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &j, nil
}

// StartJob 把任务标记为 running。
//
// 返回受影响行数：并发下两个 worker 同时抢到同一个任务时，只有一个能改成功，
// 另一个应当放弃——用「条件更新」做互斥，省掉一把进程内锁。
func (s *Store) StartJob(ctx context.Context, id string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`, JobRunning, time.Now().Unix(), id, JobPending)
	if err != nil {
		return 0, fmt.Errorf("标记任务开始失败: %w", err)
	}
	return res.RowsAffected()
}

// UpdateJobProgress 更新已完成数量。
//
// 只在数值真的前进时才写库：前端每秒轮询一次，如果每次都写，
// 一个 5000 个对象的迁移就会产生 5000 次无意义的 UPDATE。
func (s *Store) UpdateJobProgress(ctx context.Context, id string, done int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET done = ?, updated_at = ?
		WHERE id = ? AND done < ?`, done, time.Now().Unix(), id, done)
	if err != nil {
		return fmt.Errorf("更新任务进度失败: %w", err)
	}
	return nil
}

// FinishJob 结束任务。status 只能是 done 或 failed。
func (s *Store) FinishJob(ctx context.Context, id, status, errMsg string) error {
	if status != JobDone && status != JobFailed {
		return errors.New("非法的任务结束状态: " + status)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, errMsg, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("结束任务失败: %w", err)
	}
	return nil
}
