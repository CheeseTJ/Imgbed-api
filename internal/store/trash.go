package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// TrashItem 是回收站里的一条记录：一张被软删除的图片，外加到期时间。
//
// ExpiresAt 由调用方按 trash_retain_days 换算后填进来，
// 前端拿去显示「x 天后自动删除」，不必自己算。
type TrashItem struct {
	Image
	DeletedAt int64
	ExpiresAt int64
}

// trashColumns 在图片列的基础上补回收站需要的时间字段。
const trashColumns = `id, owner_id, folder_id, object_key, filename, ext, mime,
	size, width, height, COALESCE(sha256, ''), visibility, views, deleted_at, created_at`

func scanTrash(row interface{ Scan(...any) error }) (*TrashItem, error) {
	var t TrashItem
	err := row.Scan(
		&t.Image.ID, &t.Image.OwnerID, &t.Image.FolderID, &t.Image.ObjectKey, &t.Image.Filename,
		&t.Image.Ext, &t.Image.Mime, &t.Image.Size, &t.Image.Width, &t.Image.Height,
		&t.Image.SHA256, &t.Image.Visibility, &t.Image.Views, &t.DeletedAt, &t.Image.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTrash 列出回收站里的图片，**最先到期的排最前**——那才是用户最该处理的。
func (s *Store) ListTrash(ctx context.Context, ownerID string, limit, offset int) ([]TrashItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+trashColumns+`
		FROM images
		WHERE owner_id = ? AND deleted_at IS NOT NULL
		ORDER BY deleted_at ASC, id ASC
		LIMIT ? OFFSET ?`, ownerID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("查询回收站失败: %w", err)
	}
	defer rows.Close()

	list := make([]TrashItem, 0, limit)
	for rows.Next() {
		t, err := scanTrash(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// CountTrash 统计回收站里的图片数量，用于分页与侧栏角标。
func (s *Store) CountTrash(ctx context.Context, ownerID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM images
		WHERE owner_id = ? AND deleted_at IS NOT NULL`, ownerID).Scan(&n)
	return n, err
}

// GetTrashImage 读取一条回收站记录（含 deleted_at）。
//
// 与 GetImage 的区别：GetImage 会把已删除的图排除在外，这里恰恰只要它们。
// ownerID 参与 WHERE，越权直接查不到。
func (s *Store) GetTrashImage(ctx context.Context, ownerID, id string) (*Image, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+imageColumns+`
		FROM images
		WHERE id = ? AND owner_id = ? AND deleted_at IS NOT NULL`, id, ownerID)

	im, err := scanImage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return im, nil
}

// RestoreImage 把图片从回收站恢复。
//
// 只清 deleted_at，R2 对象从未被删过，所以恢复是零成本的——
// 这正是「软删除 + 到期才物理删除」这个设计的目的：给用户反悔的机会。
func (s *Store) RestoreImage(ctx context.Context, ownerID, id string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE images SET deleted_at = NULL
		WHERE id = ? AND owner_id = ? AND deleted_at IS NOT NULL`, id, ownerID)
	if err != nil {
		return 0, fmt.Errorf("恢复图片失败: %w", err)
	}
	return res.RowsAffected()
}

// PurgeImage 彻底删除一条回收站记录（只删 DB 行）。
//
// **R2 对象必须由调用方先删**。顺序不能反：先删行再删对象的话，
// 一旦删对象失败，库里就没有记录可查了，那个对象会变成无人知晓的孤儿，
// 既占存储又永远清不掉。
func (s *Store) PurgeImage(ctx context.Context, ownerID, id string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM images
		WHERE id = ? AND owner_id = ? AND deleted_at IS NOT NULL`, id, ownerID)
	if err != nil {
		return 0, fmt.Errorf("彻底删除图片失败: %w", err)
	}
	return res.RowsAffected()
}

// ListExpiredTrash 列出**所有用户**已过保留期的回收站记录，供后台清理任务使用。
//
// 不带 ownerID：清理是系统级的，不针对某个用户。
// limit 控制单轮批量，避免一次性把几千条读进内存。
func (s *Store) ListExpiredTrash(ctx context.Context, now int64, limit int) ([]Image, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+imageColumns+`
		FROM images
		WHERE deleted_at IS NOT NULL AND deleted_at <= ?
		ORDER BY deleted_at ASC
		LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("查询过期回收站失败: %w", err)
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
