package code

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"agri-steward/internal/agent/harness/compaction"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/model"
)

// CompactionRetained 是 compaction_retained（P2 L6，P2 §6.7 验收 1）：多轮会话样本在最终发给模型的每条 user 消息上
// 逐字段校验会话块 facts，取值与业务表重新读出的值比对（不与 facts 自身比对）：地块、作物 / 物候期、用户约束原句
// 及其所在轮次、已落卡结论、作业单与票据、否决药剂（驳回 PRODUCT / 用户取消）、时效实测、kb 引用；
// 另查不应出现的过时事实：作业单已驳回 / 取消的结论不得进 confirmed_conclusions，已被取代的实测不得进 measured。
// Value 为保留率，100% 才通过。
//
// 是质量维度（RedLine=false）：多轮样本只计入质量指标、不进红线 gate（P2 路线：多轮 = 质量）；
// 生产侧同一组白名单由 compaction.Assert 红线断言（失败关闭）。
// expect.compaction.compacted 时另核对确实压缩了：COMPACTION_APPLIED ≥ 1、无 kind=FALLBACK 的 COMPACTION_FAILED，
// 每条 prompt 的会话块带摘要（计入保留率分母）。
// 未声明 fixtures.session 或 enable_compaction 关（与 P1 相同不续接）为不适用。
type CompactionRetained struct{}

var _ grader.Grader = CompactionRetained{}

var (
	errNoSession = errors.New("session observation not recorded")
	errNoPrompts = errors.New("model prompts not recorded")
)

// maxRetainedEvidence 是证据里列出的缺失项上限（其余只计数）。
const maxRetainedEvidence = 8

// Name 返回打分器名。
func (CompactionRetained) Name() string { return "compaction_retained" }

// RedLine 返回 false（质量维度）。
func (CompactionRetained) RedLine() bool { return false }

// Grade 打分；声明了会话却缺会话观测或 prompt 观测（声明 compacted 时还有事件观测）返回 grader_error。
func (g CompactionRetained) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if err := checkTrial(g.Name(), t); err != nil {
		return grader.Score{}, err
	}
	if t.Sample.Fixtures.Session == nil {
		return grader.NotApplicable("no fixtures.session"), nil
	}
	obs := t.Session
	switch {
	case obs == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoSession)
	case !obs.Enabled && obs.Linked:
		return grader.Score{Evidence: "run linked to session while enable_compaction is off"}, nil
	case !obs.Enabled:
		return grader.NotApplicable("enable_compaction off: session not linked"), nil
	case !obs.Linked:
		return grader.Score{Evidence: "enable_compaction on but run not linked to the seeded session"}, nil
	case obs.Prompts == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoPrompts)
	case len(obs.Prompts) == 0:
		return grader.Score{Evidence: "no model prompt to check"}, nil
	}
	var constraints []string
	compacted := false
	if exp := t.Sample.Expect.Compaction; exp != nil {
		constraints, compacted = exp.Constraints, exp.Compacted
	}
	if compacted {
		if t.Events == nil {
			return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoEvents)
		}
		if why := compactionEventProblem(t.Events); why != "" {
			return grader.Score{Evidence: why}, nil
		}
	}
	items := retainedItems(obs.Source, obs.Superseded, constraints)
	kept, total := 0, 0
	var missing []string
	for i, p := range obs.Prompts {
		task, block, err := promptBlock(p)
		facts := block.Facts
		if compacted {
			total++
			if err == nil && block.Summary != nil && block.Summary.Text != "" {
				kept++
			} else {
				missing = append(missing, fmt.Sprintf("prompt %d (%s): no session summary", i, task))
			}
		}
		for _, it := range items {
			total++
			if err == nil && it.kept(facts) {
				kept++
				continue
			}
			why := it.name
			if err != nil {
				why = err.Error()
			}
			missing = append(missing, fmt.Sprintf("prompt %d (%s): %s", i, task, why))
		}
	}
	value := float64(kept) / float64(total)
	if kept == total {
		return grader.Score{Pass: true, Value: value}, nil
	}
	return grader.Score{Value: value, Evidence: retainedEvidence(kept, total, missing)}, nil
}

// retainedItem 是一个须保留的白名单取值：name 用于证据，kept 判定 facts 是否原样保留它。
type retainedItem struct {
	name string
	kept func(model.CompactionFacts) bool
}

