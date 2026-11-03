package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/model"
)

// memAppender 是 AuditAppender 桩：fail 非空时写失败；block 为 true 时阻塞到 ctx 结束；panics 为 true 时 panic。
type memAppender struct {
	mu     sync.Mutex
	recs   []model.AuditRecord
	fail   error
	block  bool
	panics bool
}

func (m *memAppender) Append(ctx context.Context, r model.AuditRecord) error {
	if m.panics {
		panic("audit driver bug")
	}
	if m.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if m.fail != nil {
		return m.fail
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, r)
	return nil
}

func newTestSink(t *testing.T, audit AuditAppender) (*AuditAlertSink, *MetricsObserver, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	obs := NewMetricsObserver()
	s, err := NewAuditAlertSink(audit, fixedClock{decideNow}, slog.New(slog.NewTextHandler(&buf, nil)), obs)
	if err != nil {
		t.Fatal(err)
	}
	return s, obs, &buf
}

func metricsText(t *testing.T, obs *MetricsObserver) string {
	t.Helper()
	var b strings.Builder
	if err := obs.Registry().WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// AuditAlertSink：计 as_alert_total、写 ALERT.{name} 审计（target order: 优先于 run:，P0→FAILED、P1→DENIED，summary 无 PII 字段）、
// 打 Error 日志。
func TestAuditAlertSink_Fire(t *testing.T) {
	tests := []struct {
		name       string
		alert      model.Alert
		wantTarget string
		wantResult string
		wantRun    bool
	}{
		{name: "order target P0", alert: model.Alert{Name: model.AlertPreApprovalDevice, Level: model.AlertP0,
			Scope: model.Scope{TenantID: 1, FarmID: 2}, RunID: 5, OrderID: 9, Detail: "ticket 7 not approved"},
			wantTarget: "order:9", wantResult: model.AuditResultFailed, wantRun: true},
		{name: "run target P0", alert: model.Alert{Name: model.AlertAdviceNoCitation, Level: model.AlertP0,
			Scope: model.Scope{TenantID: 1, FarmID: 2}, RunID: 5}, wantTarget: "run:5", wantResult: model.AuditResultFailed, wantRun: true},
		{name: "P1 denied no ids", alert: model.Alert{Name: model.AlertFlagChanged, Level: model.AlertP1,
			Scope: model.Scope{TenantID: 1, FarmID: 2}}, wantTarget: "", wantResult: model.AuditResultDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			audit := &memAppender{}
			s, obs, logs := newTestSink(t, audit)
			s.Fire(context.Background(), tt.alert)
			if len(audit.recs) != 1 {
				t.Fatalf("audit rows = %d, want 1", len(audit.recs))
			}
			r := audit.recs[0]
			if r.Action != string(tt.alert.Name.AuditAction()) || r.Target != tt.wantTarget || r.Result != tt.wantResult ||
				r.TenantID != 1 || r.FarmID != 2 || r.Actor.Type != model.ActorSystem || !r.CreatedAt.Equal(decideNow) {
				t.Fatalf("record = %+v", r)
			}
			if (r.RunID != nil) != tt.wantRun || (tt.wantRun && *r.RunID != tt.alert.RunID) {
				t.Fatalf("run_id = %v, want %d", r.RunID, tt.alert.RunID)
			}
			var sum map[string]any
			if err := json.Unmarshal(r.Summary, &sum); err != nil {
				t.Fatal(err)
			}
			for k := range sum {
				switch k {
				case "alert", "level", "run_id", "order_id", "detail":
				default:
					t.Fatalf("summary has unexpected key %q: %s", k, r.Summary)
				}
			}
			if sum["alert"] != string(tt.alert.Name) || sum["level"] != string(tt.alert.Level) {
				t.Fatalf("summary = %s", r.Summary)
			}
			want := MetricAlerts + `{alert="` + string(tt.alert.Name) + `",level="` + string(tt.alert.Level) + `",farm_id="2"} 1`
			if got := metricsText(t, obs); !strings.Contains(got, want) {
				t.Fatalf("metrics missing %q:\n%s", want, got)
			}
			if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "alert="+string(tt.alert.Name)) {
				t.Fatalf("log = %s", logs.String())
			}
		})
	}
}

// detail 截断到 alertDetailMax 个字符，防止 summary 过长。
func TestAuditAlertSink_DetailTruncated(t *testing.T) {
	audit := &memAppender{}
	s, _, _ := newTestSink(t, audit)
	s.Fire(context.Background(), model.Alert{Name: model.AlertModelActuate, Level: model.AlertP0, RunID: 1,
		Detail: strings.Repeat("长", alertDetailMax+50)})
	var sum alertSummary
	if err := json.Unmarshal(audit.recs[0].Summary, &sum); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(sum.Detail)); n > alertDetailMax {
		t.Fatalf("detail runes = %d, want <= %d", n, alertDetailMax)
	}
}

