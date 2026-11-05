package graph

import (
	"slices"
	"sync"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/guardrail"
	"agri-steward/internal/agent/knowledge"
)

// ---- P2 L4 覆盖检查：生育期、PHI、product_candidates 是否全部由规则层给出（docs/p2/L4-待下沉清单.md §覆盖检查）----

// productMentions（product_candidates）：模型原文推荐的药剂必须在规则层候选内；规则层无候选（非施药作业类、
// 拒答）时 Allowed 为空，原文肯定推荐任何药剂都拒收。未载入知识时为 nil（由护栏按「无快照」拒绝）。
func (s *state) productMentions() *guardrail.ProductMentions {
	if s.env == nil || s.env.snap == nil {
		return nil
	}
	m := &guardrail.ProductMentions{Lexicon: s.env.lexicon()}
	for _, id := range s.candidates {
		m.Allowed = append(m.Allowed, s.env.commonName(id))
	}
	return m
}

// lexicons 是按知识快照缓存的药名词表（*knowledge.Snapshot → calc.Lexicon）：快照由 knowledge.Store 按 ref
// 进程内缓存且只读，条目数以已打开的知识版本数为界。
var lexicons sync.Map

// lexicon 返回本次快照的药名词表（同一快照只构造一次）。
func (e *env) lexicon() calc.Lexicon {
	if v, ok := lexicons.Load(e.snap); ok {
		return v.(calc.Lexicon)
	}
	v, _ := lexicons.LoadOrStore(e.snap, calc.NewLexicon(e.snap.Registry()))
	return v.(calc.Lexicon)
}

// stageApplies（生育期）：条目适用生育期含当前生育期（或为全生育期）。当前生育期未知时不在这里拦
// （物候缺失由数据缺失路径处理）。
func stageApplies(en *knowledge.Entry, stage string) bool {
	return stage == "" || len(en.Stage) == 0 || slices.Contains(en.Stage, knowledge.Wildcard) || slices.Contains(en.Stage, stage)
}

// PHI：问题没提收获时规则层不按 PHI 过滤候选，由 to_order 开票前约束闸门的 phi_interval 拦截（P1 L3 设计，
// 红线样本 rice-cons-0012 / 0014 / 0040 断言「作业类 + 单留 DRAFT」）；覆盖检查确认该路径无绕过，不在规则层重复过滤。
