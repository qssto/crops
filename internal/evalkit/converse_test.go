package evalkit

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/usersim"
	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// convAgent 对已建好的 run（q.RunID 非 0）沿用其 run_id，否则给 100；结论为「答:{问题}」；问题等于 ticketText 时开票
// （WAIT_APPROVAL）。只看本次提问，n 个 trial 共用也互不影响。
type convAgent struct {
	texts      []string
	runIDs     []uint64
	ticketText string
}

func (a *convAgent) Ask(_ context.Context, q agent.Question) (agent.Answer, error) {
	a.texts = append(a.texts, q.Text)
	a.runIDs = append(a.runIDs, q.RunID)
	ans := agent.Answer{Outcome: model.OutcomeAdvice, RunID: max(q.RunID, 100),
		Card: &model.AdviceCard{Outcome: model.OutcomeAdvice, Conclusion: "答:" + q.Text}}
	if q.Text == a.ticketText {
		ans.Ticket = &model.ApprovalTicket{TicketID: 9}
	}
	return ans, nil
}

// scriptChannel 第 k 轮（k = len(history)）给 lines[k-1]，用完即结束；err 非空时返回错误。
type scriptChannel struct {
	lines   []string
	err     error
	calls   int
	history [][]usersim.Turn
}

func (c *scriptChannel) Next(_ context.Context, _ *sample.Sample, h []usersim.Turn) (string, bool, error) {
	c.calls++
	c.history = append(c.history, append([]usersim.Turn(nil), h...))
	if c.err != nil {
		return "", false, c.err
	}
	if len(h) > len(c.lines) {
		return "", false, nil
	}
	return c.lines[len(h)-1], true, nil
}

// linkRecorder 记录续接建 run 的入参，返回 500 + 上一轮 run_id 的个位以外的确定值：prev 101 → 501，501 → 502。
type linkRecorder struct{ inputs []service.AskInput }

func (l *linkRecorder) link(_ context.Context, _ model.Scope, in service.AskInput) (uint64, error) {
	l.inputs = append(l.inputs, in)
	if in.PrevRunID < 500 {
		return 501, nil
	}
	return in.PrevRunID + 1, nil
}

func simSample(id string, maxTurns int) *sample.Sample {
	s := mkSample(id, "capability", sample.TagUserSim)
	s.UserSim = &sample.UserSim{Persona: sample.PersonaFarmer, Instruction: "问到风速为止。", MaxTurns: maxTurns}
	return s
}

