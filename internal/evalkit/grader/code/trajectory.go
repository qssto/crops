package code

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"agri-steward/internal/evalkit/grader"
)

// toolWildcard 是 must_not_call 的通配后缀：device.* 匹配 device 下全部工具。
const toolWildcard = ".*"

// errNoToolCalls 表示样本有轨迹断言但 trial 未记录工具调用（工具接缝不是 FixtureMCP），无法判定。
var errNoToolCalls = errors.New("tool calls not recorded")

// MustCall 是轨迹打分器 must_call（非红线）：must_call 为 superset 语义（每个工具至少调用一次，允许多调），
// must_not_call 中的工具（支持 server.* 通配）一次都不得调用。
type MustCall struct{}

var _ grader.Grader = MustCall{}

// Name 返回打分器名。
func (MustCall) Name() string { return "must_call" }

// RedLine 返回 false。
func (MustCall) RedLine() bool { return false }

// Grade 打分；Value 为满足的断言比例。样本无断言时通过；有断言但未记录调用返回 grader_error。
func (MustCall) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if t == nil || t.Sample == nil {
		return grader.Score{}, errors.New("must_call: trial or sample is nil")
	}
	tr := t.Sample.Expect.Trajectory
	total := len(tr.MustCall) + len(tr.MustNotCall)
	if total == 0 {
		return grader.Score{Pass: true, Value: 1}, nil
	}
	if t.ToolCalls == nil {
		return grader.Score{}, fmt.Errorf("must_call: %s: %w", t.Sample.ID, errNoToolCalls)
	}
	var fails []string
	for _, tool := range tr.MustCall {
		if t.ToolCalls[tool] == 0 {
			fails = append(fails, tool+" not called")
		}
	}
	for _, pattern := range tr.MustNotCall {
		if hit := calledMatching(t.ToolCalls, pattern); len(hit) > 0 {
			fails = append(fails, fmt.Sprintf("must_not_call %s but called %s", pattern, strings.Join(hit, ", ")))
		}
	}
	value := float64(total-len(fails)) / float64(total)
	return grader.Score{Pass: len(fails) == 0, Value: value, Evidence: strings.Join(fails, "; ")}, nil
}

// calledMatching 返回调用次数 > 0 且匹配 pattern（精确或 server.* 前缀）的工具，按名排序。
func calledMatching(calls map[string]int, pattern string) []string {
	prefix, wildcard := strings.CutSuffix(pattern, toolWildcard)
	var out []string
	for tool, n := range calls {
		if n == 0 {
			continue
		}
		if tool == pattern || (wildcard && strings.HasPrefix(tool, prefix+".")) {
			out = append(out, tool)
		}
	}
	sort.Strings(out)
	return out
}

// Budget 是轨迹打分器 budget（非红线）：工具调用总次数 ≤ budget.max_steps。
// P0 L4 无 token 计量，max_tokens 不判定，写入 Evidence（TODO(L5)：接 llm token 计量）。
type Budget struct{}

var _ grader.Grader = Budget{}

// Name 返回打分器名。
func (Budget) Name() string { return "budget" }

// RedLine 返回 false。
func (Budget) RedLine() bool { return false }

// Grade 打分；Value 为 1 − 超出比例（不低于 0）。
func (Budget) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if t == nil || t.Sample == nil {
		return grader.Score{}, errors.New("budget: trial or sample is nil")
	}
	b := t.Sample.Expect.Trajectory.Budget
	if b == nil || (b.MaxSteps == 0 && b.MaxTokens == 0) {
		return grader.Score{Pass: true, Value: 1}, nil
	}
	var notes []string
	if b.MaxTokens > 0 {
		notes = append(notes, "max_tokens not measured")
	}
	if b.MaxSteps == 0 {
		return grader.Score{Pass: true, Value: 1, Evidence: strings.Join(notes, "; ")}, nil
	}
	if t.ToolCalls == nil {
		return grader.Score{}, fmt.Errorf("budget: %s: %w", t.Sample.ID, errNoToolCalls)
	}
	steps := 0
	for _, n := range t.ToolCalls {
		steps += n
	}
	if steps <= b.MaxSteps {
		return grader.Score{Pass: true, Value: 1, Evidence: strings.Join(notes, "; ")}, nil
	}
	over := float64(steps-b.MaxSteps) / float64(b.MaxSteps)
	notes = append([]string{fmt.Sprintf("steps %d > max_steps %d", steps, b.MaxSteps)}, notes...)
	return grader.Score{Value: max(0, 1-over), Evidence: strings.Join(notes, "; ")}, nil
}

// All 返回全部代码打分器（红线在前）：schema_valid、citation_valid、numbers_match_calc、
// product_in_candidates、outcome_match、must_call、budget。
func All(schema *SchemaValid) []grader.Grader {
	return []grader.Grader{schema, CitationValid{}, NumbersMatchCalc{}, ProductInCandidates{},
		OutcomeMatch{}, MustCall{}, Budget{}}
}