// 应失败用例：审计写失败 / 阻塞 / panic 时 Fire 不 panic、不阻塞（有界返回），指标照计，失败记 Error 日志。
func TestAuditAlertSink_AuditFailureDoesNotBreakCaller(t *testing.T) {
	tests := []struct {
		name  string
		audit *memAppender
		want  string
	}{
		{name: "write error", audit: &memAppender{fail: errors.New("db down")}, want: "write audit failed"},
		{name: "write blocks", audit: &memAppender{block: true}, want: "write audit failed"},
		{name: "appender panics", audit: &memAppender{panics: true}, want: "panic recovered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, obs, logs := newTestSink(t, tt.audit)
			if s.timeout != DefaultAlertWriteTimeout || DefaultAlertWriteTimeout > 500*time.Millisecond {
				t.Fatalf("default write timeout = %v, want %v (<= 500ms)", s.timeout, DefaultAlertWriteTimeout)
			}
			done := make(chan struct{})
			start := time.Now()
			go func() {
				defer close(done)
				s.Fire(context.Background(), model.Alert{Name: model.AlertAuditWriteFailed, Level: model.AlertP0,
					Scope: model.Scope{TenantID: 1, FarmID: 3}, RunID: 4})
			}()
			select {
			case <-done:
			case <-time.After(s.timeout + 2*time.Second):
				t.Fatal("Fire blocked past the audit write timeout")
			}
			if elapsed := time.Since(start); elapsed > s.timeout+time.Second {
				t.Fatalf("Fire took %v", elapsed)
			}
			if !strings.Contains(logs.String(), tt.want) {
				t.Fatalf("log = %s, want %q", logs.String(), tt.want)
			}
			if got := metricsText(t, obs); !strings.Contains(got, `alert="audit_write_failed",level="P0",farm_id="3"} 1`) {
				t.Fatalf("alert counter not incremented:\n%s", got)
			}
		})
	}
}

// 调用方 ctx 已取消时审计仍写入（独立短写，不随业务取消丢告警）。
func TestAuditAlertSink_CanceledCallerStillAudits(t *testing.T) {
	audit := &memAppender{}
	s, _, _ := newTestSink(t, audit)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Fire(ctx, model.Alert{Name: model.AlertModelActuate, Level: model.AlertP0, RunID: 2})
	if len(audit.recs) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit.recs))
	}
}

// chainAppender 模拟按 scope 的审计链：missing 里的 (tenant, farm) 写入即失败（head 缺失），platformFail 时 (0,0) 也失败。
type chainAppender struct {
	missing      map[[2]uint64]bool
	platformFail bool
	calls        []model.AuditRecord
	recs         []model.AuditRecord
}

var errHeadMissing = errors.New("audit chain head missing")

func (c *chainAppender) Append(_ context.Context, r model.AuditRecord) error {
	c.calls = append(c.calls, r)
	if c.missing[[2]uint64{r.TenantID, r.FarmID}] || (c.platformFail && r.TenantID == 0 && r.FarmID == 0) {
		return errHeadMissing
	}
	c.recs = append(c.recs, r)
	return nil
}

