package env

import (
	"context"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

func TestFixtureLocalOrders(t *testing.T) {
	src, err := NewFixtureLocalOrders([]sample.LocalOrderFixture{
		{WorkType: "SPRAY", Status: "EXECUTING", UpdatedAt: "2026-07-01T08:00:00+08:00", Product: "氯虫苯甲酰胺"},
		{WorkType: "SPRAY", Status: "DONE", UpdatedAt: "2026-07-07T08:00:00+08:00"},
		{WorkType: "FERTILIZE", Status: "DONE", UpdatedAt: "2026-06-01T08:00:00+08:00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	got, err := src.RecentOrders(context.Background(), model.Scope{}, 1, fixtureOrderIDBase+1, since)
	if err != nil {
		t.Fatal(err)
	}
	// 在途单不论时间都返回；since 前完成的单不返回；excludeOrderID（第 2 条）排除。
	if len(got) != 1 || got[0].OrderID != fixtureOrderIDBase || got[0].Status != model.OrderExecuting ||
		string(got[0].Payload) != `{"product":"氯虫苯甲酰胺"}` {
		t.Fatalf("got %+v", got)
	}
	all, err := src.RecentOrders(context.Background(), model.Scope{}, 1, 0, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want in-flight + done after since, got %+v", all)
	}

	for name, f := range map[string]sample.LocalOrderFixture{
		"bad time":      {WorkType: "SPRAY", Status: "DONE", UpdatedAt: "2026-07-01"},
		"draft status":  {WorkType: "SPRAY", Status: "DRAFT", UpdatedAt: "2026-07-01T08:00:00+08:00"},
		"bad work type": {WorkType: "HARVEST", Status: "DONE", UpdatedAt: "2026-07-01T08:00:00+08:00"},
	} {
		if _, err := NewFixtureLocalOrders([]sample.LocalOrderFixture{f}); !errors.Is(err, errBadLocalOrders) {
			t.Errorf("%s: err %v", name, err)
		}
	}
}
