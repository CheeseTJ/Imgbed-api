package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"imgbed/internal/auth"
	"imgbed/internal/httpx"
	"imgbed/internal/idgen"
	"imgbed/internal/imageinfo"
	"imgbed/internal/store"
)

// uploadOverhead 是 multipart 信封的开销余量。
// MaxBytesReader 的限额要比单文件上限略大，否则合法的 20MB 文件会因为
// 多出来的 boundary 与头部字段而被误判为超限。
const uploadOverhead = 1 << 20

// imageDTO 是图片对外的表示。
//
// 为什么不直接把 store.Image 返回：object_key 是内部实现细节，
// 而且前端真正需要的是拼好的 url（换域名时只改后端配置，前端不用动）。
type imageDTO struct {
	ID         string `json:"id"`
	FolderID   string `json:"folder_id"`
	Filename   string `json:"filename"`
	Ext        string `json:"ext"`
	Mime       string `json:"mime"`
	Size       int64  `json:"size"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Visibility string `json:"visibility"`
	URL        string `json:"url"`
	CreatedAt  int64  `json:"created_at"`
}

func toDTO(im store.Image, d Deps) imageDTO {
	return imageDTO{
		ID:         im.ID,
		FolderID:   im.FolderID,
		Filename:   im.Filename,
		Ext:        im.Ext,
		Mime:       im.Mime,
		Size:       im.Size,
		Width:      im.Width,
		Height:     im.Height,
		Visibility: im.Visibility,
		URL:        d.R2.PublicURL(im.ObjectKey),
		CreatedAt:  im.CreatedAt,
	}
}

// sanitizeSegment 清洗路径中用作 key 的一段。
//
// **保留 Unicode 字母与数字**，只把真正有问题的字符换成下划线。
//
// 早先的实现是「非 [a-zA-Z0-9_.-] 一律替换」，那会让所有中文目录名
// 塌缩成同一串下划线——「风景」和「截图」都变成 `__`，
// 于是 SPEC 4.3 选方案 A 的唯一理由（R2 控制台里能看懂目录结构）当场作废。
// 而 SPEC 自己给的示例 key 就是 `u/uc_8f2a/风景/2026/...`，本就期望中文可见。
//
// 安全性质不变：斜杠、反斜杠、控制字符仍然被替换，
// 连点也会被折叠，所以「生成的 key 里不存在 ..」依旧是一眼可验的不变式。
func sanitizeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			// 空格、斜杠、反斜杠、控制字符等一律折叠成下划线
			b.WriteRune('_')
		}
	}

	out := b.String()
	// 折叠连续的点：`..` 是路径穿越的唯一素材。消灭它之后，
	// 「生成的 key 里不存在 ..」就成了一个可以一眼验证的不变式，
	// 不必再去推敲它在什么上下文里是否真的危险。
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	out = strings.Trim(out, ".")
	if out == "" {
		out = "_"
	}
	return out
}

// keyPrefix 拼出「用户 + 目录」这一段公共前缀。
//
// 图片 key 和目录占位对象都从这里长出来，保证「图片落在它所属目录的前缀下」
// 这条不变式只有一个实现处，不会两边各写一份再慢慢漂移。
func keyPrefix(ownerID, folderPath string) []string {
	parts := []string{"u", sanitizeSegment(ownerID)}
	for _, seg := range strings.Split(folderPath, "/") {
		// 清洗后仍是 "_" 的段落直接丢弃：那是「空名」或纯符号名，
		// 留着只会让 key 里多出一串没有信息量的下划线。
		if seg = sanitizeSegment(seg); seg != "_" {
			parts = append(parts, seg)
		}
	}
	return parts
}

// objectKey 生成 R2 对象 key：u/{owner}/{folder/{folder}}/{ulid}{ext}
//
// 用 ULID 而不是原始文件名：原文件名可能含中文、空格、特殊符号，
// 直接进 key 会带来永久的 URL 编码麻烦；展示名单独存库，改它不必动 R2。
func objectKey(ownerID, folderPath, id, ext string) string {
	return path.Join(append(keyPrefix(ownerID, folderPath), id+ext)...)
}

// uploadImage 处理单张图片上传。
//
// 流程：读入并算 sha256 → 去重 → 上传 R2 → 写元数据。
// 顺序不能反：先写库再传对象的话，一旦上传失败库里就会留下指向不存在对象的记录。
func uploadImage(c *gin.Context, d Deps) {
	if !d.R2Ready() {
		httpx.Abort(c, http.StatusServiceUnavailable, "r2_unavailable",
			"对象存储未配置，暂时无法上传")
		return
	}

	maxBytes := d.Cfg.Limits.MaxUploadBytes()
	// 提前用 MaxBytesReader 兜住请求体，避免超大请求被完整读进内存
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes+uploadOverhead)

	fileHeader, err := c.FormFile("file")
	if err != nil {
		httpx.BadRequest(c, "缺少 file 字段，或请求体超过了大小限制")
		return
	}
	if fileHeader.Size > maxBytes {
		httpx.TooLarge(c, fmt.Sprintf("单文件不得超过 %d MB", d.Cfg.Limits.MaxUploadMB))
		return
	}

	src, err := fileHeader.Open()
	if err != nil {
		httpx.Internal(c, err, "读取上传内容失败")
		return
	}
	defer src.Close()

	// 边读边算哈希。多读 1 字节用于判断是否真的超限——
	// 只信 multipart 声明的 Size 是不够的，那个值同样来自客户端。
	var buf bytes.Buffer
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(&buf, hasher), io.LimitReader(src, maxBytes+1))
	if err != nil {
		httpx.Internal(c, err, "读取上传内容失败")
		return
	}
	if written > maxBytes {
		httpx.TooLarge(c, fmt.Sprintf("单文件不得超过 %d MB", d.Cfg.Limits.MaxUploadMB))
		return
	}
	if buf.Len() == 0 {
		httpx.BadRequest(c, "文件内容为空")
		return
	}

	data := buf.Bytes()
	info := imageinfo.Inspect(data)
	if !imageinfo.Allowed(info.Mime) {
		httpx.BadRequest(c, "不支持的文件类型：以内容判定为 "+info.Mime+"（仅支持 jpeg / png / gif / webp）")
		return
	}

	ownerID := auth.UserID(c)
	sum := hex.EncodeToString(hasher.Sum(nil))
	now := time.Now().Unix()

	// 去重：同一用户传过的相同内容直接复用，省一次上传也省一份存储
	if existing, err := d.Store.FindImageByHash(c.Request.Context(), ownerID, sum); err != nil {
		httpx.Internal(c, err, "查重失败")
		return
	} else if existing != nil {
		c.JSON(http.StatusOK, gin.H{
			"image":     toDTO(*existing, d),
			"duplicate": true,
		})
		return
	}

	folderID := c.PostForm("folder_id")

	// 解析目标目录，拿物化路径来拼 key。
	//
	// 目录不属于本人时 GetFolder 返回 nil，这里必须报错而不是退回根目录——
	// 静默回退会让「传了别人的 folder_id」看起来像上传成功，实际落到了根目录。
	folder, err := d.Store.GetFolder(c.Request.Context(), ownerID, folderID)
	if err != nil {
		httpx.Internal(c, err, "查询目录失败")
		return
	}
	if folder == nil {
		httpx.BadRequest(c, "目标目录不存在")
		return
	}

	id := idgen.New()
	key := objectKey(ownerID, folder.Path, id, info.Ext)

	ctx := c.Request.Context()
	if err := d.R2.Put(ctx, key, bytes.NewReader(data), int64(len(data)), info.Mime); err != nil {
		httpx.Internal(c, err, "上传到对象存储失败")
		return
	}

	im := store.Image{
		ID:         id,
		OwnerID:    ownerID,
		FolderID:   folderID,
		ObjectKey:  key,
		Filename:   cleanFilename(fileHeader.Filename, id, info.Ext),
		Ext:        info.Ext,
		Mime:       info.Mime,
		Size:       int64(len(data)),
		Width:      info.Width,
		Height:     info.Height,
		SHA256:     sum,
		Visibility: "public",
		CreatedAt:  now,
	}

	if err := d.Store.InsertImage(ctx, im); err != nil {
		// 并发重复上传会走到这里：唯一键把它挡下了。
		// 对象已经传上去，但内容相同，等于无害的重复对象，不专门回滚。
		if errors.Is(err, store.ErrDuplicate) {
			if dup, ferr := d.Store.FindImageByHash(ctx, ownerID, sum); ferr == nil && dup != nil {
				c.JSON(http.StatusOK, gin.H{"image": toDTO(*dup, d), "duplicate": true})
				return
			}
		}
		httpx.Internal(c, err, "写入图片记录失败")
		return
	}

	c.JSON(http.StatusCreated, gin.H{"image": toDTO(im, d), "duplicate": false})
}

// cleanFilename 得到用于展示的文件名。
// 原始名太长、去头去尾只剩后缀时，回退到 ULID + 扩展名。
func cleanFilename(name, id, ext string) string {
	name = strings.TrimSpace(path.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "" || name == "." || len(name) > 255 {
		return id + ext
	}
	return name
}

// listImages 分页列出根目录（M3 阶段）下的图片。
func listImages(c *gin.Context, d Deps) {
	ownerID := auth.UserID(c)
	folderID := c.Query("folder_id")

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(c.DefaultQuery("size", "50"))
	if size < 1 || size > 200 {
		size = 50
	}

	ctx := c.Request.Context()
	total, err := d.Store.CountImages(ctx, ownerID, folderID)
	if err != nil {
		httpx.Internal(c, err, "统计图片数量失败")
		return
	}

	items, err := d.Store.ListImages(ctx, ownerID, folderID, size, (page-1)*size)
	if err != nil {
		httpx.Internal(c, err, "查询图片列表失败")
		return
	}

	dtos := make([]imageDTO, 0, len(items))
	for _, im := range items {
		dtos = append(dtos, toDTO(im, d))
	}

	c.JSON(http.StatusOK, gin.H{
		"items":     dtos,
		"total":     total,
		"page":      page,
		"size":      size,
		"folder_id": folderID,
	})
}

// getImage 返回单张图片的详情。
func getImage(c *gin.Context, d Deps) {
	im, err := d.Store.GetImage(c.Request.Context(), auth.UserID(c), c.Param("id"))
	if err != nil {
		httpx.Internal(c, err, "查询图片失败")
		return
	}
	if im == nil {
		httpx.NotFound(c, "图片不存在")
		return
	}
	c.JSON(http.StatusOK, gin.H{"image": toDTO(*im, d)})
}

// deleteImage 把图片移入回收站（软删除，R2 对象保留到保留期结束）。
func deleteImage(c *gin.Context, d Deps) {
	affected, err := d.Store.SoftDeleteImage(
		c.Request.Context(), auth.UserID(c), c.Param("id"), time.Now().Unix())
	if err != nil {
		httpx.Internal(c, err, "删除图片失败")
		return
	}
	if affected == 0 {
		httpx.NotFound(c, "图片不存在或已在回收站")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// 移动失败的可区分原因。批量操作要按张给结果，
// 所以「该报 404」和「该报 400」必须能被调用方区分开。
var (
	errMoveNoImage  = errors.New("图片不存在或已在回收站")
	errMoveNoFolder = errors.New("目标目录不存在")
)

// patchImage 处理 PATCH /images/:id —— 承载「移动目录」与「改名」两类修改，
// 可只传其一，也可同时传。
//
// FolderID 用指针是为了区分三种请求语义：
//
//	nil      → 不移动（纯改名）
//	指向 ""  → 移动到根目录（根目录在 DB 里就是空串，与旧行为一致）
//	指向 id  → 移动到该目录
//
// 改名只动 DB 的 filename 字段：key 与 URL 由 ULID 决定，改名不破坏任何
// 已分享出去的链接，也不需要 R2 可用——所以 R2Ready 的门控只拦移动。
func patchImage(c *gin.Context, d Deps) {
	var body struct {
		FolderID *string `json:"folder_id"`
		Filename string  `json:"filename"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httpx.BadRequest(c, "请求体格式不正确")
		return
	}

	// 文件名是纯展示字段，宽容处理：去首尾空白；空串视为「不改」而非错误
	name := strings.TrimSpace(body.Filename)
	if body.FolderID == nil && name == "" {
		httpx.BadRequest(c, "没有要修改的内容：folder_id 与 filename 至少提供一个")
		return
	}
	if utf8.RuneCountInString(name) > 255 {
		httpx.BadRequest(c, "文件名过长（最多 255 字符）")
		return
	}

	ownerID := auth.UserID(c)
	ctx := c.Request.Context()

	im, err := d.Store.GetImage(ctx, ownerID, c.Param("id"))
	switch {
	case err != nil:
		httpx.Internal(c, err, "查询图片失败")
		return
	case im == nil:
		httpx.NotFound(c, "图片不存在")
		return
	}

	renamed := false
	if name != "" && name != im.Filename {
		// affected == 0 说明这条记录已进回收站（RenameImage 只改活记录），
		// 如实上报 renamed=false，而不是让前端拿到一个没生效的假成功
		affected, err := d.Store.RenameImage(ctx, ownerID, im.ID, name)
		if err != nil {
			httpx.Internal(c, err, "改名失败")
			return
		}
		renamed = affected > 0
	}

	moved := false
	if body.FolderID != nil {
		// 移动要在 R2 里复制对象，没有 R2 就做不了；纯改名不受此门控影响
		if !d.R2Ready() {
			httpx.Abort(c, http.StatusServiceUnavailable, "r2_unavailable",
				"对象存储未配置，暂时无法移动")
			return
		}
		im, err = doMove(ctx, d, ownerID, im.ID, *body.FolderID)
		switch {
		case errors.Is(err, errMoveNoImage):
			httpx.NotFound(c, "图片不存在")
			return
		case errors.Is(err, errMoveNoFolder):
			httpx.BadRequest(c, "目标目录不存在")
			return
		case err != nil:
			httpx.Internal(c, err, "移动失败")
			return
		}
		moved = true
	} else if renamed {
		// 改名后重取一次，保证返回给前端的是最新数据
		im, err = d.Store.GetImage(ctx, ownerID, im.ID)
		if err != nil || im == nil {
			httpx.Internal(c, err, "查询图片失败")
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"image": toDTO(*im, d), "moved": moved, "renamed": renamed})
}