// L7-A 合并语义：scope 写失败（如 head 缺失）时回退写平台链 (0,0)，summary 带 origin；平台链本身失败只记日志，
// 不递归、不 panic；阻塞超时不回退；scope 已是 (0,0) 不重复写。
func TestAuditAlertSink_PlatformChainFallback(t *testing.T) {
	farm := model.Scope{TenantID: 1, FarmID: 3}
	tests := []struct {
		name       string
		audit      *chainAppender
		scope      model.Scope
		wantCalls  int
		wantScope  [2]uint64 // 落库行的 (tenant, farm)；wantRows=0 时不看
		wantRows   int
		wantOrigin bool
		wantLog    string
	}{
		{name: "scope ok no fallback", audit: &chainAppender{}, scope: farm, wantCalls: 1, wantRows: 1,
			wantScope: [2]uint64{1, 3}},
		{name: "head missing falls back to platform", audit: &chainAppender{missing: map[[2]uint64]bool{{1, 3}: true}},
			scope: farm, wantCalls: 2, wantRows: 1, wantScope: [2]uint64{0, 0}, wantOrigin: true,
			wantLog: "audit written to platform chain"},
		{name: "platform also fails only logs", audit: &chainAppender{missing: map[[2]uint64]bool{{1, 3}: true},
			platformFail: true}, scope: farm, wantCalls: 2, wantLog: "platform-chain fallback failed"},
		{name: "platform scope not retried", audit: &chainAppender{platformFail: true}, scope: model.Scope{},
			wantCalls: 1, wantLog: "write audit failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, obs, logs := newTestSink(t, tt.audit)
			s.Fire(context.Background(), model.Alert{Name: model.AlertAuditWriteFailed, Level: model.AlertP0,
				Scope: tt.scope, RunID: 4, Detail: "chain head missing"})
			if len(tt.audit.calls) != tt.wantCalls || len(tt.audit.recs) != tt.wantRows {
				t.Fatalf("calls = %d rows = %d, want %d / %d", len(tt.audit.calls), len(tt.audit.recs), tt.wantCalls, tt.wantRows)
			}
			if tt.wantLog != "" && !strings.Contains(logs.String(), tt.wantLog) {
				t.Fatalf("log = %s, want %q", logs.String(), tt.wantLog)
			}
			if got := metricsText(t, obs); strings.Count(got, `alert="audit_write_failed",level="P0",farm_id="`+
				strconv.FormatUint(tt.scope.FarmID, 10)+`"} 1`) != 1 {
				t.Fatalf("alert must be counted exactly once:\n%s", got)
			}
			if tt.wantRows == 0 {
				return
			}
			r := tt.audit.recs[0]
			if [2]uint64{r.TenantID, r.FarmID} != tt.wantScope || r.Action != string(model.AlertAuditWriteFailed.AuditAction()) {
				t.Fatalf("record = %+v", r)
			}
			var sum alertSummary
			if err := json.Unmarshal(r.Summary, &sum); err != nil {
				t.Fatal(err)
			}
			if (sum.Origin != nil) != tt.wantOrigin {
				t.Fatalf("origin = %+v, want present %v", sum.Origin, tt.wantOrigin)
			}
			if tt.wantOrigin {
				if *sum.Origin != (alertOrigin{TenantID: 1, FarmID: 3}) || r.RunID != nil || sum.RunID != 4 {
					t.Fatalf("fallback row origin = %+v run_id col = %v summary run = %d", sum.Origin, r.RunID, sum.RunID)
				}
			}
		})
	}
}

// 应失败用例：阻塞到截止的写不回退（只写一次），总耗时 ≤ timeout + 余量。
func TestAuditAlertSink_TimeoutDoesNotFallBack(t *testing.T) {
	audit := &countingBlockAppender{}
	s, _, logs := newTestSink(t, audit)
	if err := s.SetWriteTimeout(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Fire(context.Background(), model.Alert{Name: model.AlertModelActuate, Level: model.AlertP0,
		Scope: model.Scope{TenantID: 1, FarmID: 3}, RunID: 1})
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond+time.Second {
		t.Fatalf("Fire took %v", elapsed)
	}
	if audit.n != 1 || strings.Contains(logs.String(), "platform") {
		t.Fatalf("appends = %d, log = %s; want 1 and no fallback", audit.n, logs.String())
	}
}

// countingBlockAppender 每次写阻塞到 ctx 结束并计数。
type countingBlockAppender struct{ n int }

func (c *countingBlockAppender) Append(ctx context.Context, _ model.AuditRecord) error {
	c.n++
	<-ctx.Done()
	return ctx.Err()
}

func TestNewAuditAlertSink_RequiresDeps(t *testing.T) {
	if _, err := NewAuditAlertSink(nil, fixedClock{decideNow}, nil, nil); err == nil {
		t.Fatal("want error without audit appender")
	}
	if _, err := NewAuditAlertSink(&memAppender{}, nil, nil, nil); err == nil {
		t.Fatal("want error without clock")
	}
}

// 写超时可配，须在 (0, MaxAlertWriteTimeout] 内；配置后阻塞写按新上限返回。
func TestAuditAlertSink_SetWriteTimeout(t *testing.T) {
	tests := []struct {
		name    string
		d       time.Duration
		wantErr bool
	}{
		{name: "short", d: 50 * time.Millisecond},
		{name: "max", d: MaxAlertWriteTimeout},
		{name: "zero rejected", d: 0, wantErr: true},
		{name: "negative rejected", d: -time.Millisecond, wantErr: true},
		{name: "over max rejected", d: MaxAlertWriteTimeout + time.Millisecond, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := newTestSink(t, &memAppender{block: true})
			err := s.SetWriteTimeout(tt.d)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SetWriteTimeout(%v) err = %v, wantErr %v", tt.d, err, tt.wantErr)
			}
			if tt.wantErr {
				if s.timeout != DefaultAlertWriteTimeout {
					t.Fatalf("timeout changed to %v on rejected value", s.timeout)
				}
				return
			}
			start := time.Now()
			s.Fire(context.Background(), model.Alert{Name: model.AlertModelActuate, Level: model.AlertP0, RunID: 1})
			if elapsed := time.Since(start); elapsed < tt.d || elapsed > tt.d+time.Second {
				t.Fatalf("Fire took %v with timeout %v", elapsed, tt.d)
			}
		})
	}
}
