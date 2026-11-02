package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/model"
	"agri-steward/internal/repository/rediskey"
	"agri-steward/internal/service"
)

type refuseAgent struct{}

func (refuseAgent) Ask(context.Context, agent.Question) (agent.Answer, error) {
	return agent.Answer{Outcome: model.OutcomeRefused}, nil
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC) }

type nopRaw struct{}

func (nopRaw) Put(context.Context, model.RawPut) (model.RawObject, error) {
	return model.RawObject{}, nil
}

type nopTools struct{}

func (nopTools) Call(context.Context, model.Scope, agent.ToolCall) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func fullDeps(t *testing.T) Deps {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	logger, err := NewLogger(&bytes.Buffer{}, "info")
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"}) // 不建连，Build 不做 IO
	t.Cleanup(func() {
		if err := rdb.Close(); err != nil {
			t.Error(err)
		}
	})
	perms, err := service.LoadPermissionMatrix("../../policy/permission_matrix.yaml")
	if err != nil {
		t.Fatal(err)
	}
	texts, err := service.LoadNotifyTexts("../../configs/texts/notify.zh-CN.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Clock: fixedClock{}, Logger: logger, DB: &gorm.DB{}, Redis: rdb, Keys: keys,
		RawStore: nopRaw{}, Agent: refuseAgent{}, Tools: nopTools{}, Risk: nopRisk{}, Constraints: passReviewer{},
		Permissions: perms, NotifyTexts: texts}
}

// passReviewer 放行全部作业单（装配测试不关心约束判定）；Registry 供主链路注入约束注册表（P1-L3 审查 15）。
type passReviewer struct{}

var passRegistry = constraint.Default()

func (passReviewer) Review(o model.WorkOrder) ([]model.ConstraintCheck, []string) {
	return o.ConstraintChecks, nil
}

func (passReviewer) Registry() *constraint.Registry { return passRegistry }

// bareReviewer 不提供注册表（只实现 service.ConstraintReviewer）。
type bareReviewer struct{}

func (bareReviewer) Review(o model.WorkOrder) ([]model.ConstraintCheck, []string) {
	return o.ConstraintChecks, nil
}

// TestConstraintRegistry_SharedWithReviewer（P1-L3 审查 15）：主链路取开票前 Reviewer 的同一注册表实例；
// 拿不到注册表时为 nil（graph.New 报缺依赖），不另建一份。
func TestConstraintRegistry_SharedWithReviewer(t *testing.T) {
	reg := constraint.Default()
	if got := constraintRegistry(constraint.NewReviewer(reg, nil)); got != reg {
		t.Fatalf("registry %p, want reviewer's %p", got, reg)
	}
	if got := constraintRegistry(bareReviewer{}); got != nil {
		t.Fatalf("bare reviewer registry = %p, want nil", got)
	}
}

type nopRisk struct{}

func (nopRisk) Assess(policy.RiskInput) (policy.Risk, error) { return policy.Risk{}, nil }
func (nopRisk) TTL(model.RiskLevel) (time.Duration, error)   { return time.Hour, nil }

func TestBuild(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(d *Deps)
		wantErr error
	}{
		{name: "ok", mutate: func(*Deps) {}},
		{name: "missing agent", mutate: func(d *Deps) { d.Agent = nil }, wantErr: ErrMissingAgent},
		{name: "missing clock", mutate: func(d *Deps) { d.Clock = nil }, wantErr: ErrMissingClock},
		{name: "missing db", mutate: func(d *Deps) { d.DB = nil }, wantErr: ErrMissingDB},
		{name: "missing redis", mutate: func(d *Deps) { d.Redis = nil }, wantErr: ErrMissingRedis},
		{name: "zero keys", mutate: func(d *Deps) { d.Keys = rediskey.Keys{} }, wantErr: ErrMissingKeys},
		{name: "missing raw store", mutate: func(d *Deps) { d.RawStore = nil }, wantErr: ErrMissingRawStore},
		{name: "missing logger", mutate: func(d *Deps) { d.Logger = nil }, wantErr: ErrMissingLogger},
		{name: "missing tools", mutate: func(d *Deps) { d.Tools = nil }, wantErr: ErrMissingTools},
		{name: "missing risk", mutate: func(d *Deps) { d.Risk = nil }, wantErr: ErrMissingRisk},
		{name: "missing permissions", mutate: func(d *Deps) { d.Permissions = nil }, wantErr: ErrMissingPermissions},
		{name: "missing constraints", mutate: func(d *Deps) { d.Constraints = nil }, wantErr: ErrMissingConstraints},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fullDeps(t)
			tt.mutate(&d)
			a, err := Build(d)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && (a.Agent == nil || a.Tools == nil || a.WorkOrders == nil || a.Locker == nil ||
				a.Runs == nil || a.Advice == nil || a.Drafts == nil || a.RunLocker == nil || a.Ask == nil ||
				a.Approvals == nil || a.Events == nil) {
				t.Fatalf("incomplete app: %+v", a)
			}
		})
	}
}

func TestBuild_ListsAllMissing(t *testing.T) {
	_, err := Build(Deps{})
	for _, want := range []error{ErrMissingAgent, ErrMissingClock, ErrMissingDB, ErrMissingRawStore, ErrMissingTools, ErrMissingRisk, ErrMissingPermissions,
		ErrMissingConstraints} {
		if !errors.Is(err, want) {
			t.Errorf("err %v does not include %v", err, want)
		}
	}
}

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLogger(&buf, "warn")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hidden")
	l.Warn("shown", "k", 1)
	if got := buf.String(); bytes.Contains([]byte(got), []byte("hidden")) || !bytes.Contains([]byte(got), []byte(`"msg":"shown"`)) {
		t.Fatalf("log output = %s", got)
	}
	if _, err := NewLogger(&buf, "loud"); err == nil {
		t.Fatal("want error for bad level")
	}
}

// 包装函数插在脱敏层内侧：tap 看到的已是掩码后的记录。
func TestNewTappedLogger_TapSeesMaskedRecords(t *testing.T) {
	var out, tapped bytes.Buffer
	tap := func(next slog.Handler) slog.Handler {
		return teeHandler{next: next, copy: slog.NewJSONHandler(&tapped, nil)}
	}
	l, err := NewTappedLogger(&out, "info", tap)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("ask", "q", "电话13800001234")
	for name, got := range map[string]string{"out": out.String(), "tap": tapped.String()} {
		if strings.Contains(got, "13800001234") || !strings.Contains(got, "138****1234") {
			t.Fatalf("%s = %s, want masked phone", name, got)
		}
	}
}

type teeHandler struct{ next, copy slog.Handler }

func (h teeHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }
func (h teeHandler) Handle(ctx context.Context, r slog.Record) error {
	return errors.Join(h.copy.Handle(ctx, r.Clone()), h.next.Handle(ctx, r))
}
func (h teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return teeHandler{next: h.next.WithAttrs(as), copy: h.copy.WithAttrs(as)}
}
func (h teeHandler) WithGroup(n string) slog.Handler {
	return teeHandler{next: h.next.WithGroup(n), copy: h.copy.WithGroup(n)}
}
