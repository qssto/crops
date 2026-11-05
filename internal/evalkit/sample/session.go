package sample

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"agri-steward/internal/model"
)

// P2 L6 多轮长会话样本（P2 §6.7 功能任务 6）：fixtures.session 声明本次提问之前的轮次，Runner 写进 trial 库后
// 按生产路径带 prev_run_id 建本次 run；expect.compaction 声明须原样保留的用户约束原句（compaction_retained）。

// RuleSession 是 fixtures.session / expect.compaction 的结构校验。
const RuleSession = "session"

// MaxSessionTurns 是 fixtures.session.turns 的上限：会话链最多 32 个 run（含本次提问）。
const MaxSessionTurns = 31

// 种子轮次的字段长度上限（与 agent_run 问题上限、advice_card.conclusion / selected_product 列宽一致）。
const (
	maxSessionQuestionRunes   = 500
	maxSessionConclusionRunes = 512
	maxSessionProductRunes    = 64
)

// SessionFixture 是本次提问之前的会话轮次（由早到晚）。PhenologyStage 写入各轮建议卡（crop 取样本 crop）。
type SessionFixture struct {
	PhenologyStage string        `yaml:"phenology_stage,omitempty"`
	Turns          []SessionTurn `yaml:"turns"`
}

// SessionTurn 是一轮已结束的问答：Question 写 RUN_STARTED；Conclusion 非空时再写一张 ADVICE 建议卡，
// Product 为其 selected_product，Cite 为其 kb 引用（kb_id@version）；Order 为该卡的作业单（须有 Conclusion），
// Measured 为该轮的时效实测工具结果。
type SessionTurn struct {
	Question   string            `yaml:"question"`
	Conclusion string            `yaml:"conclusion,omitempty"`
	Product    string            `yaml:"product,omitempty"`
	Cite       []string          `yaml:"cite,omitempty"`
	Order      *SessionOrder     `yaml:"order,omitempty"`
	Measured   []SessionMeasured `yaml:"measured,omitempty"`
}

// SessionOrder 是种子建议卡的终态作业单（只播种终态，不进派发链路）：
//   - REJECTED：一张已驳回的 ORDER 票与驳回原因（RejectCode / RejectText 必填）；
//   - CANCELED：一张已通过的票，再迁移 APPROVED → CANCELED，CancelReason 为该迁移的 reason_code（必填）。
type SessionOrder struct {
	Status       string `yaml:"status"`
	RejectCode   string `yaml:"reject_code,omitempty"`
	RejectText   string `yaml:"reject_text,omitempty"`
	CancelReason string `yaml:"cancel_reason,omitempty"`
}

// SessionMeasured 是一条时效（time_sensitive）工具结果；Superseded 为已被更新结果取代（不得进入会话块 facts）。
type SessionMeasured struct {
	Tool       string         `yaml:"tool"`
	Result     map[string]any `yaml:"result"`
	Superseded bool           `yaml:"superseded,omitempty"`
}

// 种子作业单的终态与取消原因取值。
var (
	sessionOrderStatuses = []string{string(model.OrderRejected), string(model.OrderCanceled)}
	sessionCancelReasons = []string{string(model.ReasonCanceledByUser), string(model.ReasonConditionChanged)}
)

// CompactionExpect 是长对话压缩保留期望：Constraints 是须原样出现在会话块 facts.user_constraints 的用户约束原句，
// 每条须是某一轮 question 的子串（打分器据重新读出的 RUN_STARTED 定位所在轮次）。
// Compacted 声明本次 run 必须新做一次压缩（会话自由文本稳超 compaction.trigger_tokens）：打分器另要求有
// COMPACTION_APPLIED 事件、无 kind=FALLBACK 的 COMPACTION_FAILED，且每条 prompt 的会话块带摘要，
// 防止样本因未达阈值根本不压缩而「保留率 100%」空过。
type CompactionExpect struct {
	Constraints []string `yaml:"constraints"`
	Compacted   bool     `yaml:"compacted,omitempty"`
}

