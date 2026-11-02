package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
)

// memDenialStore 记录写入的审计行，以及写入当时 ctx 的状态（Deny 返回后写入 ctx 即被取消）。
type memDenialStore struct {
	recs      []model.AuditRecord
	err       error
	ctxErrs   []error
	deadlines []time.Duration // 写入时距 deadline 的剩余时长；无 deadline 记 -1
}

func (s *memDenialStore) Append(ctx context.Context, r model.AuditRecord) error {
	s.recs = append(s.recs, r)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	s.deadlines = append(s.deadlines, left)
	return s.err
}

// blockingDenialStore 模拟库挂起：阻塞到 ctx 结束并返回其错误。
type blockingDenialStore struct{}

func (blockingDenialStore) Append(ctx context.Context, _ model.AuditRecord) error {
	<-ctx.Done()
	return ctx.Err()
}

type countDenied map[string]int

func (c countDenied) IncDenied(code string) { c[code]++ }

func TestDenialAuditor_Deny(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		d          AccessDenial
		wantTenant uint64
		wantFarm   uint64
	}{
		{name: "scope denied at platform (0,0)", d: AccessDenial{UserID: 1001, FarmID: 2, Code: DenyScope,
			Target: "farm:2", Route: "GET /api/v1/farms/:farm_id/ping", RequestID: "r1"}},
		{name: "tenant mismatch carries owner tenant", d: AccessDenial{UserID: 1001, FarmID: 3, OwnerTenantID: 2,
			Code: DenyScope, Target: "farm:3"}},
		{name: "role insufficient at farm", d: AccessDenial{Scope: model.Scope{TenantID: 1, FarmID: 1, UserID: 1003},
			UserID: 1003, FarmID: 1, Code: DenyRole, Op: OpRawRead, Target: "raw:7"}, wantTenant: 1, wantFarm: 1},
		{name: "resource not in scope", d: AccessDenial{Scope: model.Scope{TenantID: 1, FarmID: 2, UserID: 2001},
			UserID: 2001, FarmID: 2, Code: DenyResource, Op: OpAdviceRead, Target: "run:5"}, wantTenant: 1, wantFarm: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, counter := &memDenialStore{}, countDenied{}
			var logBuf bytes.Buffer
			a, err := NewDenialAuditor(store, fixedClock{now}, slog.New(slog.NewJSONHandler(&logBuf, nil)), counter)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // 请求已断开也要写审计
			a.Deny(ctx, tt.d)
			if len(store.recs) != 1 {
				t.Fatalf("records = %d", len(store.recs))
			}
			r := store.recs[0]
			if r.TenantID != tt.wantTenant || r.FarmID != tt.wantFarm || r.Action != string(model.AuditCrossFarmDenied) ||
				r.Result != model.AuditResultDenied || r.Target != tt.d.Target || r.Actor.ID != tt.d.UserID ||
				r.Actor.Type != model.ActorUser || !r.CreatedAt.Equal(now) || r.RunID != nil {
				t.Fatalf("record = %+v", r)
			}
			var sum denialSummary
			if err := json.Unmarshal(r.Summary, &sum); err != nil {
				t.Fatal(err)
			}
			if sum.Code != tt.d.Code || sum.Op != tt.d.Op || sum.FarmID != tt.d.FarmID || sum.RequestID != tt.d.RequestID ||
				sum.OwnerTenantID != tt.d.OwnerTenantID {
				t.Fatalf("summary = %+v", sum)
			}
			// 审计写入脱离请求取消，但带 denialWriteTimeout 上限
			if store.ctxErrs[0] != nil {
				t.Fatalf("write ctx already done: %v", store.ctxErrs[0])
			}
			if left := store.deadlines[0]; left <= 0 || left > denialWriteTimeout {
				t.Fatalf("write ctx deadline left = %v, want (0, %s]", left, denialWriteTimeout)
			}
			if counter[tt.d.Code] != 1 {
				t.Fatalf("counter = %v", counter)
			}
			if !strings.Contains(logBuf.String(), DeniedMetric) || !strings.Contains(logBuf.String(), tt.d.Code) {
				t.Fatalf("log = %s", logBuf.String())
			}
		})
	}
}

// 写审计失败只记错误日志，不 panic；counter 可空。
func TestDenialAuditor_StoreError(t *testing.T) {
	var logBuf bytes.Buffer
	a, err := NewDenialAuditor(&memDenialStore{err: errors.New("db down")}, fixedClock{},
		slog.New(slog.NewJSONHandler(&logBuf, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Deny(context.Background(), AccessDenial{UserID: 1, Code: DenyScope})
	if !strings.Contains(logBuf.String(), "write audit") || !strings.Contains(logBuf.String(), "db down") {
		t.Fatalf("log = %s", logBuf.String())
	}
	// 应失败用例：库挂起时 Deny 在超时后返回并记错误日志（对外 403 不变）
	logBuf.Reset()
	hung, err := NewDenialAuditor(blockingDenialStore{}, fixedClock{}, slog.New(slog.NewJSONHandler(&logBuf, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { hung.Deny(context.Background(), AccessDenial{UserID: 1, Code: DenyScope}); close(done) }()
	select {
	case <-done:
	case <-time.After(denialWriteTimeout + 3*time.Second):
		t.Fatal("Deny did not return after write timeout")
	}
	if !strings.Contains(logBuf.String(), "write audit") || !strings.Contains(logBuf.String(), "deadline exceeded") {
		t.Fatalf("log = %s", logBuf.String())
	}
	if _, err := NewDenialAuditor(nil, fixedClock{}, nil, nil); err == nil {
		t.Fatal("nil store must fail")
	}
	if _, err := NewDenialAuditor(&memDenialStore{}, nil, nil, nil); err == nil {
		t.Fatal("nil clock must fail")
	}
}
