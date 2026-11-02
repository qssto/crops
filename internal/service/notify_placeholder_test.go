package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// listAudit 是 AuditAppender 的桩。
type listAudit struct {
	recs []model.AuditRecord
	err  error
}

func (l *listAudit) Append(_ context.Context, r model.AuditRecord) error {
	if l.err != nil {
		return l.err
	}
	l.recs = append(l.recs, r)
	return nil
}

// notify.send 占位：写 NOTIFY_SEND 审计后 DONE；审计失败返回错误（退避，不吞）；payload 坏 → ErrBadOutboxPayload
// （应失败：审计失败也返回 nil，通知静默丢失）。
func TestNotifyPlaceholder(t *testing.T) {
	msg := repository.OutboxMessage{ID: 9, TenantID: 1, FarmID: 2, Topic: repository.TopicNotifySend, AggregateID: 3,
		DedupKey: RecheckReturnedDedupKey(3, 7),
		Payload:  json.RawMessage(`{"kind":"RECHECK_RETURNED","order_id":3,"run_id":5,"old_ticket_id":7}`)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	audit := &listAudit{}
	n := NotifyPlaceholder{Audit: audit, Clock: fixedClock{dispatchNow}, Logger: log}
	if err := n.Handle(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(audit.recs) != 1 {
		t.Fatalf("audit = %+v", audit.recs)
	}
	r := audit.recs[0]
	if r.Action != string(model.AuditNotifySend) || r.Result != model.AuditResultOK || r.FarmID != 2 || r.Target != "order:3" ||
		r.RunID == nil || *r.RunID != 5 {
		t.Errorf("audit = %+v", r)
	}
	boom := errors.New("audit down")
	if err := (NotifyPlaceholder{Audit: &listAudit{err: boom}, Clock: fixedClock{dispatchNow}, Logger: log}).Handle(
		context.Background(), msg); !errors.Is(err, boom) {
		t.Fatalf("audit failure err = %v", err)
	}
	bad := msg
	bad.Payload = json.RawMessage(`{"order_id":3}`)
	if err := n.Handle(context.Background(), bad); !errors.Is(err, ErrBadOutboxPayload) {
		t.Fatalf("bad payload err = %v", err)
	}
}
