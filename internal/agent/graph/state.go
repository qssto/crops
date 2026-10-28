package graph

import (
	"slices"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/calc"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// 拒答原因（advice_card.refuse_reason，与样本 expect.refuse_reason 一致）。
const (
	RefuseOutOfScope     = "OUT_OF_SCOPE"
	RefuseDataMissing    = "DATA_MISSING"
	RefuseKBMiss         = knowledge.RefuseReasonKBMiss
	RefuseLLMUnavailable = "LLM_UNAVAILABLE"
	RefuseValidation     = "VALIDATION_FAILED"
)

// 图节点名（run_step.step_id / node）。
const (
	NodeClassify  = "classify"
	NodeEnrich    = "enrich"
	NodeKnowledge = "load_knowledge"
	NodeCands     = "candidates"
	NodeChoose    = "choose"
	NodeCompute   = "compute"
	NodeExplain   = "explain"
	NodeAssemble  = "assemble"
	NodeValidate  = "validate"
	NodePersist   = "persist"
	NodeToOrder   = "to_order"
)

// nodeOrder 是主链路节点顺序（线性图）。
var nodeOrder = []string{NodeClassify, NodeEnrich, NodeKnowledge, NodeCands, NodeChoose,
	NodeCompute, NodeExplain, NodeAssemble, NodeValidate, NodePersist, NodeToOrder}

// number 是一个由 calc 计算（或取知识库参数）的数值，带出处 trace 与所引知识条目。
type number struct {
	Field  string
	Label  string // 中文名，不含数字（交给 explain）
	KB     string // 支撑该数值的知识条目 kb_id
	Result calc.Result
}

// reason 是一条规则理由：Text 进卡片（数字来自知识库与工具），Brief 交给 explain（不含实测值）。
type reason struct {
	Text  string
	Brief string
	KB    []string // 支撑的知识条目 kb_id
	Facts []string // 支撑的实测事实名（measured 引用）
}

// decision 是规则层的结论：建议类型、作业类型、决策句（不含数字）、规则理由与候选约束。
type decision struct {
	AdviceType model.AdviceType
	WorkType   model.WorkType
	Text       string // 交给 explain 的决策句，不得含数字
	Reasons    []reason
	Evaluated  int      // 实际判定过的阈值数（降级前提：有明确阈值结论）
	Cites      []string // 结论引用的知识条目 kb_id（去重保序）
	Sprayer    calc.Sprayer

	Target       string   // 防治对象（作业类植保才取候选）
	Exclude      []string // 因抗性轮换排除的 product id
	Method       string   // 施药方式过滤（如药土法），空为不限
	MaxPHIDays   *int     // 距收获天数：只保留 PHI 不超过它的药剂
	MentionedOut bool     // 问题点名了不在候选内的药剂
	Product      string   // 非药剂作业的物料名（尿素、复合肥、磷酸二氢钾），进工单 payload
}

// state 是一次 run 在节点间传递的状态（线性图，单 goroutine 访问；enrich 内部并发但汇总后才写入）。
type state struct {
	q     agent.Question
	runID uint64
	now   time.Time
	env   *env // 本次 run 的知识快照与 calc（按 KBRef 打开）

	cls   llm.ClassifyOutput
	tools map[string]*toolOut
	facts facts
	stage string
	hits  []*knowledge.Entry

	dec        decision
	candidates []string
	selected   string
	numbers    []number
	conclusion string   // 模型原文（含 {{nK}} 占位符）
	modelWhy   []string // 模型给出的理由（只校验，不进卡片）

	refuse   string
	degraded bool
	usage    llm.Usage
	detail   string // 拒答 / 校验失败的细节（日志，不进卡片）

	card  *model.AdviceCard
	order *model.WorkOrder
}

// stopped 表示已拒答（后续节点直通到 assemble / persist）。
func (s *state) stopped() bool { return s.refuse != "" }

func (s *state) refuseWith(reason, detail string) {
	if s.refuse == "" {
		s.refuse, s.detail = reason, detail
	}
}

// fail 把规则 / calc 的业务错误归为拒答（返回 nil）；不是业务错误时原样返回（run 失败）。
func (s *state) fail(err error) error {
	if r, ok := asRefusal(err); ok {
		s.refuseWith(r, err.Error())
		return nil
	}
	return err
}

// category 是分类得到的一级类。
func (s *state) category() model.Category { return model.Category(s.cls.Category) }

// degrade：模型不可用时，有明确阈值结论与知识命中则降级为保守的观察类建议（不转工单），否则拒答。
func (s *state) degrade(task llm.Task) {
	if s.dec.Evaluated == 0 || len(s.dec.Cites) == 0 {
		s.refuseWith(RefuseLLMUnavailable, string(task)+": model unavailable")
		return
	}
	s.degraded = true
	s.dec.AdviceType = model.AdviceTypeObservation
	s.candidates, s.selected = nil, ""
}

// operation 表示当前决策为作业类。
func (s *state) operation() bool { return s.dec.AdviceType == model.AdviceTypeOperation }

// ok 表示工具已调用、返回 OK 且有数据。
func (s *state) ok(tool string) bool {
	o := s.tools[tool]
	return o != nil && o.Data != nil
}

// cite 把 kb_id 追加到结论引用（去重保序）。
func (d *decision) cite(ids ...string) {
	for _, id := range ids {
		if !slices.Contains(d.Cites, id) {
			d.Cites = append(d.Cites, id)
		}
	}
}

// because 追加一条理由并把其知识条目并入结论引用。
func (d *decision) because(r reason) {
	d.Reasons = append(d.Reasons, r)
	d.cite(r.KB...)
}
