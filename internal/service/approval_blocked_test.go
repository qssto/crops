package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/model"
)

// failedCheck 是一条未通过的 PRE_APPROVAL 行（observed 带 note，threshold 带 kb_id@version）。
func failedCheck(ruleID, note string) model.ConstraintCheck {
	return model.ConstraintCheck{Phase: model.PhasePreApproval, RuleID: ruleID,
		Observed:  json.RawMessage(fmt.Sprintf(`{"value":4.2,"unit":"m/s","note":%q}`, note)),
		Threshold: json.RawMessage(`{"kb":"kb:common/operation_window/spray-weather@v1","op":"<=","value":3}`)}
}

// 复核第 2 条遗留：CONSTRAINT_BLOCKED payload 带未通过行的 rule_id / observed / threshold / note（通过行不带），
// 作为该次拦截的证据（PRE_APPROVAL 行只保留最近一次复核）。
func TestApprovalOpen_BlockedPayloadCarriesEvidence(t *testing.T) {
	f := newApprovalFixtureWith(t, &fakeTickets{}, model.OrderDraft, fakeRisk{}, &fakeReviewer{blocked: []string{constraint.RuleWind}})
	req := openRequest()
	wind := failedCheck(constraint.RuleWind, "wind 4.2 m/s > 3")
	req.Order.ConstraintChecks = append(req.Order.ConstraintChecks, wind)
	if _, err := f.svc.Open(context.Background(), approvalScope, req); !errors.Is(err, agent.ErrConstraintBlocked) {
		t.Fatalf("err = %v", err)
	}
	if len(f.events.events) != 1 || len(f.events.events[0].Payload) > blockedPayloadMax {
		t.Fatalf("events = %+v", f.events.events)
	}
	var p model.ConstraintBlockedPayload
	if err := json.Unmarshal(f.events.events[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.OrderID != 9 || p.Truncated || !slices.Equal(p.RuleIDs, []string{constraint.RuleWind}) || len(p.Checks) != 1 {
		t.Fatalf("payload %+v", p)
	}
	c := p.Checks[0]
	if c.RuleID != constraint.RuleWind || c.Note != "wind 4.2 m/s > 3" ||
		!strings.Contains(string(c.Threshold), "spray-weather@v1") || !strings.Contains(string(c.Observed), `"value":4.2`) {
		t.Errorf("check %+v (observed %s threshold %s)", c, c.Observed, c.Threshold)
	}
}

// 复核第 2 条遗留：payload 超过 8KB 时从 checks 末尾截断并标注 truncated，rule_ids 保持完整。
func TestBlockedPayload_Truncates(t *testing.T) {
	long := strings.Repeat("未识别药剂", 200) // 每行 note 约 3KB
	var checks []model.ConstraintCheck
	var ids []string
	for i := range 6 {
		id := fmt.Sprintf("rule-%d", i)
		ids = append(ids, id)
		checks = append(checks, failedCheck(id, long))
	}
	b, err := blockedPayload(9, ids, checks)
	if err != nil {
		t.Fatal(err)
	}
	var p model.ConstraintBlockedPayload
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if len(b) > blockedPayloadMax || !p.Truncated || len(p.Checks) == 0 || len(p.Checks) >= len(checks) ||
		!slices.Equal(p.RuleIDs, ids) || p.Checks[0].RuleID != "rule-0" {
		t.Fatalf("len=%d truncated=%v checks=%d rule_ids=%v", len(b), p.Truncated, len(p.Checks), p.RuleIDs)
	}
	t.Run("fits without truncation", func(t *testing.T) {
		b, err := blockedPayload(9, ids[:1], checks[:1])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "truncated") {
			t.Fatalf("payload %s", b)
		}
	})
	t.Run("rule_ids trimmed only after checks", func(t *testing.T) {
		many := make([]string, 400)
		for i := range many {
			many[i] = fmt.Sprintf("unmapped:%040d", i)
		}
		b, err := blockedPayload(9, many, checks)
		if err != nil {
			t.Fatal(err)
		}
		var p model.ConstraintBlockedPayload
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		if len(b) > blockedPayloadMax || !p.Truncated || len(p.Checks) != 0 || len(p.RuleIDs) == 0 || len(p.RuleIDs) >= len(many) {
			t.Fatalf("len=%d checks=%d rule_ids=%d", len(b), len(p.Checks), len(p.RuleIDs))
		}
	})
	t.Run("bad observed fails", func(t *testing.T) {
		bad := failedCheck("x", "n")
		bad.Observed = json.RawMessage(`{`)
		if _, err := blockedPayload(9, []string{"x"}, []model.ConstraintCheck{bad}); err == nil {
			t.Fatal("want error")
		}
	})
}

// 复核第 21 条：开票与拦截事务的第一条语句都是 work_order 行锁，之后才写校验行 / 票据（同一张单写入串行，避免 1213）。
func TestApprovalOpen_LocksOrderFirst(t *testing.T) {
	f := newApprovalFixture(t, &fakeTickets{}, model.OrderDraft, fakeRisk{})
	if _, err := f.svc.Open(context.Background(), approvalScope, openRequest()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"lock_order", "upsert_checks", "insert_ticket"}; !slices.Equal(f.tickets.trace, want) {
		t.Errorf("open trace = %v, want %v", f.tickets.trace, want)
	}
	b := newApprovalFixtureWith(t, &fakeTickets{}, model.OrderDraft, fakeRisk{}, &fakeReviewer{blocked: []string{constraint.RuleWind}})
	if _, err := b.svc.Open(context.Background(), approvalScope, openRequest()); !errors.Is(err, agent.ErrConstraintBlocked) {
		t.Fatalf("block err = %v", err)
	}
	if want := []string{"lock_order", "upsert_checks", "lock_ticket"}; !slices.Equal(b.tickets.trace, want) {
		t.Errorf("block trace = %v, want %v", b.tickets.trace, want)
	}
}

// 复核第 22 条：拦截输掉与并发开票的竞争时（回滚校验行与事件），事务外记一条 Warn，带 order_id 与 rule_ids。
func TestApprovalOpen_BlockedRaceLogsWarn(t *testing.T) {
	winner := model.ApprovalTicket{TicketID: 41, Status: model.TicketPreparing}
	f := newApprovalFixtureWith(t, &fakeTickets{lockedInTx: &winner}, model.OrderDraft, fakeRisk{},
		&fakeReviewer{blocked: []string{constraint.RuleWind, constraint.RulePHI}})
	if got, err := f.svc.Open(context.Background(), approvalScope, openRequest()); err != nil || got.TicketID != 41 {
		t.Fatalf("open = %+v, %v", got, err)
	}
	log := f.logs.String()
	for _, want := range []string{"level=WARN", "order_id=9", constraint.RuleWind, constraint.RulePHI, "ticket_id=41"} {
		if !strings.Contains(log, want) {
			t.Errorf("log %q missing %q", log, want)
		}
	}
}
