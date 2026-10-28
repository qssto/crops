package sample

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/model"
)

// 规则名，出现在违规输出中。
const (
	RuleRequired    = "required"
	RuleAttribution = "attribution"
	RuleEnums       = "enums"
	RuleExpect      = "expect_shape"
	RuleTags        = "tags"
	RuleUniqueID    = "unique_id"
	RuleRefuseRatio = "refuse_ratio"
)

// MinRefuseRatio 是每个套件中应拒答样本的最低占比（P0 §6.1）。
const MinRefuseRatio = 0.2

// AuthorClaudeDraft 是 Claude 草拟样本的署名，不得冒充专家；此类样本只能是 pending_expert。
const AuthorClaudeDraft = "claude-draft"

// 审核状态。
const (
	ReviewPendingExpert = "pending_expert"
	ReviewSigned        = "signed"
)

const signedAtLayout = "2006-01-02"

var (
	idPattern    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	layerPattern = regexp.MustCompile(`^L[0-8]$`) // P0 路线 L0–L8

	knownSuites       = set("regression", "capability", "redteam", "holdout")
	knownRoles        = set("farm_manager", "agronomist", "operator")
	knownReviews      = set(ReviewPendingExpert, ReviewSigned)
	knownOutcomes     = set(string(model.OutcomeAdvice), string(model.OutcomeRefused))
	knownRefuseReason = set("KB_MISS", "DATA_MISSING", "OUT_OF_SCOPE")
	knownAdviceTypes  = set("作业类", "观察类") // 母文档 §5.0.5 AdviceCard.advice_type
	knownApprovers    = set("approve", "reject", "timeout")
	knownToolErrors   = set("timeout", "empty", "malformed")
	knownPIIKinds     = set("phone", "id_card", "coordinate")
	// WorkOrder 状态（P0 §2.3-2），L1 冻结后改为引用 internal/model 常量
	knownOrderStatus = set("DRAFT", "PENDING_APPROVAL", "APPROVED", "DISPATCHED", "EXECUTING", "DONE",
		"REJECTED", "EXPIRED", "FAILED", "CANCELED", "PARTIALLY_DONE")
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// DefaultRules 返回静态校验规则：L0 七条 + L3 两条（must_cite 在 knowledge_ref 版本中存在、
// product_in ⊆ calc.Candidates）。kb 按 knowledge_ref 打开知识版本；后续层在其后追加规则即可（开闭原则）。
func DefaultRules(tx *knowledge.Taxonomy, kb KnowledgeOpener) []Rule {
	return []Rule{
		PerSample(RuleRequired, checkRequired),
		PerSample(RuleAttribution, checkAttribution),
		PerSample(RuleEnums, checkEnums),
		PerSample(RuleExpect, checkExpectShape),
		PerSample(RuleTags, tagsCheck(tx)),
		uniqueIDRule{},
		refuseRatioRule{min: MinRefuseRatio},
		PerSample(RuleMustCiteKB, mustCiteKBCheck(kb)),
		PerSample(RuleProductInCandidates, productInCheck(tx, kb)),
	}
}

func checkRequired(s *Sample) []string {
	var msgs []string
	need := func(ok bool, field string) {
		if !ok {
			msgs = append(msgs, field+" is required")
		}
	}
	need(s.ID != "", "id")
	need(s.Version >= 1, "version (>= 1)")
	need(s.Suite != "", "suite")
	need(strings.TrimSpace(s.Question) != "", "question")
	need(s.Context.Farm != "", "context.farm")
	need(s.Context.Field != "", "context.field")
	need(s.Context.Role != "", "context.role")
	need(s.Context.User != "", "context.user")
	need(s.Fixtures.Clock != "", "fixtures.clock")
	need(s.KnowledgeRef != "", "knowledge_ref")
	need(s.Expect.Outcome != "", "expect.outcome")
	need(len(s.Tags) > 0, "tags")
	if s.ID != "" && !idPattern.MatchString(s.ID) {
		msgs = append(msgs, fmt.Sprintf("id %q must be lowercase kebab-case", s.ID))
	}
	if s.Fixtures.Clock != "" {
		if _, err := time.Parse(time.RFC3339, s.Fixtures.Clock); err != nil {
			msgs = append(msgs, fmt.Sprintf("fixtures.clock %q is not RFC3339", s.Fixtures.Clock))
		}
	}
	return msgs
}

// checkAttribution 保证每条样本可追责：署名、签署日期、审核状态齐全且不冒充专家。
func checkAttribution(s *Sample) []string {
	var msgs []string
	if strings.TrimSpace(s.Author) == "" {
		msgs = append(msgs, "author is required")
	}
	if s.SignedAt == "" {
		msgs = append(msgs, "signed_at is required")
	} else if _, err := time.Parse(signedAtLayout, s.SignedAt); err != nil {
		msgs = append(msgs, fmt.Sprintf("signed_at %q must be YYYY-MM-DD", s.SignedAt))
	}
	switch {
	case s.ReviewStatus == "":
		msgs = append(msgs, "review_status is required")
	case !knownReviews[s.ReviewStatus]:
		msgs = append(msgs, fmt.Sprintf("review_status %q not in [pending_expert signed]", s.ReviewStatus))
	case s.Author == AuthorClaudeDraft && s.ReviewStatus != ReviewPendingExpert:
		msgs = append(msgs, "author claude-draft must stay review_status=pending_expert until an expert signs")
	}
	return msgs
}

func checkEnums(s *Sample) []string {
	var msgs []string
	enum := func(value string, allowed map[string]bool, field string) {
		if value != "" && !allowed[value] {
			msgs = append(msgs, fmt.Sprintf("%s %q is not allowed", field, value))
		}
	}
	enum(s.Suite, knownSuites, "suite")
	enum(s.Context.Role, knownRoles, "context.role")
	enum(string(s.Expect.Outcome), knownOutcomes, "expect.outcome")
	enum(s.Expect.RefuseReason, knownRefuseReason, "expect.refuse_reason")
	enum(s.Fixtures.Approver, knownApprovers, "fixtures.approver")
	if s.Expect.Advice != nil {
		enum(s.Expect.Advice.AdviceType, knownAdviceTypes, "expect.advice.advice_type")
	}
	if s.Expect.WorkOrder != nil {
		enum(s.Expect.WorkOrder.FinalStatus, knownOrderStatus, "expect.work_order.final_status")
	}
	for i, p := range s.Expect.PII {
		enum(p.Kind, knownPIIKinds, fmt.Sprintf("expect.pii[%d].kind", i))
	}
	tools := make([]string, 0, len(s.Fixtures.MCP))
	for tool := range s.Fixtures.MCP {
		tools = append(tools, tool)
	}
	sort.Strings(tools) // 输出顺序稳定
	for _, tool := range tools {
		for i, f := range s.Fixtures.MCP[tool] {
			enum(f.Error, knownToolErrors, fmt.Sprintf("fixtures.mcp.%s[%d].error", tool, i))
		}
	}
	// 套件名须与所在目录一致：eval/suites/{suite}/xxx.yaml
	if s.Path != "" && s.Suite != "" {
		if dir := filepath.Base(filepath.Dir(s.Path)); dir != s.Suite {
			msgs = append(msgs, fmt.Sprintf("suite %q does not match directory %q", s.Suite, dir))
		}
	}
	return msgs
}

// checkExpectShape 检查期望结构与 outcome 一致。
func checkExpectShape(s *Sample) []string {
	e := s.Expect
	var msgs []string
	switch e.Outcome {
	case model.OutcomeRefused:
		if e.RefuseReason == "" {
			msgs = append(msgs, "expect.refuse_reason is required when outcome=REFUSED")
		}
		if e.Advice != nil || e.WorkOrder != nil {
			msgs = append(msgs, "expect.advice / expect.work_order must be empty when outcome=REFUSED")
		}
		if e.Trajectory.StopAtApproval {
			msgs = append(msgs, "expect.trajectory.stop_at_approval requires outcome=ADVICE")
		}
	case model.OutcomeAdvice:
		if e.RefuseReason != "" {
			msgs = append(msgs, "expect.refuse_reason must be empty when outcome=ADVICE")
		}
		msgs = append(msgs, checkAdviceShape(e.Advice)...)
		if e.Trajectory.StopAtApproval && e.WorkOrder == nil {
			msgs = append(msgs, "expect.work_order is required when stop_at_approval=true")
		}
	}
	return append(msgs, checkPIIShape(s)...)
}

// checkPIIShape：每条 pii 须有 kind 与 value，且 value 原样出现在 question 中（否则断言无从谈起）。
func checkPIIShape(s *Sample) []string {
	var msgs []string
	for i, p := range s.Expect.PII {
		switch {
		case p.Kind == "" || strings.TrimSpace(p.Value) == "":
			msgs = append(msgs, fmt.Sprintf("expect.pii[%d] needs kind and value", i))
		case !strings.Contains(s.Question, p.Value):
			msgs = append(msgs, fmt.Sprintf("expect.pii[%d].value %q does not appear in question", i, p.Value))
		}
	}
	return msgs
}

func checkAdviceShape(a *AdviceExpect) []string {
	if a == nil {
		return []string{"expect.advice is required when outcome=ADVICE"}
	}
	var msgs []string
	if a.Category == "" {
		msgs = append(msgs, "expect.advice.category is required")
	}
	if a.AdviceType == "" {
		msgs = append(msgs, "expect.advice.advice_type is required")
	}
	if strings.TrimSpace(a.Conclusion) == "" {
		msgs = append(msgs, "expect.advice.conclusion is required")
	}
	if len(a.MustCite) == 0 {
		msgs = append(msgs, "expect.advice.must_cite must not be empty (no advice without citation)")
	}
	for i, n := range a.Numbers {
		if n.Field == "" || n.Unit == "" || n.Precision <= 0 {
			msgs = append(msgs, fmt.Sprintf("expect.advice.numbers[%d] needs field, unit and precision > 0", i))
		}
	}
	return msgs
}

// tagsCheck：每个 tag 必须是 Ln 编号或 taxonomy code；至少各一个；advice.category 须与 taxonomy tag 一致。
func tagsCheck(tx *knowledge.Taxonomy) SampleCheck {
	return func(s *Sample) []string {
		var msgs []string
		var hasLayer bool
		categories := make(map[string]bool)
		seen := make(map[string]bool, len(s.Tags))
		for _, tag := range s.Tags {
			if seen[tag] {
				msgs = append(msgs, fmt.Sprintf("duplicate tag %q", tag))
				continue
			}
			seen[tag] = true
			if layerPattern.MatchString(tag) {
				hasLayer = true
				continue
			}
			cat, ok := tx.CategoryOf(tag)
			if !ok {
				msgs = append(msgs, fmt.Sprintf("tag %q is neither L0-L8 nor a taxonomy code", tag))
				continue
			}
			categories[cat] = true
		}
		if len(s.Tags) > 0 && !hasLayer {
			msgs = append(msgs, "tags must include at least one layer tag (L0-L8)")
		}
		if len(s.Tags) > 0 && len(categories) == 0 {
			msgs = append(msgs, "tags must include at least one taxonomy code")
		}
		if a := s.Expect.Advice; a != nil && a.Category != "" && !categories[a.Category] {
			msgs = append(msgs, fmt.Sprintf("expect.advice.category %q does not match taxonomy tags", a.Category))
		}
		return msgs
	}
}

// uniqueIDRule：样本 ID 全局唯一。
type uniqueIDRule struct{}

func (uniqueIDRule) Name() string { return RuleUniqueID }

func (uniqueIDRule) Check(samples []*Sample) []Violation {
	firstPath := make(map[string]string, len(samples))
	var out []Violation
	for _, s := range samples {
		if s.ID == "" {
			continue
		}
		if prev, dup := firstPath[s.ID]; dup {
			out = append(out, Violation{SampleID: s.ID, Path: s.Path, Rule: RuleUniqueID,
				Msg: fmt.Sprintf("duplicate id, first defined in %s", prev)})
			continue
		}
		firstPath[s.ID] = s.Path
	}
	return out
}

// refuseRatioRule：每个套件应拒答样本占比 ≥ min。
type refuseRatioRule struct {
	min float64
}

func (refuseRatioRule) Name() string { return RuleRefuseRatio }

func (r refuseRatioRule) Check(samples []*Sample) []Violation {
	type count struct{ total, refused int }
	bySuite := make(map[string]*count)
	var order []string
	for _, s := range samples {
		c, ok := bySuite[s.Suite]
		if !ok {
			c = &count{}
			bySuite[s.Suite] = c
			order = append(order, s.Suite)
		}
		c.total++
		if s.IsRefusal() {
			c.refused++
		}
	}
	var out []Violation
	for _, suite := range order {
		c := bySuite[suite]
		if ratio := float64(c.refused) / float64(c.total); ratio < r.min {
			out = append(out, Violation{SampleID: "suite:" + suite, Rule: RuleRefuseRatio,
				Msg: fmt.Sprintf("refused %d/%d = %.0f%%, need >= %.0f%%", c.refused, c.total, ratio*100, r.min*100)})
		}
	}
	return out
}
