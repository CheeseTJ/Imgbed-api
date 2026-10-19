package api

import (
	"context"
	"log"
	"time"
)

// StartTrashCleanup 启动回收站的过期清理循环。
//
// **这是「保留 7 天」这句承诺的真正实现。** 没有它，删除确认弹窗里
// 「7 天后才真正删除」就是一句谎话——软删除的图会永久留在库里和 R2 里。
//
// 每小时扫一次「deleted_at 已超过保留期」的记录。
// 用 ticker 而不是一次性 timer：服务长期运行，清理要反复发生。
// ctx 取消（服务退出）时循环随之结束。
func StartTrashCleanup(ctx context.Context, d Deps, interval time.Duration) {
	if d.R2 == nil {
		log.Printf("回收站清理未启动：对象存储未配置")
		return
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		log.Printf("回收站清理已启动，间隔 %s，保留期 %d 天",
			interval, d.Cfg.Limits.TrashRetainDays)

		for {
			select {
			case <-ctx.Done():
				log.Printf("回收站清理已停止")
				return
			case <-ticker.C:
				purgeExpiredTrash(ctx, d)
			}
		}
	}()
}

// purgeExpiredTrash 删掉所有已过保留期的回收站记录。
//
// 单轮最多 500 条：处理不完留给下一轮，避免一次跑太久。
// **任何一条失败都直接返回**——下一轮会重试，而失败的操作没有副作用：
// 删对象是幂等的，删行也只在对象删成功之后才发生。
func purgeExpiredTrash(ctx context.Context, d Deps) {
	cutoff := time.Now().Add(-d.Cfg.Limits.TrashRetention()).Unix()

	items, err := d.Store.ListExpiredTrash(ctx, cutoff, 500)
	if err != nil {
		log.Printf("回收站清理: 查询过期项失败: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}

	purged := 0
	for _, im := range items {
		// 先删对象再删行，与 purgeImage 的顺序约定一致
		if err := d.R2.Delete(ctx, im.ObjectKey); err != nil {
			log.Printf("回收站清理: 删除对象 %s 失败，本轮到此为止: %v", im.ObjectKey, err)
			log.Printf("回收站清理: 本轮已物理删除 %d 张", purged)
			return
		}
		if _, err := d.Store.PurgeImage(ctx, im.OwnerID, im.ID); err != nil {
			log.Printf("回收站清理: 删除记录 %s 失败，本轮到此为止: %v", im.ID, err)
			log.Printf("回收站清理: 本轮已物理删除 %d 张", purged)
			return
		}
		purged++
	}

	log.Printf("回收站清理: 本轮物理删除 %d 张过期图片", purged)
}
