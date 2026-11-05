package sample

import (
	"strings"
	"testing"
)

// TestCheckSession（P2 L6）：fixtures.session 的轮次须可写进 trial 库（长度、引用格式、建议卡所需 crop / 物候期），
// expect.compaction 的约束原句须出自某一轮问题。应失败：无会话却声明 compaction、空轮次、空问题、无结论却带药剂 / 引用、
// 结论缺物候期、引用不是 kb_id@version、约束为空 / 带首尾空白 / 不在任何一轮问题里、结论超长。
func TestCheckSession(t *testing.T) {
	session := func() *SessionFixture {
		return &SessionFixture{PhenologyStage: "分蘖期", Turns: []SessionTurn{
			{Question: "旁边是蜂场，打药要注意什么？"},
			{Question: "稻飞虱多了怎么办？", Conclusion: "建议观察三天", Product: "吡蚜酮", Cite: []string{"kb-rice-001@v1"}},
		}}
	}
	tests := []struct {
		name    string
		mutate  func(s *Sample)
		wantMsg string // 空表示应通过
	}{
		{name: "not declared", mutate: func(*Sample) {}},
		{name: "valid session", mutate: func(s *Sample) {
			s.Fixtures.Session = session()
			s.Expect.Compaction = &CompactionExpect{Constraints: []string{"旁边是蜂场"}}
		}},
		{name: "session without compaction expect", mutate: func(s *Sample) { s.Fixtures.Session = session() }},
		{name: "compaction without session", wantMsg: "requires fixtures.session", mutate: func(s *Sample) {
			s.Expect.Compaction = &CompactionExpect{Constraints: []string{"旁边是蜂场"}}
		}},
		{name: "no turns", wantMsg: "turns must be 1..31", mutate: func(s *Sample) {
			s.Fixtures.Session = &SessionFixture{}
		}},
		{name: "too many turns", wantMsg: "turns must be 1..31", mutate: func(s *Sample) {
			fx := &SessionFixture{}
			for i := 0; i <= MaxSessionTurns; i++ {
				fx.Turns = append(fx.Turns, SessionTurn{Question: "还要注意什么？"})
			}
			s.Fixtures.Session = fx
		}},
		{name: "blank question", wantMsg: "turns[0].question must be 1..500 chars", mutate: func(s *Sample) {
			fx := session()
			fx.Turns[0].Question = " "
			s.Fixtures.Session = fx
		}},
		{name: "product without conclusion", wantMsg: "product / cite / order require conclusion", mutate: func(s *Sample) {
			fx := session()
			fx.Turns[0].Product = "吡蚜酮"
			s.Fixtures.Session = fx
		}},
		{name: "order without conclusion", wantMsg: "product / cite / order require conclusion", mutate: func(s *Sample) {
			fx := session()
			fx.Turns[0].Order = &SessionOrder{Status: "CANCELED", CancelReason: "CANCELED_BY_USER"}
			s.Fixtures.Session = fx
		}},
		{name: "valid orders and measured", mutate: func(s *Sample) {
			fx := session()
			fx.Turns[1].Order = &SessionOrder{Status: "REJECTED", RejectCode: "PRODUCT", RejectText: "换药"}
			fx.Turns[0].Measured = []SessionMeasured{{Tool: "weather.forecast", Result: map[string]any{"rain_mm": 3}},
				{Tool: "weather.forecast", Result: map[string]any{"rain_mm": 9}, Superseded: true}}
			s.Fixtures.Session = fx
		}},
		{name: "order bad status", wantMsg: "order.status must be one of", mutate: func(s *Sample) {
			fx := session()
			fx.Turns[1].Order = &SessionOrder{Status: "APPROVED"}
			s.Fixtures.Session = fx
		}},
		{name: "rejected without reason", wantMsg: "REJECTED requires reject_code", mutate: func(s *Sample) {
			fx := session()
			fx.Turns[1].Order = &SessionOrder{Status: "REJECTED", RejectCode: "PRODUCT"}
			s.Fixtures.Session = fx
		}},
		{name: "canceled without reason", wantMsg: "CANCELED requires cancel_reason", mutate: func(s *Sample) {
			fx := session()
			fx.Turns[1].Order = &SessionOrder{Status: "CANCELED"}
			s.Fixtures.Session = fx
		}},
		{name: "measured without result", wantMsg: "measured[0]: tool and result are required", mutate: func(s *Sample) {
			fx := session()
			fx.Turns[0].Measured = []SessionMeasured{{Tool: "weather.forecast"}}
			s.Fixtures.Session = fx
		}},
		{name: "conclusion without stage", wantMsg: "requires sample crop and fixtures.session.phenology_stage", mutate: func(s *Sample) {
			fx := session()
			fx.PhenologyStage = ""
			s.Fixtures.Session = fx
		}},
		{name: "conclusion too long", wantMsg: "conclusion exceeds 512 chars", mutate: func(s *Sample) {
			fx := session()
			fx.Turns[1].Conclusion = strings.Repeat("观", maxSessionConclusionRunes+1)
			s.Fixtures.Session = fx
		}},
		{name: "bad cite", wantMsg: "is not kb_id@version", mutate: func(s *Sample) {
			fx := session()
			fx.Turns[1].Cite = []string{"kb-rice-001"}
			s.Fixtures.Session = fx
		}},
		{name: "empty constraints", wantMsg: "constraints is required", mutate: func(s *Sample) {
			s.Fixtures.Session = session()
			s.Expect.Compaction = &CompactionExpect{}
		}},
		{name: "constraint with spaces", wantMsg: "without surrounding spaces", mutate: func(s *Sample) {
			s.Fixtures.Session = session()
			s.Expect.Compaction = &CompactionExpect{Constraints: []string{" 旁边是蜂场"}}
		}},
		{name: "constraint not in any turn", wantMsg: "is not in any fixtures.session question", mutate: func(s *Sample) {
			s.Fixtures.Session = session()
			s.Expect.Compaction = &CompactionExpect{Constraints: []string{"有机认证"}}
		}},
	}
	v := NewValidator(DefaultRules(mustTaxonomy(t), repoKB(t))...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := validSet()
			tt.mutate(set[1])
			report := v.Validate(set)
			var hit bool
			for _, viol := range report.Violations {
				if viol.Rule == RuleSession && viol.Subject() == "s-1" && strings.Contains(viol.Msg, tt.wantMsg) {
					hit = true
				}
			}
			if tt.wantMsg == "" {
				if !report.OK() {
					t.Fatalf("violations = %v, want none", report.Violations)
				}
				return
			}
			if !hit {
				t.Fatalf("violations = %v, want %s containing %q", report.Violations, RuleSession, tt.wantMsg)
			}
		})
	}
}

func TestSplitCite(t *testing.T) {
	tests := []struct {
		in      string
		id, ver string
		ok      bool
	}{
		{in: "kb-rice-001@v1", id: "kb-rice-001", ver: "v1", ok: true},
		{in: "a@b@2026-10", id: "a@b", ver: "2026-10", ok: true},
		{in: "kb-rice-001"},
		{in: "@v1"},
		{in: "kb-rice-001@"},
	}
	for _, tt := range tests {
		id, ver, ok := SplitCite(tt.in)
		if id != tt.id || ver != tt.ver || ok != tt.ok {
			t.Errorf("SplitCite(%q) = %q, %q, %v; want %q, %q, %v", tt.in, id, ver, ok, tt.id, tt.ver, tt.ok)
		}
	}
}