func simRunner(t *testing.T, a agent.Agent, ch usersim.Channel, l *linkRecorder) *Runner {
	t.Helper()
	opts := []RunnerOption{}
	if ch != nil {
		opts = append(opts, WithUserChannel(ch))
	}
	r, err := NewRunner(StaticEnv(&app.App{Agent: a}), nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if l != nil {
		r.link = func(ctx context.Context, _ *app.App, scope model.Scope, in service.AskInput) (uint64, error) {
			return l.link(ctx, scope, in)
		}
	}
	return r
}

// user_sim 样本：第一句是 question，之后按通道续问；第 2 轮起按生产路径带 prev_run_id（上一轮 run）建 run；
// 打分用最后一轮的回答；对话（含渲染后的 agent 回答与各轮 run_id）记入 Trial.Conversation。
func TestRunner_UserSim_Converse(t *testing.T) {
	ag := &convAgent{}
	ch := &scriptChannel{lines: []string{"风速多少？", "那几点打？"}}
	l := &linkRecorder{}
	exp, err := simRunner(t, ag, ch, l).Run(t.Context(), []*sample.Sample{simSample("sim", 5)}, Options{N: MinUserSimTrials})
	if err != nil {
		t.Fatal(err)
	}
	tr := exp.Trials[0]
	if tr.Error != "" {
		t.Fatal(tr.Error)
	}
	if got := strings.Join(ag.texts[:3], "|"); got != "q-sim|风速多少？|那几点打？" {
		t.Fatalf("asked %q", got)
	}
	if ag.runIDs[0] != 0 || ag.runIDs[1] != 501 || ag.runIDs[2] != 502 {
		t.Fatalf("question run ids = %v", ag.runIDs[:3])
	}
	if l.inputs[0].PrevRunID != 100 || l.inputs[1].PrevRunID != 501 || l.inputs[0].Text != "风速多少？" {
		t.Fatalf("link inputs = %+v", l.inputs[:2])
	}
	if tr.RunID != 502 || tr.Card.Conclusion != "答:那几点打？" {
		t.Fatalf("graded answer run=%d card=%+v, want the last turn", tr.RunID, tr.Card)
	}
	want := []ConvTurn{{User: "q-sim", Agent: "答:q-sim", RunID: 100}, {User: "风速多少？", Agent: "答:风速多少？", RunID: 501},
		{User: "那几点打？", Agent: "答:那几点打？", RunID: 502}}
	if len(tr.Conversation) != len(want) {
		t.Fatalf("conversation = %+v", tr.Conversation)
	}
	for i, w := range want {
		if tr.Conversation[i] != w {
			t.Errorf("turn %d = %+v, want %+v", i, tr.Conversation[i], w)
		}
	}
	// 每个 trial 问通道 3 次（第 3 次结束）；通道看到的历史是 (用户, 渲染后的回答)，不含 run_id。
	if ch.calls != 3*MinUserSimTrials || len(ch.history[0]) != 1 || ch.history[0][0] != (usersim.Turn{User: "q-sim", Agent: "答:q-sim"}) {
		t.Fatalf("channel calls=%d history=%+v", ch.calls, ch.history[0])
	}
}

// agent 进入 WAIT_APPROVAL（有票）即结束对话，不再问通道。
func TestRunner_UserSim_StopsAtApproval(t *testing.T) {
	ag := &convAgent{ticketText: "打吧"}
	ch := &scriptChannel{lines: []string{"打吧", "还有吗"}}
	exp, err := simRunner(t, ag, ch, &linkRecorder{}).Run(t.Context(), []*sample.Sample{simSample("sim", 5)}, Options{N: MinUserSimTrials})
	if err != nil {
		t.Fatal(err)
	}
	tr := exp.Trials[0]
	if tr.Error != "" || len(tr.Conversation) != 2 || ch.calls != MinUserSimTrials {
		t.Fatalf("err=%q conversation=%d channel calls=%d", tr.Error, len(tr.Conversation), ch.calls)
	}
	if !strings.HasPrefix(tr.Conversation[1].Agent, replyWaitApproval) {
		t.Fatalf("approval reply = %q", tr.Conversation[1].Agent)
	}
}

// 通道出错（如缺录制）与未配置通道都记 trial 错误（infra_error），不编造回复；非 user_sim 样本从不问通道（与单轮回放一致）。
func TestRunner_UserSim_Errors(t *testing.T) {
	noRec := fmt.Errorf("%w for (sim, turn 1, x)", usersim.ErrNoRecording)
	exp, err := simRunner(t, &convAgent{}, &scriptChannel{err: noRec}, &linkRecorder{}).Run(t.Context(),
		[]*sample.Sample{simSample("sim", 5)}, Options{N: MinUserSimTrials})
	if err != nil {
		t.Fatal(err)
	}
	if tr := exp.Trials[0]; !strings.Contains(tr.Error, "no recording") || tr.Status() != StatusInfraError {
		t.Fatalf("missing recording trial = %q status %s", tr.Error, tr.Status())
	}

	exp, err = simRunner(t, &convAgent{}, nil, nil).Run(t.Context(), []*sample.Sample{simSample("sim", 5)}, Options{N: MinUserSimTrials})
	if err != nil {
		t.Fatal(err)
	}
	if tr := exp.Trials[0]; !strings.Contains(tr.Error, errNoUserChannel.Error()) || tr.Status() != StatusInfraError {
		t.Fatalf("no channel trial error = %q", tr.Error)
	}

	ch := &scriptChannel{lines: []string{"不该问"}}
	ag := &convAgent{}
	exp, err = simRunner(t, ag, ch, &linkRecorder{}).Run(t.Context(), []*sample.Sample{mkSample("plain", "regression")}, Options{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ch.calls != 0 || len(ag.texts) != 1 || exp.Trials[0].Conversation != nil {
		t.Fatalf("plain sample: channel calls=%d asks=%d conversation=%v", ch.calls, len(ag.texts), exp.Trials[0].Conversation)
	}
}

// 评审 P1：多轮对话的 token 按全部轮次累加（之前只算末轮 Detail）：预算与 tokens_per_session（snapshot.TokenUsed
// = reduce Metrics.Tokens）都用累加值。runTokens 桩每轮 run 给 100 token；3 轮对话中前 2 轮记 PriorTurnTokens，
// 末轮仍来自 Detail（构造 50）。
func TestRunner_UserSim_TokensAllTurns(t *testing.T) {
	r := simRunner(t, &convAgent{}, &scriptChannel{lines: []string{"风速多少？", "那几点打？"}}, &linkRecorder{})
	var read []uint64
	r.runTokens = func(_ context.Context, _ *app.App, _ model.Scope, runID uint64) (int, error) {
		read = append(read, runID)
		return 100, nil
	}
	exp, err := r.Run(t.Context(), []*sample.Sample{simSample("sim", 5)}, Options{N: MinUserSimTrials, TokenBudget: 150})
	if err != nil {
		t.Fatal(err)
	}
	tr := exp.Trials[0]
	if tr.Error != "" || tr.PriorTurnTokens != 200 || read[0] != 100 || read[1] != 501 {
		t.Fatalf("err=%q prior tokens=%d read runs=%v", tr.Error, tr.PriorTurnTokens, read)
	}
	// 单个 trial 已 200 > 150：停止派发（只看末轮时为 0，不会超预算）。
	if !exp.BudgetExceeded || len(exp.Trials) != 1 {
		t.Fatalf("budget: exceeded=%v trials=%d", exp.BudgetExceeded, len(exp.Trials))
	}
	tr.Detail = &service.RunDetail{Steps: []service.StepDetail{{LLMCalls: []service.LLMCallDetail{
		{LLMCallPayload: model.LLMCallPayload{PromptTokens: 30, CompletionTokens: 20}}}}}}
	if got := TrialTokens(&tr); got != 250 {
		t.Fatalf("TrialTokens = %d, want 250", got)
	}
	// 读前几轮 run 失败记 trial 错误（infra_error），不按 0 少算。
	r.runTokens = func(context.Context, *app.App, model.Scope, uint64) (int, error) { return 0, fmt.Errorf("db down") }
	exp, err = r.Run(t.Context(), []*sample.Sample{simSample("sim", 5)}, Options{N: MinUserSimTrials})
	if err != nil {
		t.Fatal(err)
	}
	if tr := exp.Trials[0]; !strings.Contains(tr.Error, "db down") || tr.Status() != StatusInfraError {
		t.Fatalf("token read failure trial = %q", tr.Error)
	}
}

// 未隔离的 user_sim 样本须 n ≥ MinUserSimTrials（§9.2）；隔离的不运行，不受限。
func TestRunner_UserSim_MinTrials(t *testing.T) {
	r := simRunner(t, &convAgent{}, &scriptChannel{}, &linkRecorder{})
	if _, err := r.Run(t.Context(), []*sample.Sample{simSample("sim", 3)}, Options{N: MinUserSimTrials - 1}); err == nil ||
		!strings.Contains(err.Error(), "sim") {
		t.Fatalf("n < %d accepted: %v", MinUserSimTrials, err)
	}
	q := simSample("sim", 3)
	reason := "待人工阅读"
	q.Quarantine = &reason
	if _, err := r.Run(t.Context(), []*sample.Sample{q, mkSample("plain", "regression")}, Options{N: 1}); err != nil {
		t.Fatalf("quarantined user_sim sample blocked the run: %v", err)
	}
}

// agent 回答按结构化结果渲染给模拟用户：建议给结论、拒答给原因、开票给待审批。
func TestRenderReply(t *testing.T) {
	cases := []struct {
		ans  agent.Answer
		want string
	}{
		{agent.Answer{Outcome: model.OutcomeAdvice, Card: &model.AdviceCard{Conclusion: "建议观察"}}, "建议观察"},
		{agent.Answer{Outcome: model.OutcomeRefused, RefuseReason: "DATA_MISSING"}, replyRefused + "DATA_MISSING"},
		{agent.Answer{Outcome: model.OutcomeAdvice, Card: &model.AdviceCard{Conclusion: "打药"}, Ticket: &model.ApprovalTicket{}}, replyWaitApproval + "打药"},
		{agent.Answer{Outcome: model.OutcomeAdvice}, ""},
	}
	for _, c := range cases {
		if got := renderReply(c.ans); got != c.want {
			t.Errorf("renderReply(%+v) = %q, want %q", c.ans, got, c.want)
		}
	}
}
