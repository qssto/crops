package code

import (
	"context"
	"fmt"
	"sort"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// RuleReferencePasses 是 cmd/eval validate 的规则名：样本参考解须通过全部打分器（P0 L4 验收 5）。
const RuleReferencePasses = "reference_passes_graders"

// refID 是参考解卡片里的占位主键（schema 要求 ≥ 1；outcome_match 忽略 ID）。
const refID = 1

// 参考解无法构造、因而不判定的维度说明（cmd/eval validate 汇总输出）。
const (
	skipNumberPending = "numbers 期望值待 calc 回填"
	skipMaxTokens     = "budget.max_tokens：L4 无 token 计量"
	skipNoCategory    = "schema_valid：拒答样本 tags 无 taxonomy 类别"
)

// Reference 由样本 expect 构造参考解 trial：卡片字段取 expect（引用取 must_cite，数值引用指向首条 must_cite），
// 工具调用取 must_call 各一次；fixtures 注入的故障记为工具故障；L5 观测取理想值（设备零调用、日志为空、
// 作业单处于 work_order.final_status，断言 stop_at_approval 时有一张 PREPARING 票）。
// skip 为不判定的打分器名 → 原因；notes 为部分构造的原因（每条至多一次）。
func Reference(s *sample.Sample, tx *knowledge.Taxonomy, kb grader.KnowledgeIndex) (t *grader.Trial, skip map[string]string, notes []string) {
	exp := s.Expect
	card := &model.AdviceCard{AdviceID: refID, RunID: refID, TenantID: refID, FarmID: refID, FieldID: refID,
		Crop: tx.Crop, Outcome: exp.Outcome, RefuseReason: exp.RefuseReason, Reasons: []string{},
		Citations: []model.Citation{}, NextAction: model.NextNone}
	skip = map[string]string{}
	if a := exp.Advice; a != nil {
		if fillAdvice(card, a) {
			notes = append(notes, skipNumberPending)
		}
	} else if cat, ok := refusalCategory(s, tx); ok {
		card.Category = cat
	} else {
		skip[nameSchemaValid] = skipNoCategory
	}
	if b := exp.Trajectory.Budget; b != nil && b.MaxTokens > 0 {
		notes = append(notes, skipMaxTokens)
	}
	calls := make(map[string]int, len(exp.Trajectory.MustCall))
	for _, tool := range exp.Trajectory.MustCall {
		calls[tool] = 1
	}
	return &grader.Trial{Sample: s, Outcome: exp.Outcome, RefuseReason: exp.RefuseReason,
		Card: card, KB: kb, ToolCalls: calls, ToolFailures: referenceFailures(s),
		Approval: referenceApproval(exp), DeviceCalls: []string{}, Logs: []byte{},
		OrderTransitions: referenceTransitions(exp.WorkOrder)}, skip, notes
}

// referenceFailures 把 fixtures.mcp 中注入的故障记为 "tool:fault"（按工具名排序；无故障为空切片）。
func referenceFailures(s *sample.Sample) []string {
	out := []string{}
	for tool, fxs := range s.Fixtures.MCP {
		for _, fx := range fxs {
			if fx.Error != "" {
				out = append(out, tool+":"+fx.Error)
			}
		}
	}
	sort.Strings(out)
	return out
}

// referenceApproval 是理想审批状态：作业单处于期望终态；断言 stop_at_approval 时有一张 PREPARING 票。
func referenceApproval(exp sample.Expect) *grader.ApprovalState {
	st := &grader.ApprovalState{}
	if exp.WorkOrder != nil {
		st.OrderStatus = model.WorkOrderStatus(exp.WorkOrder.FinalStatus)
	}
	if exp.Trajectory.StopAtApproval {
		st.Ticket = &model.ApprovalTicket{TicketID: refID, Status: model.TicketPreparing}
		st.RunStatus, st.Interrupt = model.RunInterrupted, model.InterruptWaitApproval
		if st.OrderStatus == "" {
			st.OrderStatus = model.OrderPendingApproval
		}
	}
	return st
}

// referenceTransitions 构造参考作业单迁移（L6 state_machine_legal）：建单后按合法迁移表走到 final_status 的最短路径；
// 无 work_order 断言时为空（不涉及作业单），final_status 不可达时只含建单（由打分器之外的规则报）。
func referenceTransitions(w *sample.WorkOrderExpect) []model.OrderTransition {
	ref := model.OrderRef{TenantID: refID, FarmID: refID, OrderID: refID}
	out := []model.OrderTransition{}
	if w == nil {
		return out
	}
	out = append(out, model.OrderTransition{Ref: ref, RunID: refID, To: model.OrderDraft})
	path := shortestPath(model.OrderDraft, model.WorkOrderStatus(w.FinalStatus))
	for i := 1; i < len(path); i++ {
		out = append(out, model.OrderTransition{Ref: ref, RunID: refID, From: path[i-1], To: path[i]})
	}
	return out
}

// shortestPath 在 model.WorkOrderTransitions 上 BFS from → to，返回含两端的状态序列；不可达返回 nil。
func shortestPath(from, to model.WorkOrderStatus) []model.WorkOrderStatus {
	prev := map[model.WorkOrderStatus]model.WorkOrderStatus{from: ""}
	queue := []model.WorkOrderStatus{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == to {
			var path []model.WorkOrderStatus
			for st := to; st != ""; st = prev[st] {
				path = append([]model.WorkOrderStatus{st}, path...)
			}
			return path
		}
		for _, next := range model.WorkOrderTransitions[cur] {
			if _, seen := prev[next]; !seen {
				prev[next] = cur
				queue = append(queue, next)
			}
		}
	}
	return nil
}

// fillAdvice 按 expect.advice 填卡；返回是否有期望值待 calc 回填的数值（该条不进卡片）。
func fillAdvice(card *model.AdviceCard, a *sample.AdviceExpect) (pending bool) {
	card.Category = model.Category(a.Category)
	card.AdviceType = model.AdviceType(a.AdviceType)
	card.Conclusion = a.Conclusion
	card.NextAction = model.NextObserve
	if card.AdviceType == model.AdviceTypeOperation {
		card.NextAction = model.NextCreateOrder
	}
	var first model.Citation
	for i, ref := range a.MustCite {
		id, v, err := knowledge.ParseCitationRef(ref)
		if err != nil {
			continue // must_cite_kb 已报
		}
		c := model.Citation{TenantID: refID, FarmID: refID, Type: model.CitationKB, KBID: id, KBVersion: v}
		if i == 0 {
			first = c
		}
		card.Citations = append(card.Citations, c)
	}
	for _, n := range a.Numbers {
		if n.Value == nil {
			pending = true
			continue
		}
		card.Numbers = append(card.Numbers, model.Number{Field: n.Field, Value: *n.Value, Unit: n.Unit})
		if first.KBID != "" {
			c := first
			c.ClaimPath = fmt.Sprintf("numbers[%d]", len(card.Numbers)-1)
			card.Citations = append(card.Citations, c)
		}
	}
	if len(a.ProductIn) > 0 {
		card.ProductCandidates = a.ProductIn
		card.SelectedProduct = a.ProductIn[0]
	}
	return pending
}

// refusalCategory 取样本首个 taxonomy tag 的一级类别（拒答卡也必须带 category）。
func refusalCategory(s *sample.Sample, tx *knowledge.Taxonomy) (model.Category, bool) {
	for _, tag := range s.Tags {
		if cat, ok := tx.CategoryOf(tag); ok {
			return model.Category(cat), true
		}
	}
	return "", false
}

// ReferenceCheck 是 validate 规则：每个样本的参考解跑全部打分器，失败或 grader_error 记违规。
// knowledge_ref 打不开的样本由 must_cite_kb 报，此处跳过。Notes 汇总不判定维度（原因 → 样本数）。
type ReferenceCheck struct {
	tx      *knowledge.Taxonomy
	kb      sample.KnowledgeOpener
	graders []grader.Grader
	notes   map[string]int
}

var _ sample.Rule = (*ReferenceCheck)(nil)

// NewReferenceCheck 构造规则；graders 通常为 All(schema)。
func NewReferenceCheck(tx *knowledge.Taxonomy, kb sample.KnowledgeOpener, graders []grader.Grader) *ReferenceCheck {
	return &ReferenceCheck{tx: tx, kb: kb, graders: graders, notes: map[string]int{}}
}

// Name 返回规则名。
func (*ReferenceCheck) Name() string { return RuleReferencePasses }

// Check 实现 sample.Rule。
func (r *ReferenceCheck) Check(samples []*sample.Sample) []sample.Violation {
	ctx := context.Background()
	r.notes = map[string]int{}
	var out []sample.Violation
	for _, s := range samples {
		snap, err := r.kb.Open(ctx, s.KnowledgeRef)
		if err != nil {
			continue
		}
		t, skip, notes := Reference(s, r.tx, snap)
		for _, reason := range skip {
			r.notes[reason]++
		}
		for _, n := range notes {
			r.notes[n]++
		}
		for _, g := range r.graders {
			if _, skipped := skip[g.Name()]; skipped {
				continue
			}
			sc, err := g.Grade(ctx, t)
			msg := ""
			switch {
			case err != nil:
				msg = fmt.Sprintf("grader_error %s: %v", g.Name(), err)
			case !sc.Pass:
				msg = fmt.Sprintf("reference solution fails %s: %s", g.Name(), sc.Evidence)
			default:
				continue
			}
			out = append(out, sample.Violation{SampleID: s.ID, Path: s.Path, Rule: RuleReferencePasses, Msg: msg})
		}
	}
	return out
}

// Notes 返回上次 Check 中不判定维度的说明，按原因排序，形如「原因 ×样本数」。
func (r *ReferenceCheck) Notes() []string {
	out := make([]string, 0, len(r.notes))
	for reason, n := range r.notes {
		out = append(out, fmt.Sprintf("%s ×%d", reason, n))
	}
	sort.Strings(out)
	return out
}
