package sample

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/knowledge"
)

const testTaxonomy = `
version: 1
categories:
  - code: protect
    name: 植保
    subcategories:
      - {code: protect.timing, name: 防治时机}
  - code: fertilize
    name: 施肥
    subcategories:
      - {code: fertilize.rate, name: 用量计算}
`

func mustTaxonomy(t *testing.T) *knowledge.Taxonomy {
	t.Helper()
	tx, err := knowledge.ParseTaxonomy([]byte(testTaxonomy))
	if err != nil {
		t.Fatalf("parse taxonomy: %v", err)
	}
	return tx
}

// validAdvice 返回一条能通过全部规则的 ADVICE 样本。
func validAdvice(id string) *Sample {
	return &Sample{
		ID: id, Version: 1, Suite: "regression",
		Tags:         []string{"L4", "protect.timing"},
		Question:     "要不要打药？",
		Context:      Context{Farm: "farm-demo-01", Field: "field-001", Role: "farm_manager", User: "u-1"},
		Fixtures:     Fixtures{Clock: "2026-07-15T09:00:00+08:00"},
		KnowledgeRef: "kb@TBD",
		Expect: Expect{
			Outcome: agent.OutcomeAdvice,
			Advice: &AdviceExpect{Category: "protect", AdviceType: "作业类", Conclusion: "建议施药",
				MustCite: []string{"kb:rice/pest/x@TBD"}},
		},
		Author: AuthorClaudeDraft, ReviewStatus: ReviewPendingExpert, SignedAt: "2026-10-28",
	}
}

func validRefusal(id string) *Sample {
	s := validAdvice(id)
	s.Expect = Expect{Outcome: agent.OutcomeRefused, RefuseReason: "KB_MISS"}
	return s
}

// validSet 是 1 条拒答 + 3 条建议，拒答占比恰好 25%。
func validSet() []*Sample {
	return []*Sample{validRefusal("s-0"), validAdvice("s-1"), validAdvice("s-2"), validAdvice("s-3")}
}

func TestValidator_DefaultRules(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func([]*Sample) []*Sample
		wantRule string // 空表示应通过
		wantID   string
	}{
		{name: "valid set", mutate: func(s []*Sample) []*Sample { return s }},
		{name: "missing author", wantRule: RuleAttribution, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Author = ""; return s }},
		{name: "missing signed_at", wantRule: RuleAttribution, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].SignedAt = ""; return s }},
		{name: "bad signed_at format", wantRule: RuleAttribution, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].SignedAt = "2026/10/28"; return s }},
		{name: "claude draft claims signed", wantRule: RuleAttribution, wantID: "s-2",
			mutate: func(s []*Sample) []*Sample { s[2].ReviewStatus = ReviewSigned; return s }},
		{name: "missing question", wantRule: RuleRequired, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Question = " "; return s }},
		{name: "missing clock", wantRule: RuleRequired, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Fixtures.Clock = ""; return s }},
		{name: "clock not rfc3339", wantRule: RuleRequired, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Fixtures.Clock = "2026-07-15 09:00"; return s }},
		{name: "missing knowledge_ref", wantRule: RuleRequired, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].KnowledgeRef = ""; return s }},
		{name: "id not kebab", wantRule: RuleRequired, wantID: "S_1",
			mutate: func(s []*Sample) []*Sample { s[1].ID = "S_1"; return s }},
		{name: "unknown role", wantRule: RuleEnums, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Context.Role = "boss"; return s }},
		{name: "unknown outcome", wantRule: RuleEnums, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Expect.Outcome = "advice"; return s }},
		{name: "unknown tool error", wantRule: RuleEnums, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample {
				s[1].Fixtures.MCP = map[string][]ToolFixture{"weather.forecast": {{Error: "boom"}}}
				return s
			}},
		{name: "refusal without reason", wantRule: RuleExpect, wantID: "s-0",
			mutate: func(s []*Sample) []*Sample { s[0].Expect.RefuseReason = ""; return s }},
		{name: "refusal with advice", wantRule: RuleExpect, wantID: "s-0",
			mutate: func(s []*Sample) []*Sample { s[0].Expect.Advice = validAdvice("x").Expect.Advice; return s }},
		{name: "advice without citation", wantRule: RuleExpect, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Expect.Advice.MustCite = nil; return s }},
		{name: "stop_at_approval without work_order", wantRule: RuleExpect, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Expect.Trajectory.StopAtApproval = true; return s }},
		{name: "number without precision", wantRule: RuleExpect, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample {
				s[1].Expect.Advice.Numbers = []NumberExpect{{Field: "dose", Unit: "mL/亩"}}
				return s
			}},
		{name: "unknown tag", wantRule: RuleTags, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Tags = append(s[1].Tags, "稻飞虱"); return s }},
		{name: "layer out of range", wantRule: RuleTags, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Tags = []string{"L9", "protect.timing"}; return s }},
		{name: "no layer tag", wantRule: RuleTags, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Tags = []string{"protect.timing"}; return s }},
		{name: "no taxonomy tag", wantRule: RuleTags, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Tags = []string{"L4"}; return s }},
		{name: "category mismatches tags", wantRule: RuleTags, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[1].Expect.Advice.Category = "fertilize"; return s }},
		{name: "duplicate id", wantRule: RuleUniqueID, wantID: "s-1",
			mutate: func(s []*Sample) []*Sample { s[2].ID = "s-1"; return s }},
		{name: "refuse ratio below 20%", wantRule: RuleRefuseRatio, wantID: "suite:regression",
			mutate: func(s []*Sample) []*Sample { return append(s, validAdvice("s-4"), validAdvice("s-5")) }},
	}
	tx := mustTaxonomy(t)
	v := NewValidator(DefaultRules(tx)...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := v.Validate(tt.mutate(validSet()))
			if tt.wantRule == "" {
				if !report.OK() {
					t.Fatalf("want ok, got %v", report.Violations)
				}
				return
			}
			found := false
			for _, viol := range report.Violations {
				if viol.Rule == tt.wantRule && viol.Subject() == tt.wantID {
					found = true
				}
			}
			if !found {
				t.Fatalf("want violation rule=%s id=%s, got %v", tt.wantRule, tt.wantID, report.Violations)
			}
		})
	}
}

