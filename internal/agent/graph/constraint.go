package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"agri-steward/internal/model"
)

// ruleIDMax 是 constraint_check.rule_id 的长度上限（varchar(48)）。
const ruleIDMax = 48

// ruleIDDigestPrefix 是超长 rule_id 改用摘要时的前缀。
const ruleIDDigestPrefix = "h:"

// checkObserved 是 constraint_check.observed（§6.6 DDL 注释：{value, unit, source, collected_at, tool_call_id}）。
type checkObserved struct {
	Value       json.RawMessage `json:"value"`
	Unit        string          `json:"unit"`
	Source      string          `json:"source"`
	CollectedAt time.Time       `json:"collected_at"`
	ToolCallID  uint64          `json:"tool_call_id"`
}

// checkThreshold 是 constraint_check.threshold（带 kb_id@version 与阈值定义）。
type checkThreshold struct {
	KB           string  `json:"kb"`
	ThresholdID  string  `json:"threshold_id"`
	Op           string  `json:"op"`
	Value        float64 `json:"value"`
	Unit         string  `json:"unit"`
	DurationDays int     `json:"duration_days,omitempty"`
}

// constraintChecks 把规则层判定过的阈值转为审批前约束校验（PRE_APPROVAL，attempt 由开票时取 ticket_seq）：
// rule_id 为 kb_id#threshold_id（同一阈值多次判定只取第一次），passed 为阈值条件是否满足（P0 只展示、不拦截）。
// 实测值无 tool_call_id 来源的阈值不写（与 measured 引用同一规则）。
func (a *Agent) constraintChecks(s *state) []model.ConstraintCheck {
	out := []model.ConstraintCheck{}
	seen := map[string]bool{}
	for _, j := range s.dec.Judged {
		id := ruleID(j.KB, j.Th.ID)
		if seen[id] {
			continue
		}
		m, ok := a.measured(s, j.Fact)
		if !ok {
			continue
		}
		en, ok := s.env.snap.Entry(j.KB)
		if !ok {
			continue
		}
		obs, err := json.Marshal(checkObserved{Value: m.Value, Unit: j.Th.Unit, Source: m.Source,
			CollectedAt: m.CollectedAt, ToolCallID: m.ToolCallID})
		if err != nil {
			a.log.Warn("constraint observed not encodable", "run_id", s.runID, "rule_id", id, "err", err)
			continue
		}
		th, err := json.Marshal(checkThreshold{KB: en.Ref(), ThresholdID: j.Th.ID, Op: j.Th.Op, Value: j.Th.Value,
			Unit: j.Th.Unit, DurationDays: j.Th.DurationDays})
		if err != nil {
			a.log.Warn("constraint threshold not encodable", "run_id", s.runID, "rule_id", id, "err", err)
			continue
		}
		seen[id] = true
		out = append(out, model.ConstraintCheck{Phase: model.PhasePreApproval, RuleID: id, Passed: j.Met,
			Observed: obs, Threshold: th})
	}
	return out
}

// ruleID 是 kb_id#threshold_id；超过列宽时改为 h: + sha256 前缀（threshold 列仍带完整 kb_id 与阈值 id）。
func ruleID(kbID, thresholdID string) string {
	id := kbID + "#" + thresholdID
	if len(id) <= ruleIDMax {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return ruleIDDigestPrefix + hex.EncodeToString(sum[:])[:ruleIDMax-len(ruleIDDigestPrefix)]
}
