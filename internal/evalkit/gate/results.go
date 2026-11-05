package gate

import (
	"slices"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

// RunResults 求写进快照的 L7-b 运行结果（trial 表没有对应列）：触发上下文压缩（timeline 有 COMPACTION_APPLIED）的
// trial 数，与未声明工具调用清单。holdout 样本只记 sample_id / trial 序号（不记工具名，报告只给 ID）。
func RunResults(trials []evalkit.Trial) (compactions int, calls []evalkit.UnexpectedCallRef) {
	for _, t := range trials {
		if compacted(t) {
			compactions++
		}
		if len(t.UnexpectedCalls) == 0 {
			continue
		}
		ref := evalkit.UnexpectedCallRef{SampleID: t.SampleID, TrialIndex: t.TrialIndex, RunID: t.RunID, Tools: []string{}}
		if !t.IsHoldout() {
			ref.Tools = slices.Clone(t.UnexpectedCalls)
		}
		calls = append(calls, ref)
	}
	return compactions, calls
}

func compacted(t evalkit.Trial) bool {
	if t.Detail == nil {
		return false
	}
	for _, e := range t.Detail.Timeline {
		if e.Type == model.EventCompactionApplied {
			return true
		}
	}
	return false
}
