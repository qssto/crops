package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/model"
)

// forbiddenLabels 是高基数 / 下钻字段：一律不得作 label（下钻走 ALERT.* 审计）。
var forbiddenLabels = map[string]bool{"run_id": true, "order_id": true, "advice_id": true, "ticket_id": true, "user_id": true,
	"tenant_id": true, "device_job_id": true}

// enumLabels 是除 farm_id 外允许的 label：取值都经 enum() / 固定字面量收敛为有限集合。
var enumLabels = map[string]bool{"alert": true, "level": true, "mode": true, "result": true, "to": true, "reason": true,
	"decision": true, "code": true, "flag": true, "kind": true, "server": true, "status": true, "fault": true}

func checkLabelWhitelist(fams []MetricInfo) error {
	var errs []error
	for _, f := range fams {
		for _, l := range f.Labels {
			switch {
			case forbiddenLabels[l]:
				errs = append(errs, fmt.Errorf("%s: forbidden label %q", f.Name, l))
			case l != "farm_id" && !enumLabels[l]:
				errs = append(errs, fmt.Errorf("%s: label %q not in finite-enum whitelist", f.Name, l))
			}
		}
	}
	return errors.Join(errs...)
}

// label 白名单：注册表里不得出现 run_id / order_id 等 label，farm_id 之外只允许有限枚举（应失败用例：带 run_id 的族必须被拒）。
func TestMetrics_LabelWhitelist(t *testing.T) {
	tests := []struct {
		name    string
		fams    []MetricInfo
		wantErr string
	}{
		{name: "registered families", fams: NewMetricsObserver().Registry().Families()},
		{name: "run_id label rejected", fams: []MetricInfo{{Name: "as_x_total", Labels: []string{"farm_id", "run_id"}}},
			wantErr: `forbidden label "run_id"`},
		{name: "order_id label rejected", fams: []MetricInfo{{Name: "as_x_total", Labels: []string{"order_id"}}},
			wantErr: `forbidden label "order_id"`},
		{name: "free-form label rejected", fams: []MetricInfo{{Name: "as_x_total", Labels: []string{"detail"}}},
			wantErr: `"detail" not in finite-enum whitelist`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkLabelWhitelist(tt.fams)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// 不在枚举内的取值收敛为 other（label 基数有界）。
func TestMetricsObserver_UnknownValuesBecomeOther(t *testing.T) {
	o := NewMetricsObserver()
	o.AlertFired(model.Alert{Name: "made_up", Level: "P9", Scope: model.Scope{FarmID: 1}})
	o.ToolCalled("evil.tool", "WEIRD")
	o.IncDenied("NOPE")
	o.FlagChanged("unknown_flag")
	o.Dispatched(1, "LASER", DispatchOK)
	o.EventCommitted(model.RunEvent{FarmID: 1, Type: model.EventDegraded, Payload: json.RawMessage(`{"fault":"SOLAR_FLARE"}`)})
	o.EventCommitted(model.RunEvent{FarmID: 1, Type: model.EventOrderTransition,
		Payload: json.RawMessage(`{"to":"NOWHERE","reason_code":"WHY"}`)})
	got := metricsText(t, o)
	for _, want := range []string{
		MetricAlerts + `{alert="other",level="other",farm_id="1"} 1`,
		MetricMCPCalls + `{server="other",status="other"} 1`,
		DeniedMetric + `{code="other"} 1`,
		MetricFlagChanges + `{flag="other"} 1`,
		MetricDispatch + `{farm_id="1",mode="other",result="ok"} 1`,
		MetricDegraded + `{farm_id="1",fault="other"} 1`,
		MetricOrderTransitions + `{farm_id="1",to="other",reason="other"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, raw := range []string{"made_up", "evil", "WEIRD", "NOPE", "unknown_flag", "LASER", "SOLAR_FLARE", "NOWHERE", "WHY"} {
		if strings.Contains(got, `"`+raw+`"`) {
			t.Errorf("raw value %q leaked into labels", raw)
		}
	}
}

// 事件派生指标：作业迁移（含 EXPIRED / 复检退回原因）、DEGRADED 按故障码、LLM token；其他事件与坏 payload 不计。
func TestMetricsObserver_EventCommitted(t *testing.T) {
	o := NewMetricsObserver()
	ev := func(typ model.EventType, payload string) model.RunEvent {
		return model.RunEvent{FarmID: 2, Type: typ, Payload: json.RawMessage(payload)}
	}
	o.EventCommitted(ev(model.EventOrderTransition, `{"order_id":1,"from":"APPROVED","to":"EXPIRED","reason_code":"APPROVAL_EXPIRED"}`))
	o.EventCommitted(ev(model.EventOrderTransition, `{"order_id":1,"from":"PENDING_APPROVAL","to":"APPROVED"}`))
	o.EventCommitted(ev(model.EventDegraded, `{"fault":"LLM_UNAVAILABLE"}`))
	o.EventCommitted(ev(model.EventLLMCallEnd, `{"prompt_tokens":120,"completion_tokens":30}`))
	o.EventCommitted(ev(model.EventLLMCallEnd, `not json`))
	got := metricsText(t, o)
	for _, want := range []string{
		MetricOrderTransitions + `{farm_id="2",to="EXPIRED",reason="APPROVAL_EXPIRED"} 1`,
		MetricOrderTransitions + `{farm_id="2",to="APPROVED",reason="none"} 1`,
		MetricDegraded + `{farm_id="2",fault="LLM_UNAVAILABLE"} 1`,
		MetricLLMTokens + `{farm_id="2",kind="prompt"} 120`,
		MetricLLMTokens + `{farm_id="2",kind="completion"} 30`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

// 审批等待直方图：le 含等号、桶累计、+Inf 等于 count；负值按 0 计。
func TestMetricsObserver_ApprovalWaitHistogram(t *testing.T) {
	o := NewMetricsObserver()
	o.ApprovalDecided(1, model.TicketApproved, 60*time.Second)
	o.ApprovalDecided(1, model.TicketApproved, 10*time.Minute)
	o.ApprovalDecided(1, model.TicketApproved, 48*time.Hour)
	o.ApprovalDecided(1, model.TicketRejected, -time.Second)
	got := metricsText(t, o)
	lbl := `farm_id="1",decision="APPROVED"`
	for _, want := range []string{
		MetricApprovalWait + `_bucket{` + lbl + `,le="60"} 1`,
		MetricApprovalWait + `_bucket{` + lbl + `,le="300"} 1`,
		MetricApprovalWait + `_bucket{` + lbl + `,le="900"} 2`,
		MetricApprovalWait + `_bucket{` + lbl + `,le="86400"} 2`,
		MetricApprovalWait + `_bucket{` + lbl + `,le="+Inf"} 3`,
		MetricApprovalWait + `_count{` + lbl + `} 3`,
		MetricApprovalWait + `_sum{` + lbl + `} 173460`,
		MetricApprovalWait + `_bucket{farm_id="1",decision="REJECTED",le="60"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if !strings.Contains(got, "# TYPE "+MetricApprovalWait+" histogram") {
		t.Error("missing TYPE line")
	}
}

// /metrics 处理器：GET 返回 0.0.4 文本格式，POST 405；label 值转义。
func TestMetricRegistry_Handler(t *testing.T) {
	o := NewMetricsObserver()
	o.Resumed(true)
	o.Resumed(false)
	h := o.Registry().Handler(nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != metricsContentType {
		t.Fatalf("GET code = %d type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, MetricRunResumes+`{result="ok"} 1`) || !strings.Contains(body, MetricRunResumes+`{result="error"} 1`) {
		t.Fatalf("body = %s", body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST code = %d, want 405", rec.Code)
	}

	if got := labelText([]string{"a"}, []string{"x\"y\\z\n"}, ""); got != `{a="x\"y\\z\n"}` {
		t.Fatalf("escaped = %s", got)
	}
}

// 指标名都以 as_ 开头，常量登记的指标都在注册表里（规则文件校验依赖 Families）。
func TestMetricsObserver_FamiliesRegistered(t *testing.T) {
	fams := NewMetricsObserver().Registry().Families()
	var names []string
	for _, f := range fams {
		if !strings.HasPrefix(f.Name, "as_") {
			t.Errorf("metric %q lacks as_ prefix", f.Name)
		}
		names = append(names, f.Name)
	}
	for _, want := range []string{MetricAlerts, MetricDispatch, MetricOrderTransitions, MetricApprovalBacklog, MetricApprovalWait,
		DeniedMetric, MetricFlagChanges, MetricRunResumes, MetricLLMTokens, MetricMCPCalls, MetricDegraded} {
		if !slices.Contains(names, want) {
			t.Errorf("metric %q not registered", want)
		}
	}
}

// 审查修复 P3：counter 只增，负值与 NaN 丢弃；gauge 不受限。
func TestMetricRegistry_CounterRejectsNegative(t *testing.T) {
	r := NewMetricRegistry()
	c := r.register(MetricCounter, "as_test_total", "t", nil, "k")
	g := r.register(MetricGauge, "as_test_gauge", "t", nil, "k")
	tests := []struct {
		name string
		v    float64
		want string
	}{
		{name: "positive counted", v: 2, want: `as_test_total{k="a"} 2`},
		{name: "negative ignored", v: -5, want: `as_test_total{k="a"} 2`},
		{name: "NaN ignored", v: math.NaN(), want: `as_test_total{k="a"} 2`},
		{name: "zero ok", v: 0, want: `as_test_total{k="a"} 2`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.add(tt.v, "a")
			var b strings.Builder
			if err := r.WriteText(&b); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(b.String(), tt.want) {
				t.Fatalf("got:\n%s\nwant %q", b.String(), tt.want)
			}
		})
	}
	g.add(-3, "a")
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `as_test_gauge{k="a"} -3`) {
		t.Fatalf("gauge must accept negative:\n%s", b.String())
	}
}

// blockingWriter 第一次 Write 阻塞到 release 关闭（模拟慢 /metrics 客户端）。
type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

// 审查修复 P2：WriteText 只在族锁内复制快照，写网络时不持锁；慢客户端阻塞期间各族埋点仍能立即完成。
func TestMetricRegistry_SlowWriterDoesNotBlockRecording(t *testing.T) {
	o := NewMetricsObserver()
	o.Preseed([]uint64{1, 2, 3, 4, 5, 6, 7, 8}) // 输出超过 bufio 缓冲，写出发生在遍历中途
	w := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	writeDone := make(chan error, 1)
	go func() { writeDone <- o.Registry().WriteText(w) }()
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		close(w.release)
		t.Fatal("WriteText never wrote")
	}
	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		for _, f := range o.reg.families {
			f.touch(make([]string, len(f.info.Labels))...)
		}
		o.AlertFired(model.Alert{Name: model.AlertModelActuate, Level: model.AlertP0})
	}()
	select {
	case <-recDone:
	case <-time.After(2 * time.Second):
		close(w.release)
		t.Fatal("recording blocked while /metrics writer is slow (family lock held during write)")
	}
	close(w.release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}