// doMove 是移动的核心逻辑，单个移动与批量移动共用。
//
// 目录路径编码在 R2 的 key 里（SPEC 4.3 选定的方案 A），所以「移动」不是改一行库：
// 必须把对象复制到新 key、更新记录、再删掉旧对象。
//
// 三步顺序是刻意定死的，任何交换都会产生难以恢复的坏状态：
//
//	先删旧再改库 → 改库失败就留下一个指向已删除对象的记录
//	先改库再复制 → 库里指向一个还不存在的对象
//	复制 → 改库 → 删旧 → 中间失败只留下可重试的残留（多一个对象 / 多一个旧对象）
//
// 旧对象删除失败不报错：那只是一份孤儿数据，不影响任何正确性，
// 值得记日志但不值得让调用方当成失败。
//
// 已经在目标目录里时是**幂等空操作**：原样返回，不复制也不删除，
// 前端重试或批量里重复出现同一张图都不会产生多余的对象。
func doMove(ctx context.Context, d Deps, ownerID, id, folderID string) (*store.Image, error) {
	im, err := d.Store.GetImage(ctx, ownerID, id)
	if err != nil {
		return nil, err
	}
	if im == nil {
		return nil, errMoveNoImage
	}

	folder, err := d.Store.GetFolder(ctx, ownerID, folderID)
	if err != nil {
		return nil, err
	}
	if folder == nil {
		return nil, errMoveNoFolder
	}
	if folder.ID == im.FolderID {
		return im, nil
	}

	newKey := objectKey(ownerID, folder.Path, im.ID, im.Ext)
	if newKey == im.ObjectKey {
		return im, nil
	}

	if err := d.R2.Copy(ctx, im.ObjectKey, newKey); err != nil {
		return nil, err
	}

	affected, err := d.Store.MoveImage(ctx, ownerID, im.ID, folder.ID, newKey)
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		// 并发下图片可能刚被移入回收站。此时新对象已经复制出来了，
		// 但它永远不会被任何记录引用，直接删掉，别留孤儿。
		if derr := d.R2.Delete(ctx, newKey); derr != nil {
			log.Printf("警告: 移动失败后清理新对象 %s 也失败: %v", newKey, derr)
		}
		return nil, errMoveNoImage
	}

	if derr := d.R2.Delete(ctx, im.ObjectKey); derr != nil {
		log.Printf("警告: 移动后删除旧对象 %s 失败（孤儿对象，不影响正确性）: %v", im.ObjectKey, derr)
	}

	im.FolderID = folder.ID
	im.ObjectKey = newKey
	return im, nil
}
