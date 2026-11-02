package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

type fakeRecipients struct {
	parties    repository.TicketParties
	partiesErr error
	byRole     map[model.Role][]uint64
	rolesErr   error
	gotTicket  uint64
}

func (f *fakeRecipients) FarmUsersByRoles(_ context.Context, _ model.Scope, roles []model.Role) ([]uint64, error) {
	var out []uint64
	for _, r := range roles {
		out = append(out, f.byRole[r]...)
	}
	return out, f.rolesErr
}

func (f *fakeRecipients) TicketParties(_ context.Context, _ model.Scope, _, ticketID uint64) (repository.TicketParties, error) {
	f.gotTicket = ticketID
	return f.parties, f.partiesErr
}

type recordChannel struct {
	got []Notification
	err error
}

func (c *recordChannel) Send(_ context.Context, n Notification) error {
	c.got = append(c.got, n)
	return c.err
}

func testNotifyTexts(t *testing.T) *NotifyTexts {
	t.Helper()
	tx, err := LoadNotifyTexts("../../configs/texts/notify.zh-CN.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// 接收人规则：待审批 / 即将过期排除发起人；复检退回（取 new_ticket_id）与设备离线（无票）加发起人；
// 应失败：未知 kind / 缺票 / 坏 JSON → ErrBadOutboxPayload；查库与通道失败返回错误（退避）；无接收人 → FAILED 审计 + DONE。
func TestNotifyHandler(t *testing.T) {
	parties := repository.TicketParties{InitiatorID: 1004, RequiredRole: []model.Role{model.RoleAgronomist}}
	byRole := map[model.Role][]uint64{model.RoleAgronomist: {1002, 1004}, model.RoleFarmManager: {1001}}
	boom := errors.New("boom")
	tests := []struct {
		name       string
		payload    string
		rec        fakeRecipients
		chanErr    error
		auditErr   error
		wantUsers  []uint64
		wantTicket uint64
		wantErr    error // 为 nil 时：wantFailed 表示无接收人，否则投递成功
		wantFailed bool
	}{
		{name: "pending excludes initiator", payload: `{"kind":"PENDING_APPROVAL","order_id":9,"ticket_id":7}`,
			rec: fakeRecipients{parties: parties, byRole: byRole}, wantUsers: []uint64{1002}, wantTicket: 7},
		{name: "expiring excludes initiator", payload: `{"kind":"EXPIRING_SOON","order_id":9,"ticket_id":7}`,
			rec: fakeRecipients{parties: parties, byRole: byRole}, wantUsers: []uint64{1002}, wantTicket: 7},
		{name: "recheck uses new ticket and adds initiator",
			payload: `{"kind":"RECHECK_RETURNED","order_id":9,"run_id":5,"old_ticket_id":7,"new_ticket_id":8}`,
			rec:     fakeRecipients{parties: parties, byRole: byRole}, wantUsers: []uint64{1002, 1004}, wantTicket: 8},
		{name: "device offline: farm manager + initiator", payload: `{"kind":"DEVICE_OFFLINE","order_id":9}`,
			rec: fakeRecipients{parties: parties, byRole: byRole}, wantUsers: []uint64{1001, 1004}},
		{name: "device offline without ticket row", payload: `{"kind":"DEVICE_OFFLINE","order_id":9}`,
			rec: fakeRecipients{partiesErr: model.ErrNotFound, byRole: byRole}, wantUsers: []uint64{1001}},
		{name: "unknown kind", payload: `{"kind":"SPAM","order_id":9,"ticket_id":7}`, wantErr: ErrBadOutboxPayload},
		{name: "bad json", payload: `{`, wantErr: ErrBadOutboxPayload},
		{name: "pending without ticket", payload: `{"kind":"PENDING_APPROVAL","order_id":9}`, wantErr: ErrBadOutboxPayload},
		{name: "no recipient -> FAILED audit, done", payload: `{"kind":"PENDING_APPROVAL","order_id":9,"ticket_id":7}`,
			rec: fakeRecipients{parties: parties, byRole: map[model.Role][]uint64{model.RoleAgronomist: {1004}}}, wantFailed: true},
		{name: "ticket missing -> FAILED audit", payload: `{"kind":"EXPIRING_SOON","order_id":9,"ticket_id":7}`,
			rec: fakeRecipients{partiesErr: model.ErrNotFound, byRole: byRole}, wantFailed: true},
		{name: "no recipient audit fails -> error", payload: `{"kind":"PENDING_APPROVAL","order_id":9,"ticket_id":7}`,
			rec: fakeRecipients{parties: parties}, auditErr: boom, wantErr: boom},
		{name: "parties db error", payload: `{"kind":"PENDING_APPROVAL","order_id":9,"ticket_id":7}`,
			rec: fakeRecipients{partiesErr: boom}, wantErr: boom},
		{name: "roles db error", payload: `{"kind":"PENDING_APPROVAL","order_id":9,"ticket_id":7}`,
			rec: fakeRecipients{parties: parties, rolesErr: boom}, wantErr: boom},
		{name: "channel error -> backoff", payload: `{"kind":"PENDING_APPROVAL","order_id":9,"ticket_id":7}`,
			rec: fakeRecipients{parties: parties, byRole: byRole}, chanErr: boom, wantErr: boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &recordChannel{err: tt.chanErr}
			audit := &listAudit{err: tt.auditErr}
			h, err := NewNotifyHandler(NotifyDeps{Recipients: &tt.rec, Channel: ch, Texts: testNotifyTexts(t), Audit: audit,
				Clock: fixedClock{dispatchNow}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			msg := repository.OutboxMessage{ID: 1, TenantID: 1, FarmID: 2, Topic: repository.TopicNotifySend, AggregateID: 9,
				DedupKey: "notify:x", Payload: json.RawMessage(tt.payload)}
			err = h.Handle(context.Background(), msg)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if tt.wantErr != ErrBadOutboxPayload && errors.Is(err, ErrBadOutboxPayload) {
					t.Fatalf("transient error must not be ErrBadOutboxPayload: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantFailed {
				if len(ch.got) != 0 || len(audit.recs) != 1 || audit.recs[0].Result != model.AuditResultFailed ||
					audit.recs[0].Action != string(model.AuditNotifySend) {
					t.Fatalf("sent = %+v audit = %+v", ch.got, audit.recs)
				}
				return
			}
			if len(ch.got) != 1 || !slices.Equal(ch.got[0].UserIDs, tt.wantUsers) || tt.rec.gotTicket != tt.wantTicket {
				t.Fatalf("sent = %+v ticket = %d", ch.got, tt.rec.gotTicket)
			}
			n := ch.got[0]
			if n.Scope.FarmID != 2 || n.TextKey != NotifyTextKey(n.Kind) || !strings.Contains(n.Text, "#9") || n.DedupKey != "notify:x" {
				t.Fatalf("notification = %+v", n)
			}
		})
	}
	if _, err := NewNotifyHandler(NotifyDeps{}); err == nil {
		t.Fatal("want missing deps error")
	}
}

// 缺省通道：审计 NOTIFY_SEND OK，摘要含 user_ids / kind / text_key、不含正文；审计失败返回错误。
func TestAuditChannel(t *testing.T) {
	audit := &listAudit{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := Notification{Scope: model.Scope{TenantID: 1, FarmID: 2}, Kind: model.NotifyPendingApproval, OrderID: 9, TicketID: 7,
		RunID: 5, UserIDs: []uint64{1002}, TextKey: NotifyTextKey(model.NotifyPendingApproval), Text: "作业单 #9 待您审批"}
	if err := (AuditChannel{Audit: audit, Clock: fixedClock{dispatchNow}, Logger: log}).Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	r := audit.recs[0]
	s := string(r.Summary)
	if r.Result != model.AuditResultOK || r.Target != "order:9" || r.RunID == nil || *r.RunID != 5 ||
		!strings.Contains(s, `"user_ids":[1002]`) || !strings.Contains(s, `"text_key":"notify.PENDING_APPROVAL"`) ||
		strings.Contains(s, "待您审批") {
		t.Fatalf("audit = %+v summary = %s", r, s)
	}
	boom := errors.New("boom")
	if err := (AuditChannel{Audit: &listAudit{err: boom}, Clock: fixedClock{dispatchNow}, Logger: log}).Send(
		context.Background(), n); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if err := (AuditChannel{}).Send(context.Background(), n); err == nil {
		t.Fatal("want not configured")
	}
}

// 文案：仓库内文件四种 kind 齐全；应失败：缺 key / 未知 kind / 未知字段 / version 不符 / 空模板 / 文件不存在。
func TestParseNotifyTexts(t *testing.T) {
	b, err := os.ReadFile("../../configs/texts/notify.zh-CN.yaml")
	if err != nil {
		t.Fatal(err)
	}
	full := string(b)
	tx, err := ParseNotifyTexts(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range NotifyKinds {
		if s, ok := tx.Render(k, 9, 7); !ok || strings.Contains(s, "{") {
			t.Fatalf("%s: %q", k, s)
		}
	}
	tests := []struct{ name, yaml string }{
		{"missing key", strings.Replace(full, "  DEVICE_OFFLINE:", "  # DEVICE_OFFLINE:", 1)},
		{"unknown kind", full + "  SPAM: \"x\"\n"},
		{"unknown field", full + "extra: 1\n"},
		{"bad version", strings.Replace(full, "version: 1", "version: 2", 1)},
		{"empty text", "version: 1\ntexts:\n  PENDING_APPROVAL: a\n  EXPIRING_SOON: \" \"\n  RECHECK_RETURNED: c\n  DEVICE_OFFLINE: d\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseNotifyTexts([]byte(tt.yaml)); !errors.Is(err, ErrBadNotifyTexts) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := LoadNotifyTexts("no-such-file.yaml"); err == nil {
		t.Fatal("want read error")
	}
}
