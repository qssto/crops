package sample

import (
	"strings"
	"testing"
	"time"
)

// TestCheckFault（P1 L6）：缓存桩只用于有 max_staleness 的 server 且带 RFC3339 collected_at；fixtures.model.unavailable
// 非空且取已知任务；dispatch.advance 在 (0, 24h]；expect.degraded 取矩阵枚举。
func TestCheckFault(t *testing.T) {
	const cachedAt = "2026-07-15T08:00:00+08:00"
	withCache := func(tool string, cache any) func(s *Sample) {
		return func(s *Sample) {
			s.Fixtures.MCP = map[string][]ToolFixture{tool: {{Error: FaultTimeout, Cache: cache}}}
		}
	}
	tests := []struct {
		name    string
		mutate  func(s *Sample)
		wantErr string // 空表示应通过
	}{
		{name: "weather cache ok", mutate: withCache("weather.forecast", map[string]any{"collected_at": cachedAt})},
		{name: "crop cache ok", mutate: withCache("crop.phenology", map[string]any{"collected_at": cachedAt})},
		{name: "model down ok", mutate: func(s *Sample) {
			s.Fixtures.Model = &ModelFixture{Unavailable: []string{"explain", "choose"}}
		}},
		{name: "model all ok", mutate: func(s *Sample) { s.Fixtures.Model = &ModelFixture{Unavailable: []string{ModelTaskAll}} }},
		{name: "degraded ok", mutate: func(s *Sample) {
			s.Expect.Degraded = []DegradedExpect{{Fault: "MCP_TIMEOUT", Action: "CACHE_FALLBACK"}}
		}},
		{name: "cache on uncached server", wantErr: "not cacheable",
			mutate: withCache("field.pest_survey", map[string]any{"collected_at": cachedAt})},
		{name: "cache without collected_at", wantErr: "RFC3339 collected_at",
			mutate: withCache("weather.forecast", map[string]any{"temp_c": 26})},
		{name: "cache collected_at not rfc3339", wantErr: "RFC3339 collected_at",
			mutate: withCache("weather.forecast", map[string]any{"collected_at": "2026-07-15"})},
		{name: "cache not an object", wantErr: "RFC3339 collected_at", mutate: withCache("weather.forecast", "stale")},
		{name: "empty model fixture", wantErr: "must not be empty",
			mutate: func(s *Sample) { s.Fixtures.Model = &ModelFixture{} }},
		{name: "unknown model task", wantErr: `"summarize" is not allowed`,
			mutate: func(s *Sample) { s.Fixtures.Model = &ModelFixture{Unavailable: []string{"summarize"}} }},
		{name: "unknown fault", wantErr: `"DB_DOWN" is not allowed`, mutate: func(s *Sample) {
			s.Expect.Degraded = []DegradedExpect{{Fault: "DB_DOWN", Action: "REFUSE"}}
		}},
		{name: "unknown action", wantErr: `"RETRY" is not allowed`, mutate: func(s *Sample) {
			s.Expect.Degraded = []DegradedExpect{{Fault: "KB_MISS", Action: "RETRY"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validAdvice("s-1")
			tt.mutate(s)
			msgs := checkFault(s)
			if tt.wantErr == "" {
				if len(msgs) != 0 {
					t.Fatalf("want ok, got %v", msgs)
				}
				return
			}
			if !strings.Contains(strings.Join(msgs, "; "), tt.wantErr) {
				t.Fatalf("want violation containing %q, got %v", tt.wantErr, msgs)
			}
		})
	}
}

// TestAdvanceDuration：空串为 0；(0, 24h] 内合法；零、负数、超上限与非法串报错。
func TestAdvanceDuration(t *testing.T) {
	tests := []struct {
		advance string
		want    time.Duration
		wantErr bool
	}{
		{advance: "", want: 0},
		{advance: "3h", want: 3 * time.Hour},
		{advance: "24h", want: 24 * time.Hour},
		{advance: "0s", wantErr: true},
		{advance: "-1h", wantErr: true},
		{advance: "25h", wantErr: true},
		{advance: "soon", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.advance, func(t *testing.T) {
			got, err := (&DispatchFixture{Advance: tt.advance}).AdvanceDuration()
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("got %s err=%v, want %s wantErr=%t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
