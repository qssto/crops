package model

import "time"

// KBLoadItem 是 kb_load_item 一行（迁移 0015，P2 L5）：一次知识加载的一个条目。诊断数据，保留期见
// configs/app.yaml retention.table_days.kb_load_item。
type KBLoadItem struct {
	ID           uint64
	TenantID     uint64
	FarmID       uint64
	RunID        uint64
	LoadSeq      int    // 同一 run 内第几次加载，从 0 开始
	RankNo       int    // 排序名次，从 0 开始
	QuestionType string // 一级 taxonomy code（protect / fertilize / irrigate / disaster）
	KBID         string
	KBVersion    string
	PickReason   string // MUST_INCLUDE / RANKED / COMMON
	Strategy     string // LoaderStrategyV0 / LoaderStrategyV1
	CreatedAt    time.Time
}

// KBLoadItem.PickReason 取值（与 knowledge.PickReason 同值）。线上加载精度只统计 RANKED。
const (
	PickReasonMustInclude = "MUST_INCLUDE"
	PickReasonRanked      = "RANKED"
	PickReasonCommon      = "COMMON"
)
