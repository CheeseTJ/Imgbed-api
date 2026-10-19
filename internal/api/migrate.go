package api

import (
	"context"
	"encoding/json"
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

// jobKindFolderMigrate 是目录改名/移动对应的任务类型。
const jobKindFolderMigrate = "folder_migrate"

// migrateMaxRuntime 限定一次迁移的最长运行时间。
//
// 迁移是逐对象复制，量大了可能要跑很久；不设上限的话，
// 一个卡住的任务会永远占着 goroutine 和那个目录的迁移锁。
const migrateMaxRuntime = 30 * time.Minute

// migratePayload 是目录迁移任务的输入参数。
//
// OwnerID 放在 payload 里而不是靠任务表里的 owner_id 字段：
// worker 是系统级身份、没有用户令牌，把 owner 带进 payload
// 就能让迁移过程中的每一次查询都继续走 owner 限定的 SQL，不会放松越权检查。
type migratePayload struct {
	FolderID    string `json:"folder_id"`
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewParentID string `json:"new_parent_id"`
	OwnerID     string `json:"owner_id"`
}

// renameFolder 处理目录改名与移动。
//
// 目录路径编码在 R2 key 里（SPEC 4.3 的方案 A），所以「改名/移动」
// = 把子树里所有对象搬到新前缀下。对象数量不可预知（可能几千个），
// 因此走异步任务：接口立即返回 job_id，前端轮询进度。
func renameFolder(c *gin.Context, d Deps) {
	var body struct {
		Name     *string `json:"name"`
		ParentID *string `json:"parent_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.BadRequest(c, "请求体格式不正确")
		return
	}
	if body.Name == nil && body.ParentID == nil {
		httpx.BadRequest(c, "没有要修改的内容")
		return
	}

	ownerID := auth.UserID(c)
	ctx := c.Request.Context()

	folder, err := d.Store.GetFolder(ctx, ownerID, c.Param("id"))
	if err != nil {
		httpx.Internal(c, err, "查询目录失败")
		return
	}
	if folder == nil {
		httpx.NotFound(c, "目录不存在")
		return
	}
	if folder.ID == store.RootFolderID {
		httpx.BadRequest(c, "根目录不能改名或移动")
		return
	}
	// 迁移进行中拒绝再次修改：两个 worker 往同一批 key 上写，结果不可预测
	if folder.Migrating {
		httpx.Conflict(c, "该目录正在迁移中，请等它完成")
		return
	}

	newName := folder.Name
	if body.Name != nil {
		newName = strings.TrimSpace(*body.Name)
		switch {
		case newName == "":
			httpx.BadRequest(c, "目录名不能为空")
			return
		case newName == "." || newName == "..":
			httpx.BadRequest(c, "目录名不能是 . 或 ..")
			return
		case strings.ContainsAny(newName, `/\`):
			httpx.BadRequest(c, "目录名不能包含斜杠")
			return
		case len([]rune(newName)) > maxFolderNameRunes:
			httpx.BadRequest(c, fmt.Sprintf("目录名不得超过 %d 个字符", maxFolderNameRunes))
			return
		}
	}

	newParentID := folder.ParentID
	if body.ParentID != nil {
		newParentID = *body.ParentID
	}

	parent, err := d.Store.GetFolder(ctx, ownerID, newParentID)
	if err != nil {
		httpx.Internal(c, err, "查询父目录失败")
		return
	}
	if parent == nil {
		httpx.BadRequest(c, "目标父目录不存在")
		return
	}

	// 防成环：把目录挪进它自己的子树里，parent_id 会形成一个环，
	// 之后所有「沿 parent_id 往上走」的逻辑（面包屑、防成环自身）都会死循环。
	// 从新父目录沿 parent_id 往上走，途中遇到自己即为成环。
	walkID := parent.ID
	for guard := 0; walkID != store.RootFolderID && guard < 64; guard++ {
		if walkID == folder.ID {
			httpx.BadRequest(c, "不能把目录移动到它自己或它的子目录里")
			return
		}
		up, err := d.Store.GetFolder(ctx, ownerID, walkID)
		if err != nil {
			httpx.Internal(c, err, "查询父目录失败")
			return
		}
		if up == nil {
			break
		}
		walkID = up.ParentID
	}

	newPath := path.Join(parent.Path, newName)
	if len([]rune(newPath)) > maxFolderPathRunes {
		httpx.BadRequest(c, "目录层级过深，完整路径超出上限")
		return
	}

	// 没有任何实际变化就不兴师动众地迁移，直接回当前状态
	if newPath == folder.Path && newParentID == folder.ParentID {
		c.JSON(http.StatusOK, gin.H{"folder": toFolderDTO(*folder)})
		return
	}

	subtreeFolders, err := d.Store.ListFoldersUnderPath(ctx, ownerID, folder.Path)
	if err != nil {
		httpx.Internal(c, err, "查询子树目录失败")
		return
	}
	subtreeImages, err := d.Store.ListImagesUnderPath(ctx, ownerID, folder.Path)
	if err != nil {
		httpx.Internal(c, err, "查询子树图片失败")
		return
	}

	now := time.Now().Unix()
	job := store.Job{
		ID:        idgen.New(),
		OwnerID:   ownerID,
		Kind:      jobKindFolderMigrate,
		Total:     len(subtreeImages) + len(subtreeFolders),
		CreatedAt: now,
		UpdatedAt: now,
	}
	payload, err := json.Marshal(migratePayload{
		FolderID:    folder.ID,
		OldPath:     folder.Path,
		NewPath:     newPath,
		NewParentID: newParentID,
		OwnerID:     ownerID,
	})
	if err != nil {
		httpx.Internal(c, err, "序列化任务参数失败")
		return
	}
	job.Payload = string(payload)

	if err := d.Store.CreateJob(ctx, job); err != nil {
		httpx.Internal(c, err, "创建迁移任务失败")
		return
	}
	if err := d.Store.MarkFolderMigrating(ctx, ownerID, folder.ID); err != nil {
		httpx.Internal(c, err, "标记迁移状态失败")
		return
	}

	// 后台执行。**必须用独立上下文**：请求的 ctx 在本函数返回时就被取消，
	// 直接传下去的话任务会在接口返回的一瞬间被掐断。
	bg, cancel := context.WithTimeout(context.Background(), migrateMaxRuntime)
	go func() {
		defer cancel()
		runFolderMigrate(bg, d, job.ID)
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": job.ID,
		"total":  job.Total,
		"folder": toFolderDTO(*folder),
	})
}

// getJob 返回任务状态，前端轮询它画进度条。
func getJob(c *gin.Context, d Deps) {
	job, err := d.Store.GetJob(c.Request.Context(), auth.UserID(c), c.Param("id"))
	if err != nil {
		httpx.Internal(c, err, "查询任务失败")
		return
	}
	if job == nil {
		httpx.NotFound(c, "任务不存在")
		return
	}

	done := job.Status == store.JobDone || job.Status == store.JobFailed
	c.JSON(http.StatusOK, gin.H{
		"id":       job.ID,
		"kind":     job.Kind,
		"status":   job.Status,
		"total":    job.Total,
		"done":     job.Done,
		"finished": done,
		"error":    job.Error,
	})
}

// runFolderMigrate 执行目录迁移。
//
// 步骤与容错设计（SPEC 4.4）：
//
//	逐张：R2 Copy → 更新 DB key → 记进度
//	全部完成后：更新子树目录的 path → 删除旧对象 → 补新占位对象
//
// **可重入**：已迁移过的图（key 已带新前缀）直接跳过，
// 所以任务失败后重试不会重复复制，也不会破坏已经搬好的部分。
func runFolderMigrate(ctx context.Context, d Deps, jobID string) {
	// 抢占：两个 worker 同时拿到同一个任务时，只有一个能把 pending 改成 running
	claimed, err := d.Store.StartJob(ctx, jobID)
	if err != nil {
		log.Printf("目录迁移 %s: 标记开始失败: %v", jobID, err)
		return
	}
	if claimed == 0 {
		return // 已经有人在跑或者已结束
	}

	fail := func(err error) {
		log.Printf("目录迁移 %s 失败: %v", jobID, err)
		_ = d.Store.FinishJob(ctx, jobID, store.JobFailed, err.Error())
	}

	job, err := d.Store.GetJobByID(ctx, jobID)
	if err != nil || job == nil {
		fail(fmt.Errorf("读取任务失败: %v (job=%v)", err, job))
		return
	}

	var p migratePayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		fail(fmt.Errorf("解析任务参数失败: %v", err))
		return
	}

	oldPrefix := path.Join(keyPrefix(p.OwnerID, p.OldPath)...)
	newPrefix := path.Join(keyPrefix(p.OwnerID, p.NewPath)...)
	if oldPrefix == newPrefix {
		// 新旧前缀一致（例如两个目录清洗后同名），迁移无从谈起。
		// 这种情况下目录名已经变了，但 R2 key 不动——它们依然能被访问。
		log.Printf("目录迁移 %s: 新旧前缀相同（%s），仅更新数据库", jobID, oldPrefix)
	}

	images, err := d.Store.ListImagesUnderPath(ctx, p.OwnerID, p.OldPath)
	if err != nil {
		fail(err)
		return
	}

	// ---- 1. 逐张搬对象 ----
	migrated := 0
	var oldKeys []string
	for _, im := range images {
		// 已经是新前缀的说明上一次跑到这里就中断了，跳过即可
		if strings.HasPrefix(im.ObjectKey, newPrefix+"/") {
			migrated++
			_ = d.Store.UpdateJobProgress(ctx, jobID, migrated)
			continue
		}

		newKey := newPrefix + "/" + strings.TrimPrefix(im.ObjectKey, oldPrefix+"/")
		if err := d.R2.Copy(ctx, im.ObjectKey, newKey); err != nil {
			fail(fmt.Errorf("复制 %s → %s 失败: %v", im.ObjectKey, newKey, err))
			return
		}
		if err := d.Store.UpdateImageObjectKey(ctx, p.OwnerID, im.ID, newKey); err != nil {
			fail(err)
			return
		}
		oldKeys = append(oldKeys, im.ObjectKey)
		migrated++
		if err := d.Store.UpdateJobProgress(ctx, jobID, migrated); err != nil {
			log.Printf("目录迁移 %s: 更新进度失败（继续）: %v", jobID, err)
		}
	}

	// ---- 2. 更新子树目录的路径与父目录 ----
	folders, err := d.Store.ListFoldersUnderPath(ctx, p.OwnerID, p.OldPath)
	if err != nil {
		fail(err)
		return
	}
	for _, f := range folders {
		newPath, newParentID := p.NewPath, f.ParentID
		if f.ID != p.FolderID {
			newPath = p.NewPath + "/" + strings.TrimPrefix(f.Path, p.OldPath+"/")
		} else {
			newParentID = p.NewParentID
		}
		if err := d.Store.UpdateFolderPath(ctx, p.OwnerID, f.ID, newPath, newParentID); err != nil {
			fail(err)
			return
		}
	}

	// ---- 3. 删除旧对象。删除是幂等的，失败也只留下可事后清理的副本 ----
	for _, k := range oldKeys {
		if err := d.R2.Delete(ctx, k); err != nil {
			log.Printf("目录迁移 %s: 删除旧对象 %s 失败（可事后清理）: %v", jobID, k, err)
		}
	}
	_ = d.R2.Delete(ctx, folderPlaceholderKey(p.OwnerID, p.OldPath))

	// ---- 4. 给新位置补占位对象，让空目录在控制台里仍然可见 ----
	for _, f := range folders {
		newPath := p.NewPath
		if f.ID != p.FolderID {
			newPath = p.NewPath + "/" + strings.TrimPrefix(f.Path, p.OldPath+"/")
		}
		key := folderPlaceholderKey(p.OwnerID, newPath)
		if err := d.R2.Put(ctx, key, strings.NewReader(""), 0, "application/x-directory"); err != nil {
			log.Printf("目录迁移 %s: 写入占位对象 %s 失败（仅影响控制台可读性）: %v", jobID, key, err)
		}
	}

	if err := d.Store.ClearFolderMigrating(ctx, p.OwnerID, p.FolderID); err != nil {
		log.Printf("目录迁移 %s: 清除迁移标记失败: %v", jobID, err)
	}
	if err := d.Store.FinishJob(ctx, jobID, store.JobDone, ""); err != nil {
		log.Printf("目录迁移 %s: 标记完成失败: %v", jobID, err)
	}
	log.Printf("目录迁移 %s 完成: %s → %s（%d 张图，%d 个目录）",
		jobID, p.OldPath, p.NewPath, len(images), len(folders))
}
