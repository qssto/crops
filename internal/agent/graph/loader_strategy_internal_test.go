package graph

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/callbacks"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// usageRecorder 记 SetLoaderStrategy 与 ReplaceKBLoadItems 调用。
type usageRecorder struct {
	mu         sync.Mutex
	strategies []string
	items      [][]model.KBLoadItem
}

func (r *usageRecorder) SetLoaderStrategy(_ context.Context, _ model.Scope, _ uint64, s string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.strategies = append(r.strategies, s)
	return nil
}

func (r *usageRecorder) AddTokens(context.Context, model.Scope, uint64, int, int) error { return nil }

func (r *usageRecorder) ReplaceKBLoadItems(_ context.Context, _ model.Scope, items []model.KBLoadItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, slices.Clone(items))
	return nil
}

// runnerFakes 让 recordKnowledgeLoad 在真实 runner 守卫内执行（runner.Alive == nil，Resume 时 epoch = 1）。
type runnerFakes struct{}

type nopLease struct{}

func (nopLease) Renew(context.Context) error   { return nil }
func (nopLease) Release(context.Context) error { return nil }

func (runnerFakes) AcquireRun(context.Context, uint64) (agent.Lease, error) { return nopLease{}, nil }
func (runnerFakes) CreateRun(context.Context, model.RunState, uint64, uint64, agent.RunMeta) (uint64, error) {
	return 1, nil
}
func (runnerFakes) LoadRun(context.Context, model.Scope, uint64) (model.RunState, error) {
	return model.RunState{}, nil
}
func (runnerFakes) SaveStep(context.Context, model.Scope, uint64, model.RunStep) error { return nil }
func (runnerFakes) FinishRun(context.Context, model.Scope, uint64, model.RunStatus, string) error {
	return nil
}
func (runnerFakes) Heartbeat(context.Context, model.Scope, uint64) error { return nil }
func (runnerFakes) MarkResumed(context.Context, model.Scope, agent.ResumeRequest) (int, error) {
	return 1, nil
}

var loaderScope = model.Scope{TenantID: 1, FarmID: 1}

// P3：首次执行用 v0 不写 loader_strategy（列默认值即 v0）；v1 照写；resume（epoch > 0）总写，覆盖中断前的 v1。
// 明细每次加载整批替换（ReplaceKBLoadItems），resume 重跑不追加。
func TestRecordKnowledgeLoad_StrategyWrites(t *testing.T) {
	v1 := mustStrategy(knowledge.StrategySpec{Version: model.LoaderStrategyV1, Limit: 8, Order: []knowledge.SortKey{knowledge.SortAuthority}})
	picks := []knowledge.Pick{{Entry: &knowledge.Entry{KBID: "common/safety/phi", Version: "v1"}, Reason: knowledge.PickRanked}}
	r, err := runner.New(runner.Config{Locker: runnerFakes{}, Runs: runnerFakes{}, Resumes: runnerFakes{}, Clock: clock.System{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		st     *knowledge.Strategy
		resume bool
		want   []string
	}{
		{name: "v0 first run", st: phaseZero, want: nil},
		{name: "v1 first run", st: v1, want: []string{model.LoaderStrategyV1}},
		{name: "v0 resume", st: phaseZero, resume: true, want: []string{model.LoaderStrategyV0}},
		{name: "v1 resume", st: v1, resume: true, want: []string{model.LoaderStrategyV1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := &usageRecorder{}
			a := &Agent{cfg: Config{Usage: rec, Loading: &KnowledgeLoading{Items: rec}, Clock: clock.System{}},
				log: slog.New(slog.DiscardHandler)}
			s := &state{q: agent.Question{Scope: loaderScope}, runID: 7}
			s.cls.Category = string(model.CategoryProtect)
			exec := func(ctx context.Context, _ callbacks.Handler) (runner.Outcome, error) {
				a.recordKnowledgeLoad(ctx, s, tt.st, knowledge.Situation{}, picks)
				return runner.Outcome{Status: model.RunCompleted}, nil
			}
			if tt.resume {
				_, err = r.Resume(context.Background(), loaderScope, agent.ResumeRequest{RunID: 7}, exec)
			} else {
				_, err = r.Run(context.Background(), loaderScope, 7, exec)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(rec.strategies, tt.want) {
				t.Fatalf("SetLoaderStrategy %v, want %v", rec.strategies, tt.want)
			}
			if len(rec.items) != 1 || rec.items[0][0].Strategy != tt.st.Version() {
				t.Fatalf("ReplaceKBLoadItems %+v, want one batch of %s", rec.items, tt.st.Version())
			}
		})
	}
}

// P1-2 运行时兜底：必加规则在本次快照中一个条目都匹配不到时打 Warn（不报错）；全部匹配时不告警。
func TestWarnUnmatchedRules(t *testing.T) {
	snap, err := knowledge.NewStore(knowledge.NewRepoReader("../../..")).Open(context.Background(), knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	protect := string(model.CategoryProtect)
	for _, tt := range []struct {
		name string
		rule knowledge.MustRule
		warn bool
	}{
		{name: "typo kb_id", rule: knowledge.MustRule{KBID: "common/safety/phl"}, warn: true},
		{name: "typo file", rule: knowledge.MustRule{File: "commn/safety.md"}, warn: true},
		{name: "matched", rule: knowledge.MustRule{KBID: "common/safety/phi"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := mustStrategy(knowledge.StrategySpec{Version: model.LoaderStrategyV1, Limit: 8,
				Order: []knowledge.SortKey{knowledge.SortAuthority}, MustInclude: map[string][]knowledge.MustRule{protect: {tt.rule}}})
			var buf bytes.Buffer
			a := &Agent{log: slog.New(slog.NewTextHandler(&buf, nil))}
			a.warnUnmatchedRules(context.Background(), &state{env: &env{snap: snap}, runID: 7}, st, protect)
			got := strings.Contains(buf.String(), "must_include rules match no knowledge entry")
			if got != tt.warn {
				t.Fatalf("warned = %v, want %v; log: %s", got, tt.warn, buf.String())
			}
			if tt.warn && !strings.Contains(buf.String(), tt.rule.String()) {
				t.Fatalf("warning does not name rule %s: %s", tt.rule, buf.String())
			}
		})
	}
}
