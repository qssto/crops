package service

import (
	"encoding/json"
	"fmt"

	"agri-steward/internal/model"
)

// blockedPayloadMax 是 CONSTRAINT_BLOCKED payload 的字节上限（run_event.payload ≤ 8KB，与 audit_log.summary 一致）。
const blockedPayloadMax = 8 << 10

// blockedPayload 生成 CONSTRAINT_BLOCKED payload（复核第 2 条遗留）：除 rule_ids 外带上本次未通过的校验行
// （rule_id、observed、threshold 含 kb:<kb_id>@<version>、note），作为该次拦截的不可篡改证据——constraint_check 的
// PRE_APPROVAL 行只保留最近一次复核结果。超过 blockedPayloadMax 时从 checks 末尾逐行截掉（仍超再截 rule_ids）并置 truncated。
func blockedPayload(orderID uint64, ruleIDs []string, checks []model.ConstraintCheck) (json.RawMessage, error) {
	p := model.ConstraintBlockedPayload{OrderID: orderID, RuleIDs: ruleIDs, Checks: []model.BlockedCheck{}}
	for _, c := range checks {
		if c.Passed {
			continue
		}
		note, err := observedNote(c.Observed)
		if err != nil {
			return nil, fmt.Errorf("blocked check %s: %w", c.RuleID, err)
		}
		p.Checks = append(p.Checks, model.BlockedCheck{RuleID: c.RuleID, Observed: c.Observed, Threshold: c.Threshold, Note: note})
	}
	for {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("encode %s payload: %w", model.EventConstraintBlocked, err)
		}
		switch {
		case len(b) <= blockedPayloadMax:
			return b, nil
		case len(p.Checks) > 0:
			p.Checks = p.Checks[:len(p.Checks)-1]
		case len(p.RuleIDs) > 0:
			p.RuleIDs = p.RuleIDs[:len(p.RuleIDs)-1]
		default:
			return nil, fmt.Errorf("%s payload %d bytes exceeds %d", model.EventConstraintBlocked, len(b), blockedPayloadMax)
		}
		p.Truncated = true
	}
}

// observedNote 取 observed.note（未通过原因）；observed 为空返回空串。
func observedNote(observed json.RawMessage) (string, error) {
	if len(observed) == 0 {
		return "", nil
	}
	var o struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal(observed, &o); err != nil {
		return "", fmt.Errorf("decode observed: %w", err)
	}
	return o.Note, nil
}
