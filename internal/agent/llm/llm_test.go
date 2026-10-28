package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const tierStd Tier = "standard"

func testPrompts(t *testing.T) *Prompts {
	t.Helper()
	p, err := NewPrompts("v-test", "layers", map[Task]string{TaskClassify: "c", TaskChoose: "h", TaskExplain: "e"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// scripted 依次返回 replies；元素为 error 时返回该错误，"block" 时阻塞到 ctx 结束。
type scripted struct {
	replies []any
	n       atomic.Int32
}

func (s *scripted) Generate(ctx context.Context, _ []*schema.Message, _ ...einomodel.Option) (*schema.Message, error) {
	i := int(s.n.Add(1)) - 1
	if i >= len(s.replies) {
		i = len(s.replies) - 1
	}
	switch r := s.replies[i].(type) {
	case error:
		return nil, r
	case string:
		if r == "block" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		m := schema.AssistantMessage(r, nil)
		m.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
		return m, nil
	}
	return nil, errors.New("bad script")
}

func (s *scripted) Stream(context.Context, []*schema.Message, ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("not used")
}

type fixedLimiter struct {
	allow bool
	err   error
}

func (l fixedLimiter) Allow(context.Context, Tier) (bool, error) { return l.allow, l.err }

func TestClient_Do(t *testing.T) {
	boom := errors.New("upstream 502")
	tests := []struct {
		name      string
		replies   []any
		limiter   Limiter
		retries   int
		wantErr   error
		wantCalls int32
		wantUsage int
	}{
		{name: "ok", replies: []any{`{"product_id":"a"}`}, wantCalls: 1, wantUsage: 15},
		{name: "fenced json ok", replies: []any{"```json\n{\"product_id\":\"a\"}\n```"}, wantCalls: 1, wantUsage: 15},
		{name: "retry then ok", replies: []any{boom, `{"product_id":"a"}`}, retries: 1, wantCalls: 2, wantUsage: 15},
		{name: "retries exhausted", replies: []any{boom}, retries: 2, wantErr: ErrUnavailable, wantCalls: 3},
		{name: "bad json counts usage", replies: []any{"not json"}, wantErr: ErrBadReply, wantCalls: 1, wantUsage: 15},
		{name: "timeout", replies: []any{"block"}, wantErr: ErrUnavailable, wantCalls: 1},
		{name: "rate limited", replies: []any{`{}`}, limiter: fixedLimiter{allow: false}, wantErr: ErrRateLimited},
		{name: "limiter error", replies: []any{`{}`}, limiter: fixedLimiter{err: boom}, wantErr: ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &scripted{replies: tt.replies}
			c, err := New(Config{Model: m, Prompts: testPrompts(t), Limiter: tt.limiter,
				Tiers: map[Tier]TierConfig{tierStd: {MaxTokens: 100, Timeout: 50 * time.Millisecond, MaxRetries: tt.retries}}})
			if err != nil {
				t.Fatal(err)
			}
			var out ChooseOutput
			u, err := c.Do(context.Background(), tierStd, TaskChoose, ChooseInput{Question: "q"}, &out)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && out.ProductID != "a" {
				t.Fatalf("out = %+v", out)
			}
			if tt.wantErr != nil && !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err %v must wrap ErrUnavailable", err)
			}
			if got := m.n.Load(); got != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", got, tt.wantCalls)
			}
			if u.TotalTokens != tt.wantUsage || c.Usage()[tierStd].TotalTokens != tt.wantUsage {
				t.Fatalf("usage = %+v / %+v, want %d", u, c.Usage(), tt.wantUsage)
			}
		})
	}
}