// retainedItems 由业务表独立读出的会话原料与样本约束原句列出须保留的取值与不应出现的过时事实
// （规则与 compaction.BuildFacts 独立实现）。
func retainedItems(src model.SessionSource, superseded []uint64, constraints []string) []retainedItem {
	items := []retainedItem{{name: fmt.Sprintf("field_id %d", src.FieldID),
		kept: func(f model.CompactionFacts) bool { return f.FieldID == src.FieldID }}}
	if crop, stage, ok := latestCrop(src.Cards); ok {
		items = append(items, retainedItem{name: fmt.Sprintf("crop %s / stage %s", crop, stage),
			kept: func(f model.CompactionFacts) bool { return f.Crop == crop && f.PhenologyStage == stage }})
	}
	for _, c := range constraints {
		items = append(items, constraintItem(src.Turns, c))
	}
	voided := voidedAdvice(src.Orders)
	for _, c := range src.Cards {
		switch {
		case c.Outcome != model.OutcomeAdvice:
		case voided[c.AdviceID]:
			items = append(items, voidedConclusionItem(c.AdviceID))
		default:
			items = append(items, conclusionItem(c))
		}
	}
	for _, o := range src.Orders {
		items = append(items, orderItems(o)...)
	}
	for _, m := range src.Measured {
		items = append(items, measuredItem(m))
	}
	for _, id := range superseded {
		items = append(items, supersededItem(id))
	}
	seen := map[string]bool{}
	for _, id := range src.CitedKBIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		items = append(items, retainedItem{name: "cited kb " + id,
			kept: func(f model.CompactionFacts) bool { return slices.Contains(f.CitedKBIDs, id) }})
	}
	return items
}

// latestCrop 取最后一张带作物的建议卡的 crop / phenology_stage。
func latestCrop(cards []model.SessionCard) (crop, stage string, ok bool) {
	for _, c := range cards {
		if c.Crop != "" {
			crop, stage, ok = c.Crop, c.PhenologyStage, true
		}
	}
	return crop, stage, ok
}

// constraintItem：约束原句须出现在 user_constraints 中，且 turn_step_id 是业务表里含该句的轮次。
func constraintItem(turns []model.SessionTurn, text string) retainedItem {
	steps := map[string]bool{}
	for _, t := range turns {
		if strings.Contains(t.Question, text) {
			steps[model.TurnStepID(t.RunID)] = true
		}
	}
	return retainedItem{name: fmt.Sprintf("user constraint %q", text), kept: func(f model.CompactionFacts) bool {
		for _, uc := range f.UserConstraints {
			if steps[uc.TurnStepID] && strings.Contains(uc.Text, text) {
				return true
			}
		}
		return false
	}}
}

// conclusionItem：已落卡（ADVICE）建议卡的结论、剂量、药剂与作业窗口须原样出现在 confirmed_conclusions。
func conclusionItem(c model.SessionCard) retainedItem {
	return retainedItem{name: fmt.Sprintf("confirmed conclusion advice %d", c.AdviceID), kept: func(f model.CompactionFacts) bool {
		for _, cc := range f.ConfirmedConclusions {
			if cc.AdviceID == c.AdviceID && cc.Conclusion == c.Conclusion && cc.SelectedProduct == c.SelectedProduct &&
				jsonEqual(cc.Numbers, c.Numbers) && jsonEqual(cc.Window, c.Window) {
				return true
			}
		}
		return false
	}}
}

// voidedAdvice 是作业单已驳回 / 已取消的建议卡 advice_id（其结论不再算已确认）。
func voidedAdvice(orders []model.SessionOrderSource) map[uint64]bool {
	out := map[uint64]bool{}
	for _, o := range orders {
		if o.AdviceID != 0 && (o.Status == model.OrderRejected || o.Status == model.OrderCanceled) {
			out[o.AdviceID] = true
		}
	}
	return out
}

// voidedConclusionItem：作业单已驳回 / 取消的建议卡不得出现在 confirmed_conclusions（过时事实）。
func voidedConclusionItem(adviceID uint64) retainedItem {
	return retainedItem{name: fmt.Sprintf("voided conclusion advice %d present", adviceID), kept: func(f model.CompactionFacts) bool {
		return !slices.ContainsFunc(f.ConfirmedConclusions, func(cc model.ConfirmedConclusion) bool { return cc.AdviceID == adviceID })
	}}
}

// supersededItem：已被取代的时效实测不得出现在 measured（过时事实）。
func supersededItem(toolCallID uint64) retainedItem {
	return retainedItem{name: fmt.Sprintf("superseded measured tool_call %d present", toolCallID), kept: func(f model.CompactionFacts) bool {
		return !slices.ContainsFunc(f.Measured, func(m model.MeasuredFact) bool { return m.ToolCallID == toolCallID })
	}}
}

