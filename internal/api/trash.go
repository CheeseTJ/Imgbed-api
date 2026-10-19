package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"imgbed/internal/auth"
	"imgbed/internal/httpx"
	"imgbed/internal/store"
)

// trashDTO 是回收站对外的表示。
//
// 与 imageDTO 的差别是多了 deleted_at / expires_at，
// 前端拿后者显示「x 天后自动删除」，不必自己按保留期换算。
type trashDTO struct {
	ID        string `json:"id"`
	FolderID  string `json:"folder_id"`
	Filename  string `json:"filename"`
	Ext       string `json:"ext"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	URL       string `json:"url"`
	DeletedAt int64  `json:"deleted_at"`
	ExpiresAt int64  `json:"expires_at"`
	CreatedAt int64  `json:"created_at"`
}

func toTrashDTO(t store.TrashItem, d Deps) trashDTO {
	url := ""
	if d.R2 != nil {
		// 软删除不动 R2 对象，所以回收站里的图仍然可以预览；
		// URL 依旧不可猜测，且只有 owner 能在界面上看到它。
		url = d.R2.PublicURL(t.ObjectKey)
	}
	return trashDTO{
		ID:        t.ID,
		FolderID:  t.FolderID,
		Filename:  t.Filename,
		Ext:       t.Ext,
		Mime:      t.Mime,
		Size:      t.Size,
		Width:     t.Width,
		Height:    t.Height,
		URL:       url,
		DeletedAt: t.DeletedAt,
		ExpiresAt: t.DeletedAt + int64(d.Cfg.Limits.TrashRetention().Seconds()),
		CreatedAt: t.CreatedAt,
	}
}

// listTrash 分页列出回收站，最先到期的排最前。
func listTrash(c *gin.Context, d Deps) {
	ownerID := auth.UserID(c)

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(c.DefaultQuery("size", "50"))
	if size < 1 || size > 200 {
		size = 50
	}

	ctx := c.Request.Context()
	total, err := d.Store.CountTrash(ctx, ownerID)
	if err != nil {
		httpx.Internal(c, err, "统计回收站失败")
		return
	}

	items, err := d.Store.ListTrash(ctx, ownerID, size, (page-1)*size)
	if err != nil {
		httpx.Internal(c, err, "查询回收站失败")
		return
	}

	dtos := make([]trashDTO, 0, len(items))
	for _, t := range items {
		dtos = append(dtos, toTrashDTO(t, d))
	}

	c.JSON(http.StatusOK, gin.H{"items": dtos, "total": total, "page": page, "size": size})
}

// restoreImage 把图片从回收站恢复回原目录。
//
// 只清 deleted_at：R2 对象一直都在，所以恢复是零成本的，
// 也不需要关心目录是否还存在（folder_id 原样保留）。
func restoreImage(c *gin.Context, d Deps) {
	affected, err := d.Store.RestoreImage(c.Request.Context(), auth.UserID(c), c.Param("id"))
	if err != nil {
		httpx.Internal(c, err, "恢复图片失败")
		return
	}
	if affected == 0 {
		httpx.NotFound(c, "图片不在回收站")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// purgeImage 彻底删除一张回收站图片（R2 对象 + DB 行）。
//
// **顺序是先删对象再删行**，反过来一旦删对象失败，
// 库里就没了记录可查，那个对象会变成无人知晓的孤儿——既占存储又永远清不掉。
func purgeImage(c *gin.Context, d Deps) {
	if !d.R2Ready() {
		httpx.Abort(c, http.StatusServiceUnavailable, "r2_unavailable", "对象存储未配置")
		return
	}

	ownerID := auth.UserID(c)
	ctx := c.Request.Context()

	im, err := d.Store.GetTrashImage(ctx, ownerID, c.Param("id"))
	if err != nil {
		httpx.Internal(c, err, "查询回收站失败")
		return
	}
	if im == nil {
		httpx.NotFound(c, "图片不在回收站")
		return
	}

	if err := d.R2.Delete(ctx, im.ObjectKey); err != nil {
		httpx.Internal(c, err, "删除对象失败")
		return
	}
	affected, err := d.Store.PurgeImage(ctx, ownerID, im.ID)
	if err != nil {
		httpx.Internal(c, err, "删除记录失败")
		return
	}
	if affected == 0 {
		httpx.NotFound(c, "图片不在回收站")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// emptyTrash 清空回收站。
//
// 用户点「清空」就是明确要把眼前看到的全部删掉，所以这里**无视保留期**；
// 只清「已过 7 天」的那个活由后台清理循环负责，两者分工不同。
//
// 逐条删而不是一条 DELETE：每张图的 R2 对象都得删，顺序又必须是
// 先对象后行（见 purgeImage），单条 SQL 做不到。
func emptyTrash(c *gin.Context, d Deps) {
	if !d.R2Ready() {
		httpx.Abort(c, http.StatusServiceUnavailable, "r2_unavailable", "对象存储未配置")
		return
	}

	ownerID := auth.UserID(c)
	ctx := c.Request.Context()

	const pageSize = 200
	purged := 0
	for {
		items, err := d.Store.ListTrash(ctx, ownerID, pageSize, 0)
		if err != nil {
			httpx.Internal(c, err, "查询回收站失败")
			return
		}
		if len(items) == 0 {
			break
		}

		for _, t := range items {
			if err := d.R2.Delete(ctx, t.ObjectKey); err != nil {
				httpx.Internal(c, err, "删除对象失败")
				return
			}
			if _, err := d.Store.PurgeImage(ctx, ownerID, t.ID); err != nil {
				httpx.Internal(c, err, "删除记录失败")
				return
			}
			purged++
		}
		// 每轮都从 offset 0 重新取：删掉一批之后剩下的会顶上来
		if len(items) < pageSize {
			break
		}
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "purged": purged})
}
