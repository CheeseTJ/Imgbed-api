package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"imgbed/internal/config"
)

// testOwner 是集成测试专用的 owner_id。
//
// 所有写入都带这个 owner，测试结束统一按它清理，绝不碰真实用户的数据。
const testOwner = "__selftest_folders__"

// openTestStore 在提供了数据库环境变量时连真库，否则跳过。
//
// 与 r2 包的 TestRealR2 同样的取舍：单元测试不该依赖外部服务，
// 否则在任何 CI 上都会红；但「SQL 写得对」和「SQL 能在真实 MySQL 上跑」
// 是两回事——唯一键、LIKE 转义、生成列这些只有真库能验证。
//
// 运行方式：
//
//	. ./.env.sh && go test ./internal/store/ -run TestRealFolder -v
func openTestStore(t *testing.T) *Store {
	t.Helper()

	host := os.Getenv("IMGBED_DB_HOST")
	if host == "" {
		t.Skip("未提供 IMGBED_DB_HOST，跳过真实库测试")
	}

	port := 3306
	if p := os.Getenv("IMGBED_DB_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}

	st, err := Open(config.DB{
		Host:     host,
		Port:     port,
		Name:     os.Getenv("IMGBED_DB_NAME"),
		User:     os.Getenv("IMGBED_DB_USER"),
		Password: os.Getenv("IMGBED_DB_PASSWORD"),
		TLSMode:  os.Getenv("IMGBED_DB_TLS_MODE"),
	})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// cleanupOwner 清掉本测试写入的全部痕迹。
//
// 先删图片再删目录：虽然库里没有外键约束，但反过来会让「目录已删而图片还挂着
// folder_id」的中间态出现在清理过程中，一旦清理本身失败就很难看。
func cleanupOwner(t *testing.T, st *Store, owner string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM images WHERE owner_id = ?`, owner); err != nil {
		t.Fatalf("清理图片失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM folders WHERE owner_id = ?`, owner); err != nil {
		t.Fatalf("清理目录失败: %v", err)
	}
}

func mustCreateFolder(t *testing.T, st *Store, f Folder) Folder {
	t.Helper()
	f.OwnerID = testOwner
	f.CreatedAt = time.Now().Unix()
	if err := st.CreateFolder(context.Background(), f); err != nil {
		t.Fatalf("创建目录 %q 失败: %v", f.Name, err)
	}
	return f
}

// TestRealFolderCRUD 覆盖建目录、同级重名、子目录计数与删除。
func TestRealFolderCRUD(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	cleanupOwner(t, st, testOwner)
	t.Cleanup(func() { cleanupOwner(t, st, testOwner) })

	// 根目录是一个虚拟概念，GetFolder 应当返回零值而不是 (nil, nil)
	root, err := st.GetFolder(ctx, testOwner, RootFolderID)
	if err != nil || root == nil {
		t.Fatalf("根目录应可读取: %v / %v", root, err)
	}
	if root.Path != "" {
		t.Errorf("根目录 path 应为空，实际 %q", root.Path)
	}

	parent := mustCreateFolder(t, st, Folder{ID: "T0000000000000000000000A1", ParentID: "", Name: "风景", Path: "风景"})
	child := mustCreateFolder(t, st, Folder{ID: "T0000000000000000000000A2", ParentID: parent.ID, Name: "2026", Path: "风景/2026"})

	// 建在根目录下：父目录传 RootFolderID，path 就不带任何前缀
	if got := mustCreateFolder2(t, st, RootFolderID, "截图", "截图"); got.Path != "截图" {
		t.Errorf("根目录下新目录的 path 应为 %q，实际 %q", "截图", got.Path)
	}

	// 同级同名必须被唯一键挡下
	err = st.CreateFolder(ctx, Folder{
		ID: "T0000000000000000000000A3", OwnerID: testOwner,
		ParentID: parent.ID, Name: "2026", Path: "风景/2026", CreatedAt: time.Now().Unix(),
	})
	if err != ErrDuplicate {
		t.Errorf("同级重名应返回 ErrDuplicate，实际 %v", err)
	}

	// 不同父目录下同名是允许的
	if got := mustCreateFolder2(t, st, child.ID, "2026", "风景/2026/2026"); got.Path != "风景/2026/2026" {
		t.Errorf("深层目录 path 不对：%q", got.Path)
	}

	children, err := st.CountChildFolders(ctx, testOwner, parent.ID)
	if err != nil {
		t.Fatalf("统计子目录失败: %v", err)
	}
	if children != 1 {
		t.Errorf("父目录应有 1 个子目录，实际 %d", children)
	}

	list, err := st.ListFolders(ctx, testOwner)
	if err != nil {
		t.Fatalf("列出目录失败: %v", err)
	}
	if len(list) != 4 {
		t.Errorf("应有 4 个目录，实际 %d", len(list))
	}
	// 按 path 升序返回，前端可以据此顺序建树
	for i := 1; i < len(list); i++ {
		if list[i-1].Path > list[i].Path {
			t.Errorf("目录未按 path 升序：%q 在 %q 之前", list[i-1].Path, list[i].Path)
		}
	}

	if n, err := st.DeleteFolder(ctx, testOwner, child.ID); err != nil || n != 1 {
		t.Fatalf("删除子目录失败: n=%d err=%v", n, err)
	}
	// 删除别的 owner 的目录必须查不到
	if n, err := st.DeleteFolder(ctx, "someone_else", parent.ID); err != nil || n != 0 {
		t.Errorf("越权删除应影响 0 行，实际 n=%d err=%v", n, err)
	}
	if got, _ := st.GetFolder(ctx, testOwner, child.ID); got != nil {
		t.Errorf("目录删除后仍能读到: %+v", got)
	}
}

// folderSeq 给测试目录发号，避免用 name 拼 ID。
//
// 拼字符串再按长度截断是不可行的：len() 数的是字节，中文一个字符占 3 字节，
// 截断点很容易落在一个 UTF-8 字符中间。序号生成没有这个问题。
var folderSeq int

func mustCreateFolder2(t *testing.T, st *Store, parentID, name, wantPath string) Folder {
	t.Helper()
	folderSeq++
	// VARCHAR(26)，用 T + 25 位序号正好占满，且与真实 ULID 不冲突
	id := fmt.Sprintf("T%025d", folderSeq)
	f := mustCreateFolder(t, st, Folder{ID: id, ParentID: parentID, Name: name, Path: wantPath})
	if f.Path != wantPath {
		t.Fatalf("目录 %q 的 path = %q，期望 %q", name, f.Path, wantPath)
	}
	return f
}

// insertTestImage 插一条图片记录，sha256 留空以避开 (owner_id, dedup_key) 唯一键。
func insertTestImage(t *testing.T, st *Store, id, folderID, objectKey string) {
	t.Helper()
	err := st.InsertImage(context.Background(), Image{
		ID: id, OwnerID: testOwner, FolderID: folderID, ObjectKey: objectKey,
		Filename: id + ".jpg", Ext: ".jpg", Mime: "image/jpeg", Size: 1,
		Visibility: "public", CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("插入测试图片 %s 失败: %v", id, err)
	}
}

// TestRealFolderSubtreeImages 验证「按物化路径查子树图片」。
//
// 这是目录树最核心的一条查询：移动图片、统计占用、将来目录改名都要用它。
func TestRealFolderSubtreeImages(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	cleanupOwner(t, st, testOwner)
	t.Cleanup(func() { cleanupOwner(t, st, testOwner) })

	parent := mustCreateFolder(t, st, Folder{ID: "T1000000000000000000000A1", ParentID: "", Name: "风景", Path: "风景"})
	child := mustCreateFolder(t, st, Folder{ID: "T1000000000000000000000A2", ParentID: parent.ID, Name: "2026", Path: "风景/2026"})
	other := mustCreateFolder(t, st, Folder{ID: "T1000000000000000000000A3", ParentID: "", Name: "x_y", Path: "x_y"})

	insertTestImage(t, st, "I0000000000000000000000B1", parent.ID, "u/o/风景/I0000000000000000000000B1.jpg")
	insertTestImage(t, st, "I0000000000000000000000B2", child.ID, "u/o/风景/2026/I0000000000000000000000B2.jpg")
	insertTestImage(t, st, "I0000000000000000000000B3", other.ID, "u/o/x_y/I0000000000000000000000B3.jpg")
	insertTestImage(t, st, "I0000000000000000000000B4", RootFolderID, "u/o/I0000000000000000000000B4.jpg")

	// 根目录只算根目录自己的图片
	rootImages, err := st.ListImagesUnderPath(ctx, testOwner, "")
	if err != nil {
		t.Fatalf("查根目录图片失败: %v", err)
	}
	if len(rootImages) != 1 || rootImages[0].ID != "I0000000000000000000000B4" {
		t.Errorf("根目录应只有 1 张图，实际 %d 张", len(rootImages))
	}

	// 父目录要连子目录一起算进来
	subtree, err := st.ListImagesUnderPath(ctx, testOwner, "风景")
	if err != nil {
		t.Fatalf("查子树图片失败: %v", err)
	}
	if len(subtree) != 2 {
		t.Errorf("「风景」子树应有 2 张图（含子目录），实际 %d", len(subtree))
	}

	// 子目录只算自己的
	onlyChild, err := st.ListImagesUnderPath(ctx, testOwner, "风景/2026")
	if err != nil {
		t.Fatalf("查子目录图片失败: %v", err)
	}
	if len(onlyChild) != 1 || onlyChild[0].ID != "I0000000000000000000000B2" {
		t.Errorf("「风景/2026」应只有 1 张图，实际 %d", len(onlyChild))
	}

	// LIKE 转义：目录名里的下划线是字面量，不能当通配符。
	// 没有转义的话，查 "x_y" 会连带匹配到 "xay" 这类目录。
	escaped, err := st.ListImagesUnderPath(ctx, testOwner, "x_y")
	if err != nil {
		t.Fatalf("查 x_y 失败: %v", err)
	}
	if len(escaped) != 1 {
		t.Errorf("「x_y」应精确匹配 1 张图，实际 %d（LIKE 未转义？）", len(escaped))
	}
}

// TestRealMoveImage 验证移动图片时 folder_id 与 object_key 一起更新。
//
// 两个字段必须同时改：key 里编码了目录路径，只改一个会让库里的位置
// 和 R2 里的位置从此对不上。
func TestRealMoveImage(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	cleanupOwner(t, st, testOwner)
	t.Cleanup(func() { cleanupOwner(t, st, testOwner) })

	dest := mustCreateFolder(t, st, Folder{ID: "T2000000000000000000000A1", ParentID: "", Name: "风景", Path: "风景"})
	insertTestImage(t, st, "I2000000000000000000000B1", RootFolderID, "u/o/I2000000000000000000000B1.jpg")

	n, err := st.MoveImage(ctx, testOwner, "I2000000000000000000000B1", dest.ID, "u/o/风景/I2000000000000000000000B1.jpg")
	if err != nil || n != 1 {
		t.Fatalf("移动失败: n=%d err=%v", n, err)
	}

	im, err := st.GetImage(ctx, testOwner, "I2000000000000000000000B1")
	if err != nil || im == nil {
		t.Fatalf("移动后读不到图片: %v", err)
	}
	if im.FolderID != dest.ID {
		t.Errorf("folder_id 未更新：%q", im.FolderID)
	}
	if im.ObjectKey != "u/o/风景/I2000000000000000000000B1.jpg" {
		t.Errorf("object_key 未更新：%q", im.ObjectKey)
	}

	// 越权移动必须影响 0 行
	if n, err := st.MoveImage(ctx, "someone_else", "I2000000000000000000000B1", RootFolderID, "x"); err != nil || n != 0 {
		t.Errorf("越权移动应影响 0 行，实际 n=%d err=%v", n, err)
	}
}
