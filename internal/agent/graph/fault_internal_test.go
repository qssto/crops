package graph

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// faultKB 是降级测试引用的真实知识条目（快照内存在）。
const faultKB = "rice/pest/planthopper"

// faultAgent 只装配降级路径用到的依赖（故障矩阵、作物、日志）；Events 为空，事件不落（外部测试另测）。
func faultAgent(t *testing.T) *Agent {
	t.Helper()
	m, err := policy.LoadFaultMatrix("../../../policy/fault_matrix.yaml", "../../../configs/texts/zh-CN.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{cfg: Config{Faults: m, Crop: "rice"}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// degradableState 是满足规则层降级条件的状态：知识命中、阈值结论带引用、作业类植保且候选非空。
func degradableState(t *testing.T) *state {
	t.Helper()
	snap, err := knowledge.NewStore(knowledge.NewRepoReader("../../..")).Open(context.Background(), knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	en, ok := snap.Entry(faultKB)
	if !ok {
		t.Fatalf("knowledge entry %s missing", faultKB)
	}
	s := &state{
		q:     agent.Question{Scope: model.Scope{TenantID: 1, FarmID: 1}, FieldID: 3},
		runID: 2, env: &env{snap: snap, crop: "rice"}, stage: "分蘖期",
		cls:  llm.ClassifyOutput{Category: string(model.CategoryProtect)},
		hits: []*knowledge.Entry{en},
		dec: decision{AdviceType: model.AdviceTypeOperation, Text: "虫口密度已达防治指标，建议尽快防治",
			Evaluated: 1, Cites: []string{faultKB}, Target: "planthopper"},
		candidates: []string{"吡蚜酮"}, selected: "吡蚜酮",
	}
	return s
}

// LLM 降级条件（P1 §6.7 技术实现第 2 条）：知识未命中 / 无规则结论（决策句空、未判定阈值、无引用）/
// 作业类植保候选空 → REFUSED(LLM_UNAVAILABLE)（应失败：条件不足仍降级出建议）；条件满足 → 降级为观察类、
// 不选药、degraded=true；观察类植保无候选也可降级。
func TestDegrade_Conditions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*state)
		refuse bool
	}{
		{name: "all conditions met", mutate: func(*state) {}},
		{name: "observation without candidates", mutate: func(s *state) {
			s.dec.AdviceType, s.candidates, s.selected = model.AdviceTypeObservation, nil, ""
		}},
		{name: "no knowledge hit", mutate: func(s *state) { s.hits = nil }, refuse: true},
		{name: "empty decision text", mutate: func(s *state) { s.dec.Text = "" }, refuse: true},
		{name: "no threshold evaluated", mutate: func(s *state) { s.dec.Evaluated = 0 }, refuse: true},
		{name: "no citation", mutate: func(s *state) { s.dec.Cites = nil }, refuse: true},
		{name: "protect operation without candidates", mutate: func(s *state) { s.candidates = nil }, refuse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, s := faultAgent(t), degradableState(t)
			tt.mutate(s)
			a.degrade(context.Background(), s, NodeChoose, llm.TaskChoose)
			if tt.refuse {
				if s.refuse != RefuseLLMUnavailable || s.degraded {
					t.Fatalf("refuse=%q degraded=%v, want LLM_UNAVAILABLE without degrade", s.refuse, s.degraded)
				}
				return
			}
			if s.stopped() || !s.degraded || s.dec.AdviceType != model.AdviceTypeObservation ||
				len(s.candidates) != 0 || s.selected != "" {
				t.Fatalf("refuse=%q degraded=%v advice=%s candidates=%v selected=%q, want observation degrade",
					s.refuse, s.degraded, s.dec.AdviceType, s.candidates, s.selected)
			}
		})
	}
}

// 降级输出不豁免输出校验（§6.7 第 2 条「validate 照常」）：规则模板填入的决策句若含裸数字仍被 validate 拦成
// VALIDATION_FAILED（应失败：降级卡跳过校验直接落库）；不含数字的对照卡通过，卡片 degraded=true、观察类、无药剂。
func TestDegrade_OutputStillValidated(t *testing.T) {
	tests := []struct {
		name     string
		decision string
		want     string
	}{
		{name: "digit-free template passes", decision: "虫口密度已达防治指标，建议尽快防治"},
		{name: "bare number blocked", decision: "虫口密度已达一千头每百丛，建议三天内防治 1000", want: RefuseValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, s := faultAgent(t), degradableState(t)
			s.dec.Text = tt.decision
			ctx := context.Background()
			a.degrade(ctx, s, NodeExplain, llm.TaskExplain)
			if s.stopped() {
				t.Fatalf("degrade refused: %s", s.detail)
			}
			s.conclusion = a.degradedConclusion(s)
			if _, err := a.assemble(ctx, s); err != nil {
				t.Fatal(err)
			}
			if !s.card.Degraded || s.card.AdviceType != model.AdviceTypeObservation || s.card.SelectedProduct != "" {
				t.Fatalf("card = %+v, want degraded observation without product", s.card)
			}
			if !strings.Contains(s.card.Conclusion, tt.decision) {
				t.Errorf("conclusion %q does not embed decision", s.card.Conclusion)
			}
			if _, err := a.validate(ctx, s); err != nil {
				t.Fatal(err)
			}
			if s.refuse != tt.want {
				t.Fatalf("refuse = %q (%s), want %q", s.refuse, s.detail, tt.want)
			}
			if tt.want != "" && s.card.Outcome != model.OutcomeRefused {
				t.Errorf("outcome = %s, want REFUSED card", s.card.Outcome)
			}
		})
	}
}
