package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L7-B 看板运营指标：下发成功率按 sent_at 归日，作业成功率只算 REAL 终态，复检退回 / EXPIRED 含 DRY_RUN，积压为快照。
// 应失败用例：DRY_RUN 终态不进作业成功率、空数据比率为 null（不是 0）。
func TestMetrics_OpsDashboard(t *testing.T) {
	realMode, dry := string(model.DispatchReal), string(model.DispatchDryRun)
	f := repository.MetricFacts{
		Attempts: []repository.MetricAttempt{
			{Accepted: true, SentAt: at(mDay1, 1)}, {Accepted: false, SentAt: at(mDay1, 2)}, {Accepted: true, SentAt: at(mDay2, 1)},
		},
		Transitions: []repository.MetricTransition{
			{To: model.OrderDone, DispatchMode: realMode, CreatedAt: at(mDay1, 3)},
			{To: model.OrderPartiallyDone, DispatchMode: realMode, CreatedAt: at(mDay1, 4)},
			{To: model.OrderFailed, DispatchMode: realMode, CreatedAt: at(mDay1, 5)},
			{To: model.OrderDone, DispatchMode: dry, CreatedAt: at(mDay1, 6)}, // DRY_RUN 不进作业成功率
			{To: model.OrderExpired, ReasonCode: model.ReasonApprovalExpired, DispatchMode: dry, CreatedAt: at(mDay2, 2)},
			{To: model.OrderApproved, ReasonCode: model.ReasonRecheckFailed, DispatchMode: realMode, CreatedAt: at(mDay2, 3)},
		},
		OpenTickets: 4,
	}
	rep, err := NewMetricsService(&fakeFacts{facts: f}, fixedClock{at(mDay3, 1)}).Daily(context.Background(),
		model.Scope{TenantID: 1, FarmID: 1}, mDay1, mDay3)
	if err != nil {
		t.Fatal(err)
	}
	d1, d2, d3 := rep.Days[0], rep.Days[1], rep.Days[2]
	checkRatio(t, "day1 dispatch", d1.DispatchSuccess, 1, 2)
	checkRatio(t, "day1 job", d1.JobSuccess, 2, 3)
	checkRatio(t, "day2 dispatch", d2.DispatchSuccess, 1, 1)
	if d2.JobSuccess.Value != nil || d3.DispatchSuccess.Value != nil {
		t.Fatalf("empty ratios must be null: day2 job = %+v day3 dispatch = %+v", d2.JobSuccess, d3.DispatchSuccess)
	}
	if d1.Expired != 0 || d2.Expired != 1 || d2.RecheckReturned != 1 || d1.RecheckReturned != 0 {
		t.Fatalf("day1 = %+v day2 = %+v", d1.OpsMetrics, d2.OpsMetrics)
	}
	checkRatio(t, "total dispatch", rep.Total.DispatchSuccess, 2, 3)
	checkRatio(t, "total job", rep.Total.JobSuccess, 2, 3)
	if rep.Total.Expired != 1 || rep.Total.RecheckReturned != 1 || rep.ApprovalBacklog != 4 {
		t.Fatalf("total = %+v backlog = %d", rep.Total.OpsMetrics, rep.ApprovalBacklog)
	}

	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"dispatch_success_rate"`, `"job_success_rate"`, `"recheck_returned"`, `"expired"`,
		`"approval_backlog":4`, `"approval_wait"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON missing %s", key)
		}
	}
}

func checkRatio(t *testing.T, name string, r Ratio, num, den int) {
	t.Helper()
	if r.Num != num || r.Den != den || r.Value == nil {
		t.Fatalf("%s = %+v, want %d/%d", name, r, num, den)
	}
}
