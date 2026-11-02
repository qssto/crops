package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

type fakeSource struct {
	profile  Profile
	episodes []agent.MemoryItem
	err      error
	calls    int
	gotLimit int
}

func (f *fakeSource) Profile(context.Context, model.Scope, uint64) (Profile, error) {
	f.calls++
	return f.profile, f.err
}

func (f *fakeSource) Episodic(_ context.Context, _ model.Scope, _ uint64, limit int) ([]agent.MemoryItem, error) {
	f.calls++
	f.gotLimit = limit
	if len(f.episodes) > limit {
		return f.episodes[:limit], nil
	}
	return f.episodes, nil
}

var scope = model.Scope{TenantID: 1, FarmID: 1}

func episode(kind, summary string, ref uint64) agent.MemoryItem {
	return agent.MemoryItem{Kind: kind, RefID: ref, Summary: summary, OccurredAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
}

func kinds(items []agent.MemoryItem) string {
	var ks []string
	for _, it := range items {
		ks = append(ks, it.Kind)
	}
	return strings.Join(ks, ",")
}

func TestStore_Recent(t *testing.T) {
	eps := []agent.MemoryItem{episode("REJECTED", "作业单驳回：雨前不宜喷药", 9), episode("ADVICE", "建议观察稻飞虱虫量", 8)}
	tests := []struct {
		name      string
		opts      Options
		src       *fakeSource
		ctx       context.Context
		limit     int
		wantKinds string
		wantCalls int
		wantLimit int
		wantErr   bool
	}{
		{name: "disabled reads nothing", opts: Options{}, src: &fakeSource{episodes: eps}, wantCalls: 0},
		{name: "working profile episodic in relevance order", opts: Options{Enabled: true},
			src:       &fakeSource{profile: Profile{Summary: "地块 F1 水稻，偏好吡蚜酮", Found: true}, episodes: eps},
			ctx:       WithWorking(context.Background(), scope, agent.MemoryItem{Summary: "上一问：稻飞虱"}),
			wantKinds: "WORKING,PROFILE,REJECTED,ADVICE", wantCalls: 2, wantLimit: DefaultEpisodicLimit},
		{name: "other farm working dropped", opts: Options{Enabled: true}, src: &fakeSource{episodes: eps[:1]},
			ctx:       WithWorking(context.Background(), model.Scope{TenantID: 1, FarmID: 2}, agent.MemoryItem{Summary: "他场会话"}),
			wantKinds: "REJECTED", wantCalls: 2, wantLimit: DefaultEpisodicLimit},
		{name: "limit capped by config", opts: Options{Enabled: true, EpisodicLimit: 1}, limit: 10,
			src: &fakeSource{episodes: eps}, wantKinds: "REJECTED", wantCalls: 2, wantLimit: 1},
		{name: "rune budget truncates least relevant", opts: Options{Enabled: true, RuneBudget: 15},
			src: &fakeSource{episodes: eps}, wantKinds: "REJECTED", wantCalls: 2, wantLimit: DefaultEpisodicLimit},
		{name: "profile not found skipped", opts: Options{Enabled: true}, src: &fakeSource{episodes: eps[:1]},
			wantKinds: "REJECTED", wantCalls: 2, wantLimit: DefaultEpisodicLimit},
		{name: "source error wrapped", opts: Options{Enabled: true}, src: &fakeSource{err: errors.New("db down")},
			wantCalls: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			got, err := New(tt.src, tt.opts).Recent(ctx, scope, 3, tt.limit)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if kinds(got) != tt.wantKinds || tt.src.calls != tt.wantCalls {
				t.Fatalf("kinds = %q calls = %d, want %q / %d", kinds(got), tt.src.calls, tt.wantKinds, tt.wantCalls)
			}
			if tt.wantLimit != 0 && tt.src.gotLimit != tt.wantLimit {
				t.Fatalf("episodic limit = %d, want %d", tt.src.gotLimit, tt.wantLimit)
			}
		})
	}
}

func TestStore_NilIsDisabled(t *testing.T) {
	var s *Store
	if s.Enabled() {
		t.Fatal("nil store enabled")
	}
	in := []agent.MemoryItem{{Kind: "ADVICE", Summary: "x"}}
	ctx := WithWorking(context.Background(), scope, in...)
	if in[0].Kind != "ADVICE" || WorkingFrom(ctx, scope)[0].Kind != KindWorking {
		t.Fatalf("WithWorking must copy: in=%v ctx=%v", in, WorkingFrom(ctx, scope))
	}
}

// TestStore_ScopeIsolation：零 Scope 拒绝且不读库；working 只对绑定的同一农场可见。
func TestStore_ScopeIsolation(t *testing.T) {
	src := &fakeSource{episodes: []agent.MemoryItem{episode("ADVICE", "a", 1)}}
	for _, s := range []model.Scope{{}, {TenantID: 1}, {FarmID: 1}} {
		if _, err := New(src, Options{Enabled: true}).Recent(context.Background(), s, 3, 0); !errors.Is(err, ErrScopeRequired) {
			t.Fatalf("scope %+v: err = %v, want ErrScopeRequired", s, err)
		}
	}
	if src.calls != 0 {
		t.Fatalf("source called %d times on missing scope", src.calls)
	}
	// P1-L1-B 审查 P2-10：关闭 / nil 的 Store 也先过 Scope 闸门；有归属时关闭仍返回空。
	stores := map[string]*Store{"disabled": New(src, Options{Enabled: false}), "nil": nil}
	for name, st := range stores {
		if _, err := st.Recent(context.Background(), model.Scope{TenantID: 1}, 3, 0); !errors.Is(err, ErrScopeRequired) {
			t.Fatalf("%s store, no farm: err = %v, want ErrScopeRequired", name, err)
		}
		if items, err := st.Recent(context.Background(), scope, 3, 0); err != nil || items != nil {
			t.Fatalf("%s store, valid scope: items=%v err=%v, want nil,nil", name, items, err)
		}
	}
	if src.calls != 0 {
		t.Fatalf("disabled store read source %d times", src.calls)
	}
	ctx := WithWorking(context.Background(), scope, agent.MemoryItem{Summary: "本场会话"})
	for _, tt := range []struct {
		s    model.Scope
		want int
	}{{scope, 1}, {model.Scope{TenantID: 1, FarmID: 2}, 0}, {model.Scope{TenantID: 2, FarmID: 1}, 0}, {model.Scope{}, 0}} {
		if got := WorkingFrom(ctx, tt.s); len(got) != tt.want {
			t.Fatalf("WorkingFrom(%+v) = %v, want %d items", tt.s, got, tt.want)
		}
	}
	if got := WorkingFrom(WithWorking(context.Background(), model.Scope{}, agent.MemoryItem{Summary: "x"}), model.Scope{}); got != nil {
		t.Fatalf("unbound working visible: %v", got)
	}
}
