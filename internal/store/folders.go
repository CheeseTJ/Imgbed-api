package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// RootFolderID 是根目录的规范表示。
//
// 用空串而不是 NULL：MySQL 的唯一索引里多个 NULL 互不相同，
// 那样会让「根目录下同名目录」被重复创建，唯一键形同虚设。
const RootFolderID = ""

// Folder 是一条目录记录。
//
// Path 是「物化路径」——从根到本目录，每一级名字用 / 连接（不含首尾斜杠）。
// 这个字段是冗余的，换来三件事：
//
//  1. 按前缀查子树：`path LIKE '风景/%'`，不必递归回溯父节点；
//  2. 前端直接渲染面包屑，不用逐级查库；
//  3. 拼 R2 对象 key 时直接可用。
//
// 代价是改名要连带更新所有子孙的 path（见 M5 的异步迁移）。
type Folder struct {
	ID        string
	OwnerID   string
	ParentID  string // 空串 = 根目录
	Name      string
	Path      string // 空串 = 根目录
	Migrating bool   // 目录迁移进行中，此时拒绝再次改名/移动
	CreatedAt int64
}

// folderColumns 集中列名，避免 Scan 时列顺序与结构体字段对不上。
const folderColumns = `id, owner_id, parent_id, name, path, migrating, created_at`

func scanFolder(row interface{ Scan(...any) error }) (*Folder, error) {
	var f Folder
	err := row.Scan(&f.ID, &f.OwnerID, &f.ParentID, &f.Name, &f.Path, &f.Migrating, &f.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ListFolders 一次取回该用户的全部目录。
//
// 刻意不做「按父目录逐层展开」：个人图床的目录量级在几十到几百，
// 一次取全反而更简单——前端能直接把整棵树画出来，切换目录不必再发请求。
// 真到上千目录再改成分页或懒加载。
func (s *Store) ListFolders(ctx context.Context, ownerID string) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+folderColumns+`
		FROM folders
		WHERE owner_id = ?
		ORDER BY path ASC`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("查询目录列表失败: %w", err)
	}
	defer rows.Close()

	list := make([]Folder, 0, 16)
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// GetFolder 按 ID 读取目录，不存在或不属于该用户时返回 (nil, nil)。
//
// ownerID 参与 WHERE 而不是查完再比对：越权判断必须发生在 SQL 里，
// 否则调用处漏一次比对就是横向越权。
func (s *Store) GetFolder(ctx context.Context, ownerID, id string) (*Folder, error) {
	if id == RootFolderID {
		// 根目录不是一个真实的行，用零值代表它，调用处不必特判
		return &Folder{ID: RootFolderID, OwnerID: ownerID, ParentID: "", Name: "", Path: ""}, nil
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT `+folderColumns+`
		FROM folders
		WHERE id = ? AND owner_id = ?`, id, ownerID)

	f, err := scanFolder(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

// CreateFolder 插入一条目录记录。
//
// 同级重名由 uk_folders_sibling 挡下，返回 ErrDuplicate 让调用处给出
// 「同级已有同名目录」这种可读提示，而不是把 MySQL 1062 抛给用户。
func (s *Store) CreateFolder(ctx context.Context, f Folder) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO folders (id, owner_id, parent_id, name, path, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		f.ID, f.OwnerID, f.ParentID, f.Name, f.Path, f.CreatedAt)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == mysqlDuplicateEntry {
			return ErrDuplicate
		}
		return fmt.Errorf("写入目录记录失败: %w", err)
	}
	return nil
}

// CountChildFolders 统计直接子目录数量，用于「非空目录不可删」的判断。
func (s *Store) CountChildFolders(ctx context.Context, ownerID, parentID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM folders WHERE owner_id = ? AND parent_id = ?`,
		ownerID, parentID).Scan(&n)
	return n, err
}

// DeleteFolder 删除一条目录记录。
//
// 只删这一行，不做级联——调用方必须先确认它既没有子目录也没有图片。
// 让「删目录」保持成一个不可逆但范围明确的动作，比隐式连带删除安全得多。
func (s *Store) DeleteFolder(ctx context.Context, ownerID, id string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM folders WHERE id = ? AND owner_id = ?`, id, ownerID)
	if err != nil {
		return 0, fmt.Errorf("删除目录失败: %w", err)
	}
	return res.RowsAffected()
}

// MoveImage 把图片挪到另一个目录：同时更新 folder_id 与 object_key。
//
// 两个字段必须一起改。object_key 里编码了目录路径（SPEC 4.3 选定的方案 A），
// 只改 folder_id 会让「库里的位置」和「R2 里的位置」从此不一致。
func (s *Store) MoveImage(ctx context.Context, ownerID, id, folderID, objectKey string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE images SET folder_id = ?, object_key = ?
		WHERE id = ? AND owner_id = ? AND deleted_at IS NULL`,
		folderID, objectKey, id, ownerID)
	if err != nil {
		return 0, fmt.Errorf("移动图片失败: %w", err)
	}
	return res.RowsAffected()
}

