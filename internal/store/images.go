package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
)

// mysqlDuplicateEntry 是 MySQL 唯一键冲突的错误码。
const mysqlDuplicateEntry = 1062

// ErrDuplicate 表示同一用户下已存在内容相同的图片（按 sha256 判定）。
var ErrDuplicate = errors.New("内容重复")

// Image 是一条图片记录。
//
// ObjectKey 是 R2 中的真实 key；Filename 只是展示名——
// 改文件名不需要动 R2，这也正是把 ULID 放进 key 而不是原始文件名的原因。
type Image struct {
	ID         string
	OwnerID    string
	FolderID   string
	ObjectKey  string
	Filename   string
	Ext        string
	Mime       string
	Size       int64
	Width      int
	Height     int
	SHA256     string
	Visibility string
	Views      int64
	CreatedAt  int64
}

// imageColumns 集中列名，避免 Scan 时列顺序与结构体字段对不上。
const imageColumns = `id, owner_id, folder_id, object_key, filename, ext, mime,
	size, width, height, COALESCE(sha256, ''), visibility, views, created_at`

func scanImage(row interface{ Scan(...any) error }) (*Image, error) {
	var im Image
	err := row.Scan(
		&im.ID, &im.OwnerID, &im.FolderID, &im.ObjectKey, &im.Filename, &im.Ext, &im.Mime,
		&im.Size, &im.Width, &im.Height, &im.SHA256, &im.Visibility, &im.Views, &im.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &im, nil
}

// InsertImage 写入一条图片记录。
//
// 撞上 (owner_id, dedup_key) 唯一键时返回 ErrDuplicate 而不是原始 SQL 错误——
// 调用方据此走「秒传」分支，不必自己解析 MySQL 错误码。
func (s *Store) InsertImage(ctx context.Context, im Image) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO images
			(id, owner_id, folder_id, object_key, filename, ext, mime,
			 size, width, height, sha256, visibility, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		im.ID, im.OwnerID, im.FolderID, im.ObjectKey, im.Filename, im.Ext, im.Mime,
		im.Size, im.Width, im.Height, nullableString(im.SHA256), im.Visibility, im.CreatedAt)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == mysqlDuplicateEntry {
			return ErrDuplicate
		}
		return fmt.Errorf("写入图片记录失败: %w", err)
	}
	return nil
}

// FindImageByHash 查找该用户下未删除、且内容相同的图片。
// 命中即说明这个文件此前传过，可以直接复用它的 URL（秒传）。
func (s *Store) FindImageByHash(ctx context.Context, ownerID, sha256 string) (*Image, error) {
	if sha256 == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT `+imageColumns+`
		FROM images
		WHERE owner_id = ? AND sha256 = ? AND deleted_at IS NULL
		LIMIT 1`, ownerID, sha256)

	im, err := scanImage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return im, nil
}

// ListImages 按时间倒序列出某个目录下的图片（不含回收站）。
func (s *Store) ListImages(ctx context.Context, ownerID, folderID string, limit, offset int) ([]Image, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+imageColumns+`
		FROM images
		WHERE owner_id = ? AND folder_id = ? AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`, ownerID, folderID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("查询图片列表失败: %w", err)
	}
	defer rows.Close()

	list := make([]Image, 0, limit)
	for rows.Next() {
		im, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *im)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// CountImages 统计某个目录下的图片数量，用于分页。
func (s *Store) CountImages(ctx context.Context, ownerID, folderID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM images
		WHERE owner_id = ? AND folder_id = ? AND deleted_at IS NULL`,
		ownerID, folderID).Scan(&n)
	return n, err
}

// GetImage 按 ID 读取图片，不存在或不属于该用户时返回 (nil, nil)。
//
// ownerID 参与查询条件而不是查完再比对：权限判断必须发生在 SQL 里，
// 否则一旦有人在调用处忘了比对，就是越权读别人的图。
func (s *Store) GetImage(ctx context.Context, ownerID, id string) (*Image, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+imageColumns+`
		FROM images
		WHERE id = ? AND owner_id = ?`, id, ownerID)

	im, err := scanImage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return im, nil
}

// SoftDeleteImage 把图片移入回收站。R2 对象保留，到保留期后由清理任务物理删除。
func (s *Store) SoftDeleteImage(ctx context.Context, ownerID, id string, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE images SET deleted_at = ?
		WHERE id = ? AND owner_id = ? AND deleted_at IS NULL`, now, id, ownerID)
	if err != nil {
		return 0, fmt.Errorf("删除图片失败: %w", err)
	}
	return res.RowsAffected()
}

// IncrImageViews 累加访问计数。
//
// 刻意用 UPDATE ... SET views = views + 1 而不是「读出来加一再写回」：
// 后者在并发下会丢计数。这里也容忍失败——访问统计不值得让请求出错。
func (s *Store) IncrImageViews(ctx context.Context, id string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE images SET views = views + 1 WHERE id = ?`, id)
}

// nullableString 把空串转成 SQL NULL。
//
// sha256 列上的唯一索引依赖于此：MySQL 唯一索引把多个 NULL 视为互不相同，
// 而未参与去重的记录（sha256 为空）正需要这个语义。
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
