package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"imgbed/internal/auth"
	"imgbed/internal/httpx"
	"imgbed/internal/idgen"
	"imgbed/internal/store"
)

// maxFolderNameRunes 限制单级目录名的长度。
//
// 按「字符数」而不是字节数判断：中文目录名一个字符占 3 字节，
// 按字节限制会让中文用户莫名其妙地更早撞墙。
const maxFolderNameRunes = 100

// maxFolderPathRunes 与 schema 里 path VARCHAR(700) 对齐。
//
// MySQL 的 VARCHAR 按字符计数，所以这里也用字符数，
// 让「能存进去」与「能建出来」两个边界一致。
const maxFolderPathRunes = 700

// folderDTO 是目录对外的表示。
//
// 不暴露 migrating：那是 M5 异步迁移的内部状态，现阶段前端用不到。
type folderDTO struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	// Depth 让前端直接按层级缩进，不必自己去数 path 里有几个斜杠
	Depth     int   `json:"depth"`
	CreatedAt int64 `json:"created_at"`
}

func toFolderDTO(f store.Folder) folderDTO {
	depth := 0
	if f.Path != "" {
		depth = strings.Count(f.Path, "/") + 1
	}
	return folderDTO{
		ID:        f.ID,
		ParentID:  f.ParentID,
		Name:      f.Name,
		Path:      f.Path,
		Depth:     depth,
		CreatedAt: f.CreatedAt,
	}
}

// folderPlaceholderKey 返回目录占位对象的 key（以 / 结尾的 0 字节对象）。
//
// R2 没有真实目录，控制台里那个「新建文件夹」写的就是这种东西。
// 它纯粹是为了让空目录在 R2 控制台里也看得见——目录树的权威数据源始终是 DB，
// 占位对象丢了不影响任何功能，所以下面所有涉及它的失败都只记日志。
func folderPlaceholderKey(ownerID, folderPath string) string {
	return path.Join(keyPrefix(ownerID, folderPath)...) + "/"
}

// listFolders 一次返回全部目录。
//
// 不分页也不懒加载：目录树要整体渲染，分批返回只会让前端多维护一份
// 「哪些分支还没加载」的状态，而个人量级下（几十到几百个目录）没有这个必要。
func listFolders(c *gin.Context, d Deps) {
	list, err := d.Store.ListFolders(c.Request.Context(), auth.UserID(c))
	if err != nil {
		httpx.Internal(c, err, "查询目录列表失败")
		return
	}

	dtos := make([]folderDTO, 0, len(list))
	for _, f := range list {
		dtos = append(dtos, toFolderDTO(f))
	}
	c.JSON(http.StatusOK, gin.H{"items": dtos})
}

// createFolder 新建目录。
//
// 目录树的权威数据源是 DB，R2 里只额外补一个占位对象让空目录在控制台可见。
// 因此建目录不依赖对象存储——R2 没配也能建（只是控制台里看不到空目录）。
func createFolder(c *gin.Context, d Deps) {
	var body struct {
		ParentID string `json:"parent_id"`
		Name     string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.BadRequest(c, "请求体格式不正确")
		return
	}

	name := strings.TrimSpace(body.Name)
	switch {
	case name == "":
		httpx.BadRequest(c, "目录名不能为空")
		return
	case name == "." || name == "..":
		httpx.BadRequest(c, "目录名不能是 . 或 ..")
		return
	case strings.ContainsAny(name, `/\`):
		// 斜杠是路径分隔符，混进名字里会让物化路径与实际层级对不上
		httpx.BadRequest(c, "目录名不能包含斜杠")
		return
	case len([]rune(name)) > maxFolderNameRunes:
		httpx.BadRequest(c, fmt.Sprintf("目录名不得超过 %d 个字符", maxFolderNameRunes))
		return
	}

	ownerID := auth.UserID(c)
	ctx := c.Request.Context()

	parent, err := d.Store.GetFolder(ctx, ownerID, body.ParentID)
	if err != nil {
		httpx.Internal(c, err, "查询父目录失败")
		return
	}
	if parent == nil {
		httpx.BadRequest(c, "父目录不存在")
		return
	}

	// parent.Path 为空（父是根）时，path.Join 正好返回 name，
	// 不必为根目录单写一个分支。
	fullPath := path.Join(parent.Path, name)
	if len([]rune(fullPath)) > maxFolderPathRunes {
		httpx.BadRequest(c, "目录层级过深，完整路径超出上限")
		return
	}

	f := store.Folder{
		ID:        idgen.New(),
		OwnerID:   ownerID,
		ParentID:  parent.ID,
		Name:      name,
		Path:      fullPath,
		CreatedAt: time.Now().Unix(),
	}

	if err := d.Store.CreateFolder(ctx, f); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			httpx.Abort(c, http.StatusConflict, "folder_exists", "同级已存在同名目录")
			return
		}
		httpx.Internal(c, err, "创建目录失败")
		return
	}

	if d.R2Ready() {
		key := folderPlaceholderKey(ownerID, f.Path)
		if err := d.R2.Put(ctx, key, strings.NewReader(""), 0, "application/x-directory"); err != nil {
			log.Printf("警告: 写入目录占位对象 %s 失败（仅影响控制台可读性）: %v", key, err)
		}
	}

	c.JSON(http.StatusCreated, gin.H{"folder": toFolderDTO(f)})
}

// deleteFolder 删除目录。
//
// **只允许删空目录**（没有子目录、也没有未删除的图片）。
//
// 刻意不做级联：级联删除图片在语义上属于回收站那一块（M6），
// 而现在连回收站界面都还没有。隐式连带删除会让图片「凭空消失」，
// 用户既看不到也恢复不了——宁可他先把目录清空，也不要制造这种惊吓。
func deleteFolder(c *gin.Context, d Deps) {
	ownerID := auth.UserID(c)
	ctx := c.Request.Context()

	target, err := d.Store.GetFolder(ctx, ownerID, c.Param("id"))
	if err != nil {
		httpx.Internal(c, err, "查询目录失败")
		return
	}
	if target == nil {
		httpx.NotFound(c, "目录不存在")
		return
	}
	if target.ID == store.RootFolderID {
		httpx.BadRequest(c, "根目录不能删除")
		return
	}

	children, err := d.Store.CountChildFolders(ctx, ownerID, target.ID)
	if err != nil {
		httpx.Internal(c, err, "统计子目录失败")
		return
	}
	if children > 0 {
		httpx.Abort(c, http.StatusConflict, "folder_not_empty",
			fmt.Sprintf("目录下还有 %d 个子目录，请先清空", children))
		return
	}

	images, err := d.Store.CountImages(ctx, ownerID, target.ID)
	if err != nil {
		httpx.Internal(c, err, "统计图片失败")
		return
	}
	if images > 0 {
		httpx.Abort(c, http.StatusConflict, "folder_not_empty",
			fmt.Sprintf("目录下还有 %d 张图片，请先移走或删除", images))
		return
	}

	affected, err := d.Store.DeleteFolder(ctx, ownerID, target.ID)
	if err != nil {
		httpx.Internal(c, err, "删除目录失败")
		return
	}
	if affected == 0 {
		httpx.NotFound(c, "目录不存在")
		return
	}

	// 占位对象一并清掉，否则 R2 控制台里会留下一个点进去什么都没有的空壳目录
	if d.R2Ready() {
		if err := d.R2.Delete(ctx, folderPlaceholderKey(ownerID, target.Path)); err != nil {
			log.Printf("警告: 删除目录占位对象失败（仅影响控制台可读性）: %v", err)
		}
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}
