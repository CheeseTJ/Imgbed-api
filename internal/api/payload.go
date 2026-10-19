package api

import (
	"github.com/gin-gonic/gin"

	"imgbed/internal/r2"
	"imgbed/internal/store"
)

// usagePayload 把用量统一成响应结构。
//
// 两个「占用」字段**口径不同**，前端用哪个取决于想表达什么：
//
//   - used_bytes：按图片元数据汇总（不含回收站）——业务视角的占用
//   - r2.used_bytes：Cloudflare 报告的真实占用（对象本体 + 元数据，**含回收站里的**）
//
// 侧栏的存储条用 **r2 的数**——那才是账单上的数。
// r2 为 null 表示 Cloudflare 的 usage 接口不可用（没配 token 或上游出错），
// 前端回退到 used_bytes，功能不受影响。
func usagePayload(u store.Usage, r2Usage *r2.BucketUsage, acct *r2.AccountUsage, curBucket string) gin.H {
	var r2Field any
	if r2Usage != nil {
		r2Field = gin.H{
			"used_bytes":     r2Usage.UsedBytes,
			"payload_bytes":  r2Usage.PayloadBytes,
			"metadata_bytes": r2Usage.MetadataBytes,
			"object_count":   r2Usage.ObjectCount,
			"quota_bytes":    r2Usage.QuotaBytes,
			"measured_at":    r2Usage.MeasuredAt,
		}
	}

	var acctField any
	if acct != nil {
		rows := make([]gin.H, 0, len(acct.Buckets))
		for _, b := range acct.Buckets {
			// current 用来在前端标记「存储条统计的是哪个桶」——
			// 图床桶与其余桶（如 music）分属两种口径，得让人一眼分清
			rows = append(rows, gin.H{
				"name":         b.Name,
				"used_bytes":   b.UsedBytes,
				"object_count": b.ObjectCount,
				"current":      b.Name == curBucket,
			})
		}
		acctField = gin.H{
			"buckets":            rows,
			"total_used_bytes":   acct.TotalUsed,
			"total_object_count": acct.TotalObjects,
		}
	}

	return gin.H{
		"image_count":  u.ImageCount,
		"folder_count": u.FolderCount,
		"used_bytes":   u.UsedBytes,
		"total_bytes":  0,
		"r2":           r2Field,
		"r2_account":   acctField,
	}
}
