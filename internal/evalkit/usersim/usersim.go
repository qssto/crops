// Package usersim 是多轮评测的用户通道（P2 L7-b，评测设计方案 §9.2）：Runner 以样本 question 为第一句，
// 每次 agent 回答后问通道要下一句。Scripted 是脚本用户（只说第一句，与单轮回放一致）；Simulated 是 LLM 模拟用户
// （按样本 user_sim 的 persona / instruction 续问，输出 ###STOP### 或达到 max_turns 即结束）。
// 模拟用户模型独立配置（eval/usersim/usersim.yaml），现只有录制回放客户端（Replay，待真实模型）。
package usersim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agri-steward/internal/evalkit/sample"
)

// StopToken 是模拟用户结束对话的标记（§9.2）。
const StopToken = "###STOP###"

var (
	// ErrNotUserSim 表示样本没有 user_sim 块却交给了模拟用户通道。
	ErrNotUserSim = errors.New("usersim: sample has no user_sim block")
	// ErrEmptyReply 表示模拟用户给了空回复（不当作结束，也不编造回复）。
	ErrEmptyReply = errors.New("usersim: empty reply")
)

// Turn 是一轮对话：用户说的话与 agent 的回答（agent 一侧由 Runner 从结构化结果渲染）。
type Turn struct {
	User  string `json:"user"`
	Agent string `json:"agent"`
}

// Channel 是用户通道：history 是已完成的轮次（至少含第一句及其回答），返回下一句；ok=false 表示对话结束。
type Channel interface {
	Next(ctx context.Context, s *sample.Sample, history []Turn) (text string, ok bool, err error)
}

// Scripted 是脚本用户：只说样本 question 一句，从不续问。
type Scripted struct{}

var _ Channel = Scripted{}

// Next 实现 Channel：总是结束。
func (Scripted) Next(context.Context, *sample.Sample, []Turn) (string, bool, error) {
	return "", false, nil
}

// Request 是一次模拟用户调用：Turn 是要生成的轮次下标（第一句为 0，故 Turn = len(History) ≥ 1）。
type Request struct {
	SampleID    string
	Turn        int
	ConvHash    string
	Persona     string
	Instruction string
	History     []Turn
}

// Client 是模拟用户模型接缝。
type Client interface {
	Reply(ctx context.Context, r Request) (string, error)
}

// Simulated 是 LLM 模拟用户通道。
type Simulated struct {
	client Client
}

var _ Channel = (*Simulated)(nil)

// NewSimulated 用模型客户端构造模拟用户通道。
func NewSimulated(c Client) *Simulated { return &Simulated{client: c} }

// Next 实现 Channel：达到 max_turns（含第一句）即结束；回复含 StopToken 即结束；空回复与客户端错误返回错误。
func (m *Simulated) Next(ctx context.Context, s *sample.Sample, history []Turn) (string, bool, error) {
	u := s.UserSim
	if u == nil {
		return "", false, fmt.Errorf("%w: %s", ErrNotUserSim, s.ID)
	}
	if len(history) == 0 {
		return "", false, fmt.Errorf("usersim: %s: history must contain the answered opening question", s.ID)
	}
	if len(history) >= u.MaxTurns {
		return "", false, nil
	}
	req := Request{SampleID: s.ID, Turn: len(history), ConvHash: ConvHash(u, history), Persona: u.Persona,
		Instruction: u.Instruction, History: history}
	reply, err := m.client.Reply(ctx, req)
	if err != nil {
		return "", false, fmt.Errorf("usersim: %s turn %d: %w", s.ID, req.Turn, err)
	}
	if strings.Contains(reply, StopToken) {
		return "", false, nil
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return "", false, fmt.Errorf("%w: %s turn %d", ErrEmptyReply, s.ID, req.Turn)
	}
	return reply, true, nil
}

// ConvHash 是录制键里的对话哈希：sha256(JSON{persona, instruction, history}) 的小写 hex。
// 人设、隐藏指令或任一轮文本变化都换键，录制失效（缺录制）而不是错配。
func ConvHash(u *sample.UserSim, history []Turn) string {
	body, err := json.Marshal(struct {
		Persona     string `json:"persona"`
		Instruction string `json:"instruction"`
		History     []Turn `json:"history"`
	}{u.Persona, u.Instruction, history})
	if err != nil { // 只含字符串字段，不会失败
		panic(fmt.Sprintf("usersim: marshal conversation: %v", err))
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
