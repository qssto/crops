package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/repository"
)

// memBacklog 是 BacklogStore 桩。
type memBacklog struct {
	rows   []repository.FarmCount
	err    error
	gotSys repository.SystemScope
	calls  int
}

func (m *memBacklog) OpenOrderTickets(_ context.Context, sys repository.SystemScope) ([]repository.FarmCount, error) {
	m.gotSys = sys
	m.calls++
	return m.rows, m.err
}

// 审批积压 gauge：按农场整体替换（消失的农场归零移除），查询失败保留上一轮的值并返回错误（应失败用例）。
func TestBacklogRefresher_RefreshOnce(t *testing.T) {
	obs := NewMetricsObserver()
	store := &memBacklog{rows: []repository.FarmCount{{TenantID: 1, FarmID: 1, N: 3}, {TenantID: 1, FarmID: 2, N: 1}}}
	b, err := NewBacklogRefresher(store, obs, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.interval != DefaultScanInterval {
		t.Fatalf("interval = %v, want default", b.interval)
	}
	if err := b.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := metricsText(t, obs)
	for _, want := range []string{MetricApprovalBacklog + `{farm_id="1"} 3`, MetricApprovalBacklog + `{farm_id="2"} 1`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if store.gotSys != scanSystem {
		t.Fatalf("sys = %v", store.gotSys)
	}

	store.rows = []repository.FarmCount{{TenantID: 1, FarmID: 1, N: 2}}
	if err := b.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = metricsText(t, obs)
	if !strings.Contains(got, MetricApprovalBacklog+`{farm_id="1"} 2`) || strings.Contains(got, `{farm_id="2"}`) {
		t.Fatalf("after replace:\n%s", got)
	}

	store.err = errors.New("db down")
	if err := b.RefreshOnce(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(metricsText(t, obs), MetricApprovalBacklog+`{farm_id="1"} 2`) {
		t.Fatal("failed refresh must keep previous values")
	}
}

// Run 启动即刷新，ctx 取消后返回。
func TestBacklogRefresher_Run(t *testing.T) {
	store := &memBacklog{}
	b, err := NewBacklogRefresher(store, NopObserver{}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.Run(ctx)
	if store.calls != 1 {
		t.Fatalf("calls = %d, want 1 immediate refresh", store.calls)
	}
}

func TestNewBacklogRefresher_RequiresDeps(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, mk := range map[string]func() (*BacklogRefresher, error){
		"store":  func() (*BacklogRefresher, error) { return NewBacklogRefresher(nil, NopObserver{}, log, 0) },
		"obs":    func() (*BacklogRefresher, error) { return NewBacklogRefresher(&memBacklog{}, nil, log, 0) },
		"logger": func() (*BacklogRefresher, error) { return NewBacklogRefresher(&memBacklog{}, NopObserver{}, nil, 0) },
	} {
		if _, err := mk(); err == nil {
			t.Fatalf("missing %s: want error", name)
		}
	}
}

// memFarms 是 FarmLister 桩。
type memFarms struct {
	ids    []uint64
	err    error
	gotSys repository.SystemScope
	calls  int
}

func (m *memFarms) KnownFarms(_ context.Context, sys repository.SystemScope) ([]uint64, error) {
	m.gotSys = sys
	m.calls++
	return m.ids, m.err
}

// 审查修复 P0：指标预置器按已登记农场预置 0 值序列（系统级凭证）；读失败返回错误、不预置；Run 启动即预置一次。
func TestMetricsPreseeder(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	obs := NewMetricsObserver()
	farms := &memFarms{ids: []uint64{5, 6}}
	p, err := NewMetricsPreseeder(farms, obs, log, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.interval != DefaultScanInterval {
		t.Fatalf("interval = %v, want default", p.interval)
	}
	if err := p.PreseedOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := metricsText(t, obs)
	for _, want := range []string{MetricAlerts + `{alert="advice_no_citation",level="P0",farm_id="5"} 0`,
		MetricAlerts + `{alert="advice_no_citation",level="P0",farm_id="6"} 0`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if farms.gotSys != scanSystem {
		t.Fatalf("sys = %v", farms.gotSys)
	}

	farms.ids, farms.err = []uint64{8}, errors.New("db down")
	if err := p.PreseedOnce(context.Background()); err == nil {
		t.Fatal("want error when KnownFarms fails")
	}
	if strings.Contains(metricsText(t, obs), `farm_id="8"`) {
		t.Fatal("preseeded despite KnownFarms error")
	}

	farms.err, farms.calls = nil, 0
	slow, err := NewMetricsPreseeder(farms, obs, log, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slow.Run(ctx)
	if farms.calls != 1 || !strings.Contains(metricsText(t, obs), `farm_id="8"`) {
		t.Fatalf("Run calls = %d, want 1 immediate preseed", farms.calls)
	}
}

func TestNewMetricsPreseeder_RequiresDeps(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	obs := NewMetricsObserver()
	for name, mk := range map[string]func() (*MetricsPreseeder, error){
		"farms":  func() (*MetricsPreseeder, error) { return NewMetricsPreseeder(nil, obs, log, 0) },
		"obs":    func() (*MetricsPreseeder, error) { return NewMetricsPreseeder(&memFarms{}, nil, log, 0) },
		"logger": func() (*MetricsPreseeder, error) { return NewMetricsPreseeder(&memFarms{}, obs, nil, 0) },
	} {
		if _, err := mk(); err == nil {
			t.Fatalf("missing %s: want error", name)
		}
	}
}