// RenameImage 更新图片的展示名。
//
// 只动 DB 里的 filename 字段——R2 对象 key 与公开 URL 由 ULID 决定，
// 改名不会破坏任何已经分享出去的链接。与 MoveImage 一样只作用于
// 未删除的图片：回收站里的记录即将被清理或还原，改名没有意义。
func (s *Store) RenameImage(ctx context.Context, ownerID, id, filename string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE images SET filename = ?
		WHERE id = ? AND owner_id = ? AND deleted_at IS NULL`,
		filename, id, ownerID)
	if err != nil {
		return 0, fmt.Errorf("图片改名失败: %w", err)
	}
	return res.RowsAffected()
}

// ListFoldersUnderPath 列出某目录及其所有子目录（含自身）。
//
// 与 ListImagesUnderPath 配套：目录改名/移动时要用它算出子树里每个目录的新路径。
// 返回顺序按 path 升序，父目录一定排在子目录前面。
func (s *Store) ListFoldersUnderPath(ctx context.Context, ownerID, pathPrefix string) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+folderColumns+`
		FROM folders
		WHERE owner_id = ? AND (path = ? OR path LIKE ?)
		ORDER BY path ASC`, ownerID, pathPrefix, escapeLike(pathPrefix)+"/%")
	if err != nil {
		return nil, fmt.Errorf("查询子树目录失败: %w", err)
	}
	defer rows.Close()

	list := make([]Folder, 0, 16)
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// UpdateFolderPath 更新单个目录的物化路径与父目录。
//
// 目录改名/移动时要对子树里的每个目录各调一次：路径是逐级冗余存储的，
// 改一处就得改整条链。用一条 UPDATE 而不是读改写，避免并发下的覆盖。
func (s *Store) UpdateFolderPath(ctx context.Context, ownerID, id, newPath, newParentID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE folders SET path = ?, parent_id = ? WHERE id = ? AND owner_id = ?`,
		newPath, newParentID, id, ownerID)
	if err != nil {
		return fmt.Errorf("更新目录路径失败: %w", err)
	}
	return nil
}

// MarkFolderMigrating 标记目录进入迁移状态，期间拒绝再次改名/移动。
//
// 没有它的话，用户在迁移进行到一半时又发一次改名，
// 两个 worker 会往同一批 key 上互相覆盖，结果不可预测。
func (s *Store) MarkFolderMigrating(ctx context.Context, ownerID, id string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE folders SET migrating = 1 WHERE id = ? AND owner_id = ?`, id, ownerID)
	if err != nil {
		return fmt.Errorf("标记目录迁移状态失败: %w", err)
	}
	return nil
}

// ClearFolderMigrating 清掉迁移标记。迁移结束（无论成败）都要调，否则目录会被永久锁死。
func (s *Store) ClearFolderMigrating(ctx context.Context, ownerID, id string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE folders SET migrating = 0 WHERE id = ? AND owner_id = ?`, id, ownerID)
	if err != nil {
		return fmt.Errorf("清除目录迁移状态失败: %w", err)
	}
	return nil
}

// GetJobByID 不带 owner 条件读任务，**只给后台 worker 用**。
//
// worker 是系统级身份，不持有任何用户的令牌；
// 面向用户的查询一律走带 ownerID 的 GetJob。
func (s *Store) GetJobByID(ctx context.Context, id string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, owner_id, kind, payload, total, done, status, error, created_at, updated_at
		FROM jobs WHERE id = ?`, id)

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

// UpdateImageObjectKey 更新图片的 R2 对象 key。
//
// 目录迁移时逐张调用。只改 key 不改 folder_id——目录的层级关系没变，
// 变的只是目录自己的名字。
func (s *Store) UpdateImageObjectKey(ctx context.Context, ownerID, id, objectKey string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE images SET object_key = ? WHERE id = ? AND owner_id = ?`,
		objectKey, id, ownerID)
	if err != nil {
		return fmt.Errorf("更新图片对象 key 失败: %w", err)
	}
	return nil
}

// ListImagesUnderPath 列出某目录及其所有子目录下的未删除图片。
//
// 用 path LIKE '前缀/%' 而不是递归查 folder_id：物化路径让子树查询退化成
// 一次范围扫描，这正是当初冗余存 path 的原因。
// 前缀为空时返回根目录下的全部图片。
func (s *Store) ListImagesUnderPath(ctx context.Context, ownerID, pathPrefix string) ([]Image, error) {
	query := `
		SELECT ` + imageColumns + `
		FROM images
		WHERE owner_id = ? AND deleted_at IS NULL`
	args := []any{ownerID}

	if pathPrefix != "" {
		// 先按 id 集合过滤出子树里的目录，再用它筛图片。
		// 不直接 LIKE object_key：key 里的段是清洗过的（空格变下划线等），
		// 拿原始目录名去比会因为清洗差异而漏掉。
		query += ` AND folder_id IN (
			SELECT id FROM folders
			WHERE owner_id = ? AND (path = ? OR path LIKE ?)
		)`
		args = append(args, ownerID, pathPrefix, escapeLike(pathPrefix)+"/%")
	} else {
		query += ` AND folder_id = ''`
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询子树图片失败: %w", err)
	}
	defer rows.Close()

	list := make([]Image, 0, 16)
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

// escapeLike 转义 LIKE 模式里的通配符，避免目录名里的 % 和 _ 被当成通配。
//
// 目录名允许含 _（清洗规则里保留了下划线），不转义的话「a_b」会匹配到「axb」。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
