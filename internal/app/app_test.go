package app

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository/rediskey"
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
	return Deps{Clock: fixedClock{}, Logger: logger, DB: &gorm.DB{}, Redis: rdb, Keys: keys,
		RawStore: nopRaw{}, Agent: refuseAgent{}, Tools: nopTools{}}
}

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
				a.Runs == nil || a.Advice == nil || a.Drafts == nil || a.RunLocker == nil || a.Ask == nil) {
				t.Fatalf("incomplete app: %+v", a)
			}
		})
	}
}

func TestBuild_ListsAllMissing(t *testing.T) {
	_, err := Build(Deps{})
	for _, want := range []error{ErrMissingAgent, ErrMissingClock, ErrMissingDB, ErrMissingRawStore, ErrMissingTools} {
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
