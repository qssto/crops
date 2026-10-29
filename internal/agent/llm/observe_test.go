package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stepClock 每次 Now 前进 step。
type stepClock struct {
	t    time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time { c.t = c.t.Add(c.step); return c.t }

func TestClient_Observer(t *testing.T) {
	boom := errors.New("upstream 502")
	tests := []struct {
		name         string
		replies      []any
		retries      int
		limiter      Limiter
		wantCalls    int // Observer 回调次数
		wantAttempts int
		wantErr      bool
		wantTokens   int
	}{
		{name: "ok", replies: []any{`{"product_id":"a"}`}, wantCalls: 1, wantAttempts: 1, wantTokens: 15},
		{name: "retry then ok", replies: []any{boom, `{"product_id":"a"}`}, retries: 1, wantCalls: 1, wantAttempts: 2, wantTokens: 15},
		{name: "bad reply keeps raw", replies: []any{"not json"}, wantCalls: 1, wantAttempts: 1, wantErr: true, wantTokens: 15},
		{name: "rate limited not observed", replies: []any{`{}`}, limiter: fixedLimiter{allow: false}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []Call
			c, err := New(Config{Model: &scripted{replies: tt.replies}, Prompts: testPrompts(t), Limiter: tt.limiter,
				Tiers:    map[Tier]TierConfig{tierStd: {Timeout: time.Second, MaxTokens: 10, MaxRetries: tt.retries}},
				Observer: func(_ context.Context, c Call) { got = append(got, c) },
				Clock:    &stepClock{t: time.Unix(0, 0), step: 5 * time.Millisecond}})
			if err != nil {
				t.Fatal(err)
			}
			var out ChooseOutput
			_, err = c.Do(t.Context(), tierStd, TaskChoose, ChooseInput{Question: "q"}, &out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(got) != tt.wantCalls {
				t.Fatalf("observer calls = %d, want %d", len(got), tt.wantCalls)
			}
			if tt.wantCalls == 0 {
				return
			}
			call := got[0]
			if call.Task != TaskChoose || call.Tier != tierStd || len(call.Attempts) != tt.wantAttempts {
				t.Fatalf("call = %+v", call)
			}
			if call.Usage.TotalTokens != tt.wantTokens || call.Latency <= 0 || (call.Err != nil) != tt.wantErr {
				t.Fatalf("usage=%+v latency=%v err=%v", call.Usage, call.Latency, call.Err)
			}
			if len(call.Messages) != 2 || call.Messages[0].Role != "system" || !strings.Contains(call.Messages[1].Content, `"task"`) {
				t.Fatalf("messages = %+v", call.Messages)
			}
			if last := call.Attempts[len(call.Attempts)-1]; last.Reply == "" {
				t.Fatalf("last attempt reply missing: %+v", last)
			}
		})
	}
}

func TestNew_ObserverRequiresClock(t *testing.T) {
	_, err := New(Config{Model: &scripted{replies: []any{`{}`}}, Prompts: testPrompts(t),
		Tiers:    map[Tier]TierConfig{tierStd: {Timeout: time.Second, MaxTokens: 10}},
		Observer: func(context.Context, Call) {}})
	if err == nil {
		t.Fatal("want error when observer set without clock")
	}
}
