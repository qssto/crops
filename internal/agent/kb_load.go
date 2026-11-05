package agent

import (
	"context"

	"agri-steward/internal/model"
)

// ---- P2 L5：知识加载明细（kb_load_item，迁移 0015）----

// KBLoadItemStore 写一次加载 (run_id, load_seq) 的条目明细：整批替换该次加载的旧行（resume 重跑后只留最后一次）。
// 实现：repository.KBLoadItemRepo。写失败只记日志，不让 run 失败（诊断数据）。
type KBLoadItemStore interface {
	ReplaceKBLoadItems(ctx context.Context, scope model.Scope, items []model.KBLoadItem) error
}
