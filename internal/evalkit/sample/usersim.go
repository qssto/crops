package sample

import (
	"fmt"
	"slices"
	"strings"
)

// P2 L7-b 多轮模拟用户样本（评测设计方案 §9.2）：user_sim 声明隐藏指令与人设，Runner 以 sample.question 为第一句，
// 之后每轮由模拟用户通道（internal/evalkit/usersim）按 instruction 续问，直到输出 ###STOP###、达到 max_turns
// 或 agent 进入 WAIT_APPROVAL。只用于 capability 套件，红线门禁按 user_sim tag 排除（§6.8 验收 4）。

// RuleUserSim 是 user_sim 块与 tag 的结构校验。
const RuleUserSim = "user_sim"

// MaxUserSimTurns 是 user_sim.max_turns 的上限（§9.2 初始 10 轮，含第一句）。
const MaxUserSimTurns = 10

// SuiteCapability 是能力套件：模拟用户样本只进这里（只算能力 / 质量指标；gate 排除 user_sim 也要求此套件）。
const SuiteCapability = "capability"

// 模拟用户人设模板（§9.2）。
const (
	PersonaFarmer     = "农场主"
	PersonaAgronomist = "农艺师"
	PersonaOperator   = "机手"
)

// UserSimPersonas 是全部人设模板。
var UserSimPersonas = []string{PersonaFarmer, PersonaAgronomist, PersonaOperator}

// UserSim 是模拟用户声明：Instruction 只给模拟用户看（不进 agent 提示词）；MaxTurns 含第一句 question。
type UserSim struct {
	Persona     string `yaml:"persona"`
	Instruction string `yaml:"instruction"`
	MaxTurns    int    `yaml:"max_turns"`
}

func checkUserSim(s *Sample) []string {
	tagged := slices.Contains(s.Tags, TagUserSim)
	u := s.UserSim
	switch {
	case u == nil && tagged:
		return []string{"tag user_sim requires a user_sim block"}
	case u == nil:
		return nil
	case !tagged:
		return []string{"user_sim block requires tag user_sim (red-line gates exclude samples by that tag)"}
	}
	var msgs []string
	if !slices.Contains(UserSimPersonas, u.Persona) {
		msgs = append(msgs, fmt.Sprintf("user_sim.persona must be one of %v, got %q", UserSimPersonas, u.Persona))
	}
	if strings.TrimSpace(u.Instruction) == "" {
		msgs = append(msgs, "user_sim.instruction is required")
	}
	if u.MaxTurns < 1 || u.MaxTurns > MaxUserSimTurns {
		msgs = append(msgs, fmt.Sprintf("user_sim.max_turns must be 1..%d, got %d", MaxUserSimTurns, u.MaxTurns))
	}
	if s.Suite != SuiteCapability {
		msgs = append(msgs, fmt.Sprintf("user_sim samples belong only in suite %s, got %q", SuiteCapability, s.Suite))
	}
	return msgs
}
