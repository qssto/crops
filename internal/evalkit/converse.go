package evalkit

import (
	"context"
	"errors"
	"fmt"

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/usersim"
	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// P2 L7-b 多轮模拟用户（评测设计方案 §9.2）：声明 user_sim 的样本以 question 为第一句，每轮回答后问用户通道
// （WithUserChannel，cmd/eval 注入录制回放的 usersim.Simulated）要下一句；第 2 轮起按生产路径
// （AskService.CreateRun 带 prev_run_id = 上一轮 run）建 run 再交给被测 agent。通道结束、达到 max_turns
// 或 agent 进入 WAIT_APPROVAL（有票）即停；打分与各项观测用最后一轮。通道出错（如缺录制）记 infra_error，不编造回复。

// MinUserSimTrials 是 user_sim 样本的最少运行次数（§9.2：模拟用户有随机性，n ≥ 5）。
const MinUserSimTrials = 5

// 渲染给模拟用户的 agent 回答前缀（agent 一侧只给结构化结果的用户可见部分）。
const (
	replyRefused      = "【拒答】"
	replyWaitApproval = "【待审批】"
)

// errNoUserChannel 表示 user_sim 样本运行时未配置用户通道。
var errNoUserChannel = errors.New("user_sim sample requires a user channel (WithUserChannel)")

// ConvTurn 是 Trial.Conversation 的一轮：用户的话、渲染后的 agent 回答与该轮 run_id（供人工阅读 trial 对话）。
type ConvTurn struct {
	User  string `json:"user"`
	Agent string `json:"agent"`
	RunID uint64 `json:"run_id"`
}

// linkFunc 为续问建本轮 run（生产路径 AskService.CreateRun；单测替换）。
type linkFunc func(ctx context.Context, a *app.App, scope model.Scope, in service.AskInput) (uint64, error)

// createRun 是 linkFunc 的生产实现。
func createRun(ctx context.Context, a *app.App, scope model.Scope, in service.AskInput) (uint64, error) {
	if a.Ask == nil {
		return 0, errors.New("user_sim follow-up requires App.Ask")
	}
	return a.Ask.CreateRun(ctx, scope, in)
}

// runDetailTokens 从 trial 库读一轮 run 的 LLM token（与 recordDetail 同条件：无库 / 无 RunDetail 时为 0，
// 与单轮样本无 Detail 时 token 为 0 一致）。
func runDetailTokens(ctx context.Context, a *app.App, scope model.Scope, runID uint64) (int, error) {
	if a.DB == nil || a.RunDetail == nil {
		return 0, nil
	}
	d, err := a.RunDetail.Get(ctx, scope, runID)
	if err != nil {
		return 0, err
	}
	return DetailTokens(&d), nil
}

// WithUserChannel 设置多轮样本的用户通道（usersim.Simulated；未配置时 user_sim 样本记 infra_error）。
func WithUserChannel(ch usersim.Channel) RunnerOption {
	return func(r *Runner) { r.users = ch }
}

// checkUserSimTrials：未隔离的 user_sim 样本须 n ≥ MinUserSimTrials（在 splitQuarantined 之后调用）。
func checkUserSimTrials(samples []*sample.Sample, n int) error {
	for _, s := range samples {
		if s.UserSim != nil && n < MinUserSimTrials {
			return fmt.Errorf("evalkit: user_sim sample %s needs n >= %d, got %d", s.ID, MinUserSimTrials, n)
		}
	}
	return nil
}

// converse 在第一轮回答 ans 之后续问（样本无 user_sim 时原样返回），返回最后一轮的提问与回答；对话写 t.Conversation。
func (r *Runner) converse(ctx context.Context, t *Trial, a *app.App, s *sample.Sample, q agent.Question,
	ans agent.Answer) (agent.Question, agent.Answer, error) {
	if s.UserSim == nil {
		return q, ans, nil
	}
	if r.users == nil {
		return q, ans, errNoUserChannel
	}
	t.Conversation = []ConvTurn{{User: q.Text, Agent: renderReply(ans), RunID: ans.RunID}}
	for ans.Ticket == nil && len(t.Conversation) < s.UserSim.MaxTurns {
		text, ok, err := r.users.Next(ctx, s, historyOf(t.Conversation))
		if err != nil {
			return q, ans, fmt.Errorf("user sim turn %d: %w", len(t.Conversation), err)
		}
		if !ok {
			break
		}
		if ans.RunID == 0 {
			return q, ans, fmt.Errorf("user sim turn %d: previous answer has no run_id to continue", len(t.Conversation))
		}
		// 上一轮不是末轮：其 token 计入 PriorTurnTokens（末轮由 recordDetail 读进 Detail）。
		n, err := r.runTokens(ctx, a, q.Scope, ans.RunID)
		if err != nil {
			return q, ans, fmt.Errorf("user sim turn %d: read tokens of run %d: %w", len(t.Conversation), ans.RunID, err)
		}
		t.PriorTurnTokens += n
		next := q
		next.Text = text
		if next.RunID, err = r.link(ctx, a, q.Scope, service.AskInput{FieldID: q.FieldID, Text: text, PrevRunID: ans.RunID}); err != nil {
			return q, ans, fmt.Errorf("user sim turn %d: create run: %w", len(t.Conversation), err)
		}
		if ans, err = a.Agent.Ask(ctx, next); err != nil {
			return next, ans, fmt.Errorf("user sim turn %d: %w", len(t.Conversation), err)
		}
		q = next
		t.Conversation = append(t.Conversation, ConvTurn{User: text, Agent: renderReply(ans), RunID: ans.RunID})
	}
	return q, ans, nil
}

// historyOf 是通道看到的历史（不含 run_id：录制键的对话哈希只随文本变化）。
func historyOf(conv []ConvTurn) []usersim.Turn {
	h := make([]usersim.Turn, len(conv))
	for i, c := range conv {
		h[i] = usersim.Turn{User: c.User, Agent: c.Agent}
	}
	return h
}

// renderReply 把结构化回答渲染成给模拟用户看的文本：开票为待审批 + 结论，拒答为原因，否则为建议卡结论。
func renderReply(ans agent.Answer) string {
	conclusion := ""
	if ans.Card != nil {
		conclusion = ans.Card.Conclusion
	}
	switch {
	case ans.Ticket != nil:
		return replyWaitApproval + conclusion
	case ans.Outcome == model.OutcomeRefused:
		return replyRefused + ans.RefuseReason
	}
	return conclusion
}
