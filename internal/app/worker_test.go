package app

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agri-steward/internal/config"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

func loopNames(loops []Loop) []string {
	out := make([]string, 0, len(loops))
	for _, l := range loops {
		out = append(out, l.Name)
	}
	return out
}

// TestServerWorkerLoops（§6.5 功能任务 1）：server 只跑开关订阅，不再起 outbox 轮询 / 扫描 / 分区维护；
// 这些全部在 worker 里，且 worker 也订阅开关。
func TestServerWorkerLoops(t *testing.T) {
	a, err := Build(fullDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	server := loopNames(a.ServerLoops())
	if !slices.Equal(server, []string{LoopFlags}) {
		t.Fatalf("server loops = %v, want [%s]", server, LoopFlags)
	}
	for _, banned := range []string{LoopOutbox, LoopScanner, LoopPartitions} {
		if slices.Contains(server, banned) {
			t.Fatalf("server must not run %q: %v", banned, server)
		}
	}
	wl, err := a.WorkerLoops()
	if err != nil {
		t.Fatal(err)
	}
	worker := loopNames(wl)
	for _, want := range []string{LoopOutbox, LoopScanner, LoopPartitions, LoopFlags} {
		if !slices.Contains(worker, want) {
			t.Fatalf("worker loops %v missing %q", worker, want)
		}
	}
	for _, l := range append(a.ServerLoops(), wl...) {
		if l.Run == nil {
			t.Fatalf("loop %q has nil Run", l.Name)
		}
	}
}

func TestOutboxHandlers_Topics(t *testing.T) {
	a, err := Build(fullDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	h, err := a.OutboxHandlers()
	if err != nil {
		t.Fatal(err)
	}
	for _, topic := range []string{repository.TopicOrderDispatch, repository.TopicNotifySend} {
		if h[topic] == nil {
			t.Fatalf("handler table missing %q: %v", topic, h)
		}
	}
	if _, err := a.NewOutboxPoller(); err != nil {
		t.Fatalf("NewOutboxPoller: %v", err)
	}
}

func nopHandler(context.Context, repository.OutboxMessage) error { return nil }

func TestMergeOutboxHandlers(t *testing.T) {
	base := map[string]service.OutboxHandler{"a": nopHandler}
	tests := []struct {
		name    string
		tables  []map[string]service.OutboxHandler
		wantErr string
		want    []string
	}{
		{name: "merged", tables: []map[string]service.OutboxHandler{base, {"b": nopHandler}, nil}, want: []string{"a", "b"}},
		{name: "duplicate topic fails", tables: []map[string]service.OutboxHandler{base, {"a": nopHandler}},
			wantErr: "registered twice"},
		{name: "nil handler fails", tables: []map[string]service.OutboxHandler{base, {"b": nil}}, wantErr: "no handler"},
		{name: "empty topic fails", tables: []map[string]service.OutboxHandler{{"": nopHandler}}, wantErr: "no handler"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MergeOutboxHandlers(tt.tables...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, topic := range tt.want {
				if got[topic] == nil {
					t.Fatalf("missing %q in %v", topic, got)
				}
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
		})
	}
}

func TestBuild_DispatchConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Dispatch
		want    config.Dispatch
		wantErr bool
	}{
		{name: "zero takes default", want: config.DefaultDispatch()},
		{name: "custom kept", cfg: config.Dispatch{MaxAttempts: 3, ReceiptTimeout: time.Minute, DeviceWaitMax: time.Hour},
			want: config.Dispatch{MaxAttempts: 3, ReceiptTimeout: time.Minute, DeviceWaitMax: time.Hour}},
		{name: "zero attempts fails", cfg: config.Dispatch{ReceiptTimeout: time.Minute, DeviceWaitMax: time.Hour}, wantErr: true},
		{name: "zero timeout fails", cfg: config.Dispatch{MaxAttempts: 2, DeviceWaitMax: time.Hour}, wantErr: true},
		{name: "zero device wait fails", cfg: config.Dispatch{MaxAttempts: 2, ReceiptTimeout: time.Minute}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fullDeps(t)
			d.Dispatch = tt.cfg
			a, err := Build(d)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if a.DispatchConfig != tt.want {
				t.Fatalf("DispatchConfig = %+v, want %+v", a.DispatchConfig, tt.want)
			}
		})
	}
}

// TestStartLoops：wait 阻塞到全部任务在 ctx 取消后返回。
func TestStartLoops(t *testing.T) {
	var stopped atomic.Int32
	run := func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond)
		stopped.Add(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait := StartLoops(ctx, []Loop{{Name: "x", Run: run}, {Name: "y", Run: run}})
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("wait returned before cancel")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return")
	}
	if got := stopped.Load(); got != 2 {
		t.Fatalf("stopped = %d, want 2", got)
	}
}
