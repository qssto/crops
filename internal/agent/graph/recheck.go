package graph

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/model"
)

// errBadRecheck 表示复检入参不全（缺原 run 或 epoch 未递增）。
var errBadRecheck = errors.New("graph: recheck needs run_id and epoch > 0")

var _ agent.Rechecker = (*Agent)(nil)

// Recheck 下发前复检（agent.Rechecker，P0 §6.7 功能任务 3）：对原 run 以新 epoch 重跑规则层前段
// （classify → enrich → load_knowledge → candidates）。时效工具（工具注册表 TimeSensitive）的 call_key 含 epoch，
// 因而重取、不读缓存；非时效工具 call_key 不变，网关命中原结果。不经 runner、不写 run_step / 卡片 / 作业单、不调设备。
// 结论与原建议卡比对：仍为同一类作业且原选药剂仍在候选内即未变；模型不可用或时效数据缺失返回 ErrRecheckInconclusive。
func (a *Agent) Recheck(ctx context.Context, req agent.RecheckRequest) (agent.RecheckResult, error) {
	runID := req.Question.RunID
	if runID == 0 || req.Epoch <= 0 {
		return agent.RecheckResult{}, errBadRecheck
	}
	s, err := a.newState(ctx, req.Question, runID)
	if err != nil {
		return agent.RecheckResult{}, fmt.Errorf("graph: recheck run %d: %w", runID, err)
	}
	ectx := runner.WithEpoch(ctx, req.Epoch)
	for _, step := range []node{a.classify, a.enrich, a.loadKnowledge, a.candidates} {
		if s, err = step(ectx, s); err != nil {
			return agent.RecheckResult{}, fmt.Errorf("graph: recheck run %d: %w", runID, err)
		}
	}
	res, err := recheckVerdict(s, req.Card)
	if err != nil {
		return res, fmt.Errorf("graph: recheck run %d: %w", runID, err)
	}
	a.log.InfoContext(ctx, "pre-dispatch recheck", "run_id", runID, "epoch", req.Epoch, "changed", res.Changed, "detail", res.Detail)
	return res, nil
}

// recheckVerdict 比对复检状态与原建议卡。拒答为 LLM_UNAVAILABLE / DATA_MISSING 时无结论；其余拒答、转观察类、
// 换一级类或原选药剂不再是候选，均视为结论变化。
func recheckVerdict(s *state, card model.AdviceCard) (agent.RecheckResult, error) {
	switch s.refuse {
	case "":
	case RefuseLLMUnavailable, RefuseDataMissing:
		return agent.RecheckResult{}, fmt.Errorf("%w: %s: %s", agent.ErrRecheckInconclusive, s.refuse, s.detail)
	default:
		return changed("refused %s: %s", s.refuse, s.detail), nil
	}
	switch {
	case s.category() != card.Category:
		return changed("category %s -> %s", card.Category, s.category()), nil
	case !s.operation():
		return changed("advice type %s -> %s: %s", card.AdviceType, s.dec.AdviceType, s.dec.Text), nil
	case card.SelectedProduct != "" && !slices.Contains(s.candidates, card.SelectedProduct):
		return changed("product %s no longer admitted", card.SelectedProduct), nil
	}
	return agent.RecheckResult{}, nil
}

func changed(format string, args ...any) agent.RecheckResult {
	return agent.RecheckResult{Changed: true, Detail: fmt.Sprintf(format, args...)}
}