// SplitCite 把 kb_id@version 拆成 kb_id 与 version（按最后一个 @）；格式不对返回 false。
func SplitCite(ref string) (id, version string, ok bool) {
	i := strings.LastIndex(ref, "@")
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

func checkSession(s *Sample) []string {
	fx, exp := s.Fixtures.Session, s.Expect.Compaction
	if fx == nil {
		if exp != nil {
			return []string{"expect.compaction requires fixtures.session"}
		}
		return nil
	}
	var msgs []string
	if n := len(fx.Turns); n == 0 || n > MaxSessionTurns {
		msgs = append(msgs, fmt.Sprintf("fixtures.session.turns must be 1..%d, got %d", MaxSessionTurns, n))
	}
	for i, t := range fx.Turns {
		msgs = append(msgs, sessionTurnProblems(s, fx, i, t)...)
	}
	if exp == nil {
		return msgs
	}
	if len(exp.Constraints) == 0 {
		msgs = append(msgs, "expect.compaction.constraints is required")
	}
	for i, c := range exp.Constraints {
		if strings.TrimSpace(c) != c || c == "" {
			msgs = append(msgs, fmt.Sprintf("expect.compaction.constraints[%d] must be non-empty without surrounding spaces", i))
			continue
		}
		if !inSomeTurn(fx.Turns, c) {
			msgs = append(msgs, fmt.Sprintf("expect.compaction.constraints[%d] %q is not in any fixtures.session question", i, c))
		}
	}
	return msgs
}

func sessionTurnProblems(s *Sample, fx *SessionFixture, i int, t SessionTurn) []string {
	var msgs []string
	at := fmt.Sprintf("fixtures.session.turns[%d]", i)
	if q := strings.TrimSpace(t.Question); q == "" || utf8.RuneCountInString(q) > maxSessionQuestionRunes {
		msgs = append(msgs, fmt.Sprintf("%s.question must be 1..%d chars", at, maxSessionQuestionRunes))
	}
	if t.Conclusion == "" {
		if t.Product != "" || len(t.Cite) > 0 || t.Order != nil {
			msgs = append(msgs, at+": product / cite / order require conclusion (no advice card)")
		}
		return append(msgs, sessionExtraProblems(at, t)...)
	}
	if utf8.RuneCountInString(t.Conclusion) > maxSessionConclusionRunes {
		msgs = append(msgs, fmt.Sprintf("%s.conclusion exceeds %d chars", at, maxSessionConclusionRunes))
	}
	if utf8.RuneCountInString(t.Product) > maxSessionProductRunes {
		msgs = append(msgs, fmt.Sprintf("%s.product exceeds %d chars", at, maxSessionProductRunes))
	}
	if s.Crop == "" || fx.PhenologyStage == "" {
		msgs = append(msgs, at+": conclusion requires sample crop and fixtures.session.phenology_stage")
	}
	for j, ref := range t.Cite {
		if _, _, ok := SplitCite(ref); !ok {
			msgs = append(msgs, fmt.Sprintf("%s.cite[%d] %q is not kb_id@version", at, j, ref))
		}
	}
	return append(msgs, sessionExtraProblems(at, t)...)
}

// sessionExtraProblems 校验轮次的作业单与时效实测。
func sessionExtraProblems(at string, t SessionTurn) []string {
	var msgs []string
	for j, m := range t.Measured {
		if strings.TrimSpace(m.Tool) == "" || len(m.Result) == 0 {
			msgs = append(msgs, fmt.Sprintf("%s.measured[%d]: tool and result are required", at, j))
		} else if _, err := json.Marshal(m.Result); err != nil {
			msgs = append(msgs, fmt.Sprintf("%s.measured[%d].result: %v", at, j, err))
		}
	}
	o := t.Order
	if o == nil {
		return msgs
	}
	switch {
	case !slices.Contains(sessionOrderStatuses, o.Status):
		msgs = append(msgs, fmt.Sprintf("%s.order.status must be one of %v", at, sessionOrderStatuses))
	case o.Status == string(model.OrderRejected) &&
		(!slices.Contains(model.RejectReasonCodes, model.RejectReasonCode(o.RejectCode)) || o.RejectText == "" || o.CancelReason != ""):
		msgs = append(msgs, at+".order: REJECTED requires reject_code (approval reason code) and reject_text, no cancel_reason")
	case o.Status == string(model.OrderCanceled) &&
		(!slices.Contains(sessionCancelReasons, o.CancelReason) || o.RejectCode != "" || o.RejectText != ""):
		msgs = append(msgs, fmt.Sprintf("%s.order: CANCELED requires cancel_reason in %v, no reject_*", at, sessionCancelReasons))
	}
	return msgs
}

func inSomeTurn(turns []SessionTurn, text string) bool {
	for _, t := range turns {
		if strings.Contains(t.Question, text) {
			return true
		}
	}
	return false
}
