package sample

import (
	"strings"
	"testing"
)

// TestCheckUserSim（P2 L7-b，评测设计方案 §9.2）：user_sim 块须给出合法 persona、非空 instruction、max_turns 1..10，
// 只用于 capability 套件，且与 user_sim tag 互为充要（红线门禁按 tag 排除，块与 tag 不一致会让模拟用户样本混进红线）。
func TestCheckUserSim(t *testing.T) {
	sim := func() *UserSim {
		return &UserSim{Persona: PersonaFarmer, Instruction: "你想知道明天能不能打药，问清楚风速再决定。", MaxTurns: 4}
	}
	tests := []struct {
		name    string
		mutate  func(s *Sample)
		wantMsg string // 空表示应通过
	}{
		{name: "not declared", mutate: func(*Sample) {}},
		{name: "valid", mutate: func(s *Sample) { s.UserSim = sim(); s.Tags = append(s.Tags, TagUserSim) }},
		{name: "every persona", mutate: func(s *Sample) {
			s.Tags = append(s.Tags, TagUserSim)
			for _, p := range UserSimPersonas {
				s.UserSim = sim()
				s.UserSim.Persona = p
				if msgs := checkUserSim(s); len(msgs) > 0 {
					panic(p + ": " + strings.Join(msgs, "; "))
				}
			}
		}},
		{name: "tag without block", wantMsg: "tag user_sim requires a user_sim block", mutate: func(s *Sample) {
			s.Tags = append(s.Tags, TagUserSim)
		}},
		{name: "block without tag", wantMsg: "user_sim block requires tag user_sim", mutate: func(s *Sample) { s.UserSim = sim() }},
		{name: "unknown persona", wantMsg: "persona must be one of", mutate: func(s *Sample) {
			s.UserSim, s.Tags = sim(), append(s.Tags, TagUserSim)
			s.UserSim.Persona = "farmer"
		}},
		{name: "blank instruction", wantMsg: "instruction is required", mutate: func(s *Sample) {
			s.UserSim, s.Tags = sim(), append(s.Tags, TagUserSim)
			s.UserSim.Instruction = "  "
		}},
		{name: "zero max_turns", wantMsg: "max_turns must be 1..10", mutate: func(s *Sample) {
			s.UserSim, s.Tags = sim(), append(s.Tags, TagUserSim)
			s.UserSim.MaxTurns = 0
		}},
		{name: "too many turns", wantMsg: "max_turns must be 1..10", mutate: func(s *Sample) {
			s.UserSim, s.Tags = sim(), append(s.Tags, TagUserSim)
			s.UserSim.MaxTurns = MaxUserSimTurns + 1
		}},
		{name: "not capability", wantMsg: "only in suite capability", mutate: func(s *Sample) {
			s.UserSim, s.Tags = sim(), append(s.Tags, TagUserSim)
			s.Suite = "regression"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sample{ID: "cap-sim", Suite: SuiteCapability, Tags: []string{"L4"}}
			tc.mutate(s)
			msgs := checkUserSim(s)
			if tc.wantMsg == "" {
				if len(msgs) > 0 {
					t.Fatalf("want pass, got %v", msgs)
				}
				return
			}
			if !strings.Contains(strings.Join(msgs, "; "), tc.wantMsg) {
				t.Fatalf("want %q, got %v", tc.wantMsg, msgs)
			}
		})
	}
}

// user_sim 块随 YAML 严格解析进 Sample（未知字段报错）。
func TestParse_UserSimBlock(t *testing.T) {
	raw := []byte(`id: cap-sim
version: 1
suite: capability
tags: [L4, user_sim]
question: 明天能打药吗？
user_sim:
  persona: 机手
  instruction: 你只关心几点出发。
  max_turns: 3
`)
	s, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.UserSim == nil || s.UserSim.Persona != PersonaOperator || s.UserSim.MaxTurns != 3 || s.UserSim.Instruction != "你只关心几点出发。" {
		t.Fatalf("user_sim = %+v", s.UserSim)
	}
	if _, err := Parse(append(raw, []byte("  hidden: x\n")...)); err == nil {
		t.Fatal("unknown user_sim field accepted")
	}
}
