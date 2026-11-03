package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

type memAlertList struct {
	rows       []repository.AlertRow
	err        error
	gotScope   model.Scope
	gotActions []string
	gotFrom    time.Time
	gotTo      time.Time
	gotBefore  repository.AlertCursor
	gotLimit   int
	calls      int
}

// ListAlerts 模拟 SQL：rows 已按 (created_at, id) 倒序，取严格小于游标的前 limit 行。
func (m *memAlertList) ListAlerts(_ context.Context, scope model.Scope, actions []string, from, to time.Time,
	before repository.AlertCursor, limit int) ([]repository.AlertRow, error) {
	m.calls++
	m.gotScope, m.gotActions, m.gotFrom, m.gotTo, m.gotBefore, m.gotLimit = scope, actions, from, to, before, limit
	if m.err != nil {
		return nil, m.err
	}
	var out []repository.AlertRow
	for _, r := range m.rows {
		if !before.IsZero() && !(r.CreatedAt.Before(before.At) || (r.CreatedAt.Equal(before.At) && r.ID < before.ID)) {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, r)
	}
	return out, nil
}

// 审查修复 P2：键集分页。limit+1 判断下一页；同一 created_at 按 id 切页不重不漏；末页无 next_cursor；
// 应失败：非法游标 → ErrBadInput 且不查库。
func TestAlertService_ListCursor(t *testing.T) {
	scope := model.Scope{TenantID: 1, FarmID: 3}
	t1, t2 := decideNow.Add(-2*time.Minute), decideNow.Add(-time.Minute)
	store := &memAlertList{rows: []repository.AlertRow{
		{ID: 5, CreatedAt: t2, Action: "ALERT.model_actuate"}, {ID: 4, CreatedAt: t2, Action: "ALERT.model_actuate"},
		{ID: 3, CreatedAt: t2, Action: "ALERT.model_actuate"}, {ID: 9, CreatedAt: t1, Action: "ALERT.model_actuate"},
		{ID: 2, CreatedAt: t1, Action: "ALERT.model_actuate"},
	}}
	svc, err := NewAlertService(store, fixedClock{decideNow})
	if err != nil {
		t.Fatal(err)
	}
	var got []uint64
	var pages int
	q := AlertQuery{Limit: 2}
	for {
		list, err := svc.List(context.Background(), scope, q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if store.gotLimit != 3 {
			t.Fatalf("store limit = %d, want limit+1", store.gotLimit)
		}
		for _, it := range list.Items {
			got = append(got, it.ID)
		}
		if list.NextCursor == "" {
			break
		}
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
		q.Cursor = list.NextCursor
	}
	if !slices.Equal(got, []uint64{5, 4, 3, 9, 2}) || pages != 3 {
		t.Fatalf("ids = %v over %d pages, want [5 4 3 9 2] over 3", got, pages)
	}
	if c, err := ParseAlertCursor(FormatAlertCursor(repository.AlertCursor{At: t2, ID: 4})); err != nil ||
		!c.At.Equal(t2) || c.ID != 4 {
		t.Fatalf("cursor round trip = %+v, %v", c, err)
	}

	calls := store.calls
	for _, bad := range []string{"x", "_", "abc_1", "-5_1", "0_1", "1700000000000_0", "1700000000000_x", "1700000000000"} {
		if _, err := svc.List(context.Background(), scope, AlertQuery{Cursor: bad}); !errors.Is(err, ErrBadInput) {
			t.Fatalf("cursor %q: err = %v, want ErrBadInput", bad, err)
		}
	}
	if store.calls != calls {
		t.Fatal("store queried on bad cursor")
	}
}

// 告警列表：缺省窗口 24h / limit 50；按告警名映射 action（cross_farm_denied 含业务审计）；行带 run 链接与 summary 字段。
// 应失败用例：未登记告警名、from>=to、跨度超限、limit 越界 → ErrBadInput 且不查库。
func TestAlertService_List(t *testing.T) {
	scope := model.Scope{TenantID: 1, FarmID: 3}
	run := uint64(9)
	sum, err := json.Marshal(alertSummary{Alert: model.AlertPreApprovalDevice, Level: model.AlertP0, RunID: 9, OrderID: 4,
		Detail: "device call blocked"})
	if err != nil {
		t.Fatal(err)
	}
	store := &memAlertList{rows: []repository.AlertRow{
		{ID: 2, RunID: &run, Action: "ALERT.pre_approval_device", Target: "order:4", Result: model.AuditResultFailed, Summary: sum},
		{ID: 1, Action: string(model.AuditCrossFarmDenied), Result: model.AuditResultDenied, Summary: json.RawMessage(`{"user_id":7}`)},
	}}
	svc, err := NewAlertService(store, fixedClock{decideNow})
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(context.Background(), scope, AlertQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !store.gotTo.Equal(decideNow) || !store.gotFrom.Equal(decideNow.Add(-DefaultAlertListSpan)) ||
		store.gotLimit != DefaultAlertListLimit+1 || store.gotScope.FarmID != 3 || !store.gotBefore.IsZero() {
		t.Fatalf("from = %v to = %v limit = %d", store.gotFrom, store.gotTo, store.gotLimit)
	}
	if len(store.gotActions) != len(model.AlertNames)+2 || !slices.Contains(store.gotActions, string(model.AuditFlagChange)) {
		t.Fatalf("actions = %v", store.gotActions)
	}
	a, b := list.Items[0], list.Items[1]
	if a.Alert != model.AlertPreApprovalDevice || a.Level != model.AlertP0 || a.OrderID != 4 || a.RunID != 9 ||
		a.RunLink != "/api/v1/farms/3/runs/9" || a.Detail != "device call blocked" {
		t.Fatalf("item 0 = %+v", a)
	}
	if b.Alert != model.AlertCrossFarmDenied || b.RunLink != "" || b.Detail != "" {
		t.Fatalf("item 1 = %+v (business summary must not leak)", b)
	}
	if list.NextCursor != "" {
		t.Fatalf("next_cursor = %q, want empty when rows <= limit", list.NextCursor)
	}
	out, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "user_id") {
		t.Fatalf("business audit summary leaked: %s", out)
	}

	if _, err := svc.List(context.Background(), scope, AlertQuery{Name: model.AlertCrossFarmDenied}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.gotActions, []string{"ALERT.cross_farm_denied", string(model.AuditCrossFarmDenied)}) {
		t.Fatalf("actions = %v", store.gotActions)
	}

	bad := []AlertQuery{
		{Name: "made_up"},
		{From: decideNow, To: decideNow},
		{From: decideNow.Add(-MaxAlertListSpan - time.Hour), To: decideNow},
		{Limit: MaxAlertListLimit + 1},
		{Limit: -1},
	}
	calls := store.calls
	for _, q := range bad {
		if _, err := svc.List(context.Background(), scope, q); !errors.Is(err, ErrBadInput) {
			t.Fatalf("query %+v: err = %v, want ErrBadInput", q, err)
		}
	}
	if store.calls != calls {
		t.Fatal("store queried on bad input")
	}

	store.err = errors.New("db down")
	if _, err := svc.List(context.Background(), scope, AlertQuery{}); err == nil || errors.Is(err, ErrBadInput) {
		t.Fatalf("err = %v, want wrapped store error", err)
	}
}