// 后续层可在默认规则后追加自定义规则。
func TestValidator_ExtraRule(t *testing.T) {
	extra := PerSample("must_cite_exists", func(s *Sample) []string {
		if s.Expect.Advice != nil {
			return []string{"kb not found"}
		}
		return nil
	})
	v := NewValidator(append(DefaultRules(mustTaxonomy(t)), extra)...)
	report := v.Validate(validSet())
	if got, want := strings.Join(report.FailedIDs(), ","), "s-1,s-2,s-3"; got != want {
		t.Fatalf("FailedIDs = %s, want %s", got, want)
	}
}

const sampleTemplate = `id: %ID%
version: 1
suite: regression
tags: [L4, protect.timing]
question: 要不要打药？
context: {farm: farm-demo-01, field: field-001, role: farm_manager, user: u-1}
fixtures: {clock: "2026-07-15T09:00:00+08:00"}
knowledge_ref: kb@TBD
expect: {outcome: REFUSED, refuse_reason: KB_MISS}
%AUTHOR%review_status: pending_expert
signed_at: 2026-10-28
`

func writeSample(t *testing.T, dir, id string, withAuthor bool) {
	t.Helper()
	author := ""
	if withAuthor {
		author = "author: claude-draft\n"
	}
	body := strings.NewReplacer("%ID%", id, "%AUTHOR%", author).Replace(sampleTemplate)
	if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}
}

func TestLoadAndValidate(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]bool // id → 是否带 author
		raw     map[string]string
		wantErr string // 空表示应通过；否则错误信息需包含它
	}{
		{name: "ok", files: map[string]bool{"tmp-0001": true, "tmp-0002": true}},
		{name: "missing author is rejected with id", files: map[string]bool{"tmp-0001": true, "tmp-no-author": false},
			wantErr: "tmp-no-author"},
		{name: "unknown field is a parse error", files: map[string]bool{"tmp-0001": true},
			raw: map[string]string{"typo.yaml": "id: tmp-typo\nauthr: x\n"}, wantErr: "typo.yaml"},
		{name: "empty dir", wantErr: "no sample found"},
	}
	v := NewValidator(DefaultRules(mustTaxonomy(t))...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "regression")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for id, withAuthor := range tt.files {
				writeSample(t, dir, id, withAuthor)
			}
			for name, body := range tt.raw {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			samples, err := LoadAndValidate(filepath.Dir(dir), v)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				if len(samples) != len(tt.files) {
					t.Fatalf("loaded %d samples, want %d", len(samples), len(tt.files))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// 缺 author 的样本：错误类型为 *ValidationError，FailedIDs 精确列出该样本。
func TestLoadAndValidate_MissingAuthorListsID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "regression")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSample(t, dir, "tmp-ok", true)
	writeSample(t, dir, "tmp-no-author", false)
	_, err := LoadAndValidate(filepath.Dir(dir), NewValidator(DefaultRules(mustTaxonomy(t))...))
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("want *ValidationError, got %v", err)
	}
	if got := strings.Join(verr.Report.FailedIDs(), ","); got != "tmp-no-author" {
		t.Fatalf("FailedIDs = %s, want tmp-no-author", got)
	}
}

// 仓库内的 20 条样本必须通过静态校验。
func TestRepoSamplesValid(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "eval")
	tx, err := knowledge.LoadTaxonomy(filepath.Join(root, "taxonomy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	samples, err := LoadAndValidate(filepath.Join(root, "suites"), NewValidator(DefaultRules(tx)...))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) < 20 {
		t.Fatalf("got %d samples, want >= 20", len(samples))
	}
}