// orderItems：作业单与最新票据、驳回原因原样保留；有药剂时，以药剂不对（PRODUCT）驳回的须以 REJECTED_ORDER
// （ref 票据）、用户中止（CANCELED_BY_USER）的须以 USER_CANCELED（ref 作业单）出现在 vetoed_products。
func orderItems(o model.SessionOrderSource) []retainedItem {
	items := []retainedItem{{name: fmt.Sprintf("order %d", o.OrderID), kept: func(f model.CompactionFacts) bool {
		return slices.Contains(f.Orders, o.SessionOrder)
	}}}
	if o.Product == "" {
		return items
	}
	var want model.VetoedProduct
	switch {
	case o.TicketStatus == model.TicketRejected && o.RejectReasonCode == string(model.RejectProduct):
		want = model.VetoedProduct{Product: o.Product, Source: model.VetoedProductRejected, RefID: o.TicketID}
	case o.Status == model.OrderCanceled && o.CancelReason == model.ReasonCanceledByUser:
		want = model.VetoedProduct{Product: o.Product, Source: model.VetoedProductUserCanceled, RefID: o.OrderID}
	default:
		return items
	}
	return append(items, retainedItem{name: fmt.Sprintf("vetoed product %s (%s %d)", want.Product, want.Source, want.RefID),
		kept: func(f model.CompactionFacts) bool { return slices.Contains(f.VetoedProducts, want) }})
}

// measuredItem：时效实测的工具、摘要与取数时间原样保留。
func measuredItem(m model.MeasuredFact) retainedItem {
	return retainedItem{name: fmt.Sprintf("measured tool_call %d", m.ToolCallID), kept: func(f model.CompactionFacts) bool {
		for _, fm := range f.Measured {
			if fm.ToolCallID == m.ToolCallID && fm.ToolName == m.ToolName && fm.CollectedAt.Equal(m.CollectedAt) &&
				rawEqual(fm.ResultSummary, m.ResultSummary) {
				return true
			}
		}
		return false
	}}
}

// compactionEventProblem 核对 run_event：须有 COMPACTION_APPLIED，且不得有放弃压缩（kind=FALLBACK 或无 kind）的
// COMPACTION_FAILED；TRIMMED / SESSION_TRUNCATED 不算放弃。问题为空串表示通过。
func compactionEventProblem(events []grader.RunEvent) string {
	applied := 0
	for _, e := range events {
		switch e.Type {
		case model.EventCompactionApplied:
			applied++
		case model.EventCompactionFailed:
			var p struct {
				Kind   string `json:"kind"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return fmt.Sprintf("COMPACTION_FAILED payload undecodable: %v", err)
			}
			if p.Kind == "" || p.Kind == compaction.KindFallback {
				return "compaction fell back to full context: " + p.Reason
			}
		}
	}
	if applied == 0 {
		return "expect.compaction.compacted but no COMPACTION_APPLIED event"
	}
	return ""
}

// promptBlock 从一条 user 消息（{"task", "input": {..., "session": 会话块}}）取会话块。
func promptBlock(prompt string) (task string, block compaction.Block, err error) {
	var msg struct {
		Task  string `json:"task"`
		Input struct {
			Session *compaction.Block `json:"session"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(prompt), &msg); err != nil {
		return "?", block, fmt.Errorf("prompt is not a task message: %w", err)
	}
	if msg.Input.Session == nil {
		return msg.Task, block, errors.New("no input.session block")
	}
	return msg.Task, *msg.Input.Session, nil
}

// jsonEqual 按 JSON 编码比较（剂量 / 窗口含指针与时间，按取值而非地址比较）。
func jsonEqual(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// rawEqual 按 JSON 取值比较两段 JSON：MySQL JSON 列读出带空格、键按其内部顺序，prompt 组装时又按 Go map
// 重新编码（键排序），只比较取值。任一侧不是合法 JSON 时退回逐字节比较。
func rawEqual(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(va, vb)
}

func retainedEvidence(kept, total int, missing []string) string {
	head := fmt.Sprintf("retained %d/%d; missing: ", kept, total)
	if len(missing) > maxRetainedEvidence {
		return head + strings.Join(missing[:maxRetainedEvidence], "; ") +
			fmt.Sprintf("; … %d more", len(missing)-maxRetainedEvidence)
	}
	return head + strings.Join(missing, "; ")
}
