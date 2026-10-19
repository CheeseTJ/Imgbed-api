package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"imgbed/internal/auth"
	"imgbed/internal/httpx"
)

// batchImages 批量处理图片：移动 / 删除（软删）/ 恢复。
//
// 语义是**尽力而为 + 逐张回报**，不是全有全无：
// 一批 50 张里第 3 张失败时，把失败原因告诉用户比整批回滚有用得多——
// 用户关心的就是「哪几张没成、为什么」。
//
// 彻底删除（purge）刻意不提供批量：它不可逆，逐张确认
// 比一次全灭更符合破坏性操作的分级。
func batchImages(c *gin.Context, d Deps) {
	var body struct {
		Action   string   `json:"action"` // move | delete | restore
		IDs      []string `json:"ids"`
		FolderID string   `json:"folder_id"` // 仅 move 需要
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.BadRequest(c, "请求体格式不正确")
		return
	}

	max := d.Cfg.Limits.MaxBatch
	if max <= 0 {
		max = 50
	}
	switch {
	case len(body.IDs) == 0:
		httpx.BadRequest(c, "没有要处理的图片")
		return
	case len(body.IDs) > max:
		httpx.Abort(c, http.StatusRequestEntityTooLarge, "batch_too_large",
			fmt.Sprintf("一次最多处理 %d 张图片", max))
		return
	case body.Action == "move" && body.FolderID == "":
		httpx.BadRequest(c, "批量移动需要指定目标目录")
		return
	}

	ownerID := auth.UserID(c)
	ctx := c.Request.Context()

	type item struct {
		ID    string `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	results := make([]item, 0, len(body.IDs))

	// 去重：同一张图出现两次会各算一次，而第二次多半因为状态已变而失败
	seen := make(map[string]struct{}, len(body.IDs))
	succeeded := 0
	for _, id := range body.IDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}

		entry := item{ID: id, OK: true}
		var err error

		switch body.Action {
		case "move":
			_, err = doMove(ctx, d, ownerID, id, body.FolderID)
		case "delete":
			_, err = d.Store.SoftDeleteImage(ctx, ownerID, id, time.Now().Unix())
		case "restore":
			_, err = d.Store.RestoreImage(ctx, ownerID, id)
		default:
			httpx.BadRequest(c, "未知的批量操作: "+body.Action)
			return
		}

		if err != nil {
			entry.OK = false
			entry.Error = err.Error()
		} else {
			succeeded++
		}
		results = append(results, entry)
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":        succeeded == len(results),
		"succeeded": succeeded,
		"failed":    len(results) - succeeded,
		"results":   results,
	})
}