func TestClient_DoErrors(t *testing.T) {
	c, err := New(Config{Model: &scripted{replies: []any{`{}`}}, Prompts: testPrompts(t),
		Tiers: map[Tier]TierConfig{tierStd: {MaxTokens: 1, Timeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	var out ChooseOutput
	if _, err := c.Do(context.Background(), "nope", TaskChoose, nil, &out); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("err = %v, want ErrUnknownTier", err)
	}
	if _, err := c.Do(context.Background(), tierStd, "nope", nil, &out); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("err = %v, want ErrUnknownTask", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked, err := New(Config{Model: &scripted{replies: []any{"block"}}, Prompts: testPrompts(t),
		Tiers: map[Tier]TierConfig{tierStd: {MaxTokens: 1, Timeout: time.Second, MaxRetries: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.Do(ctx, tierStd, TaskChoose, nil, &out); !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want context.Canceled (not degradable)", err)
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error for empty config")
	}
}

func TestClient_Redacts(t *testing.T) {
	var seen string
	m := modelFunc(func(msgs []*schema.Message) string {
		seen = msgs[len(msgs)-1].Content
		return `{"product_id":"a"}`
	})
	c, err := New(Config{Model: m, Prompts: testPrompts(t), Redact: func(s string) string { return strings.ReplaceAll(s, "13800138000", "***") },
		Tiers: map[Tier]TierConfig{tierStd: {MaxTokens: 1, Timeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	var out ChooseOutput
	if _, err := c.Do(context.Background(), tierStd, TaskChoose, ChooseInput{Question: "打电话13800138000"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seen, "13800138000") || !strings.Contains(seen, "***") {
		t.Fatalf("user message not redacted: %s", seen)
	}
}

type modelFunc func([]*schema.Message) string

func (f modelFunc) Generate(_ context.Context, msgs []*schema.Message, _ ...einomodel.Option) (*schema.Message, error) {
	return schema.AssistantMessage(f(msgs), nil), nil
}

func (f modelFunc) Stream(context.Context, []*schema.Message, ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("not used")
}

func TestLoadPrompts_Repo(t *testing.T) {
	p, err := LoadPrompts("../../../prompts", "v1")
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.System(TaskExplain)
	if err != nil || !strings.Contains(s, "explain") || !strings.Contains(s, "边界") || p.Version() != "v1" {
		t.Fatalf("system = %q, err %v", s, err)
	}
	if _, err := LoadPrompts("../../../prompts", "../v1"); err == nil {
		t.Fatal("want error for path-like version")
	}
	if _, err := LoadPrompts(t.TempDir(), "v1"); err == nil {
		t.Fatal("want error for missing files")
	}
}

func TestOpenAI_Generate(t *testing.T) {
	var gotAuth string
	var gotReq chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil || r.URL.Path != chatCompletionsPath || json.Unmarshal(body, &gotReq) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if gotReq.Messages[0].Content == "fail" {
			http.Error(w, `{"error":"quota"}`, http.StatusTooManyRequests)
			return
		}
		_, err = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
		if err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	m, err := NewOpenAI(OpenAIConfig{BaseURL: srv.URL + "/", APIKey: "k-test", Model: "m-test"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.Generate(context.Background(), []*schema.Message{schema.SystemMessage("s"), schema.UserMessage("u")},
		einomodel.WithTemperature(0.2), einomodel.WithMaxTokens(64))
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != `{"ok":true}` || out.ResponseMeta.Usage.TotalTokens != 5 {
		t.Fatalf("out = %+v", out)
	}
	if gotAuth != "Bearer k-test" || gotReq.Model != "m-test" || *gotReq.MaxTokens != 64 || len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" {
		t.Fatalf("request = %+v auth %q", gotReq, gotAuth)
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.SystemMessage("fail")}); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want status 429", err)
	}
}

func TestOpenAIConfigFromEnv(t *testing.T) {
	t.Setenv(EnvBaseURL, "http://x")
	t.Setenv(EnvAPIKey, "")
	t.Setenv(EnvModel, "")
	_, err := OpenAIConfigFromEnv()
	if !errors.Is(err, ErrMissingEnv) || !strings.Contains(err.Error(), EnvAPIKey+", "+EnvModel) {
		t.Fatalf("err = %v", err)
	}
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvModel, "m")
	cfg, err := OpenAIConfigFromEnv()
	if err != nil || cfg.Model != "m" {
		t.Fatalf("cfg = %+v err %v", cfg, err)
	}
}
