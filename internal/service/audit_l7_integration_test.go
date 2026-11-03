//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P1-L7-A：真实库上的篡改检出（验收 2）、截尾重算检出（锚点）、保留期清理端到端（验收 5 的保留期部分）。

var l7Farm = repository.ChainKey{TenantID: itest.TenantID, FarmID: itest.FarmID}

// appendChain 在 (1,1) 链上逐笔写 n 行审计，返回 chain_seq → audit_log.id。
func appendChain(t *testing.T, db *repository.DB, n int) map[uint64]uint64 {
	t.Helper()
	for i := range n {
		summary := json.RawMessage(fmt.Sprintf(`{"i": %d, "问题": "稻瘟病"}`, i))
		if err := (repository.AuditRepo{DB: db}).Append(t.Context(), model.AuditRecord{TenantID: itest.TenantID,
			FarmID: itest.FarmID, Actor: model.Actor{Type: model.ActorUser, ID: 1001}, Action: "ASK",
			Target: fmt.Sprintf("field:%d", i), PayloadDigest: repository.Digest(summary), Summary: summary,
			Result: model.AuditResultOK, CreatedAt: t0.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := repository.AuditChainRepo{DB: db}.Rows(t.Context(), l7Farm, 1, uint64(n))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[uint64]uint64{}
	for _, r := range rows {
		ids[r.ChainSeq] = r.ID
	}
	return ids
}

func execSQL(t *testing.T, db *repository.DB, q string, args ...any) {
	t.Helper()
	if err := db.WithContext(t.Context()).Exec(q, args...).Error; err != nil {
		t.Fatal(err)
	}
}

// 验收 2：直接改库的三类篡改都被 VerifyChain 报出第一个断点（类型 + chain_seq + audit_log.id）。
func TestVerifyChainTamper_Integration(t *testing.T) {
	e := itest.Setup(t, "svcl7a")
	tests := []struct {
		name     string
		tamper   string
		wantKind service.BreakKind
		wantSeq  uint64
		idOfSeq  uint64 // 期望 AuditID 对应的 chain_seq；0 表示断点不落在行上
	}{
		{name: "intact"},
		{"row modified", `UPDATE audit_log SET summary = JSON_OBJECT('i', 99) WHERE tenant_id = 1 AND farm_id = 1 AND chain_seq = 3`,
			service.BreakRowModified, 3, 3},
		{"middle row deleted", `DELETE FROM audit_log WHERE tenant_id = 1 AND farm_id = 1 AND chain_seq = 3`,
			service.BreakGap, 3, 4},
		{"prev hash rewritten", `UPDATE audit_log SET prev_hash = REPEAT('f', 64) WHERE tenant_id = 1 AND farm_id = 1 AND chain_seq = 3`,
			service.BreakLinkBroken, 3, 3},
		{"last row deleted", `DELETE FROM audit_log WHERE tenant_id = 1 AND farm_id = 1 AND chain_seq = 5`,
			service.BreakGap, 5, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slot := e.Fresh(t, t0)
			ids := appendChain(t, slot.DB, 5)
			if tt.tamper != "" {
				execSQL(t, slot.DB, tt.tamper)
			}
			res, err := service.VerifyChain(t.Context(), repository.AuditChainRepo{DB: slot.DB}, l7Farm, 0, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantKind == "" {
				if res.Break != nil || res.Rows != 5 {
					t.Fatalf("intact chain: rows %d break %+v", res.Rows, res.Break)
				}
				return
			}
			b := res.Break
			if b == nil || b.Kind != tt.wantKind || b.ChainSeq != tt.wantSeq || b.AuditID != ids[tt.idOfSeq] {
				t.Fatalf("break = %+v, want %s seq %d id %d", b, tt.wantKind, tt.wantSeq, ids[tt.idOfSeq])
			}
		})
	}
}

// 截尾后重算：verify 本身通过，只有与锚点比对才能发现（TRUNCATED / RECOMPUTED）。
func TestAnchorDetectsRecompute_Integration(t *testing.T) {
	e := itest.Setup(t, "svcl7a")
	chains := func(db *repository.DB) repository.AuditChainRepo { return repository.AuditChainRepo{DB: db} }
	tests := []struct {
		name   string
		tamper func(t *testing.T, db *repository.DB)
		want   service.AnchorMismatchKind
	}{
		{name: "intact", tamper: func(*testing.T, *repository.DB) {}},
		{"truncated tail with head rewound", func(t *testing.T, db *repository.DB) {
			rows, err := chains(db).Rows(t.Context(), l7Farm, 2, 2)
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows %v err %v", rows, err)
			}
			execSQL(t, db, `DELETE FROM audit_log WHERE tenant_id = 1 AND farm_id = 1 AND chain_seq = 3`)
			execSQL(t, db, `UPDATE audit_chain_head SET chain_seq = 2, head_hash = ? WHERE tenant_id = 1 AND farm_id = 1`,
				rows[0].RowHash)
		}, service.AnchorTruncated},
		{"last row rewritten and rehashed", func(t *testing.T, db *repository.DB) {
			rows, err := chains(db).Rows(t.Context(), l7Farm, 3, 3)
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows %v err %v", rows, err)
			}
			r := rows[0]
			r.Rec.Summary = json.RawMessage(`{"i": 99}`)
			hash, err := repository.AuditRowHash(r.PrevHash, r)
			if err != nil {
				t.Fatal(err)
			}
			execSQL(t, db, `UPDATE audit_log SET summary = ?, row_hash = ? WHERE tenant_id = 1 AND farm_id = 1 AND id = ?`,
				string(r.Rec.Summary), hash, r.ID)
			execSQL(t, db, `UPDATE audit_chain_head SET head_hash = ? WHERE tenant_id = 1 AND farm_id = 1`, hash)
		}, service.AnchorRecomputed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slot := e.Fresh(t, t0)
			appendChain(t, slot.DB, 3)
			head, err := chains(slot.DB).Head(t.Context(), l7Farm)
			if err != nil {
				t.Fatal(err)
			}
			anchor, _, err := service.BuildAnchor("20260715", []repository.ChainHead{head}, t0)
			if err != nil {
				t.Fatal(err)
			}
			tt.tamper(t, slot.DB)
			res, err := service.VerifyChain(t.Context(), chains(slot.DB), l7Farm, 0, 0, nil)
			if err != nil || res.Break != nil {
				t.Fatalf("verify after %s: break %+v err %v (recompute must pass plain verify)", tt.name, res.Break, err)
			}
			mm, err := service.CheckAnchor(t.Context(), chains(slot.DB), anchor)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(mm) != 0 {
					t.Fatalf("intact: mismatches %v", mm)
				}
				return
			}
			if len(mm) != 1 || mm[0].Kind != tt.want || mm[0].Key != l7Farm || mm[0].ChainSeq != 3 {
				t.Fatalf("mismatches %v, want one %s at chain_seq 3", mm, tt.want)
			}
		})
	}
}

// memRemover 记录删除的对象 key（集成用例不连 MinIO）。
type memRemover struct{ keys []string }

func (m *memRemover) RemoveObject(_ context.Context, _, key string) error {
	m.keys = append(m.keys, key)
	return nil
}

type noObjects struct{}

func (noObjects) GetObject(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("object store must not be read for an expired raw_object")
}

// 保留期端到端：只清到期行（先删对象、再置 deleted_at、写 RETENTION_PURGE），当日重复 Tick 不再删；过期读返回 ErrRawExpired。
func TestRetentionPurge_Integration(t *testing.T) {
	e := itest.Setup(t, "svcl7a")
	slot := e.Fresh(t, t0)
	now := t0.Add(48 * time.Hour)
	insert := func(key string, expire time.Time) uint64 {
		execSQL(t, slot.DB, `INSERT INTO raw_object (tenant_id, farm_id, kind, object_key, sha256, size_bytes, created_at, expire_at)
VALUES (1, 1, 'LLM_IO', ?, REPEAT('a', 64), 10, ?, ?)`, key, t0, expire)
		return uint64(itest.Count(t, slot.DB, "SELECT MAX(id) FROM raw_object"))
	}
	expired1 := insert("raw/it/expired-1", t0)
	expired2 := insert("raw/it/expired-2", t0.Add(time.Hour))
	live := insert("raw/it/live", now.Add(24*time.Hour))
	rm := &memRemover{}
	svc := &service.RetentionService{Store: repository.RetentionRepo{DB: slot.DB}, Tx: repository.Transactor{DB: slot.DB},
		Audit: repository.AuditRepo{}, Objects: rm, Bucket: "agri-steward-raw",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sys := repository.NewSystemScope(service.ScanRetention)
	for i, want := range []int{2, 0} {
		st, err := svc.Tick(t.Context(), sys, now)
		if err != nil || st.Purged != want || !st.Drained {
			t.Fatalf("tick %d: %+v err %v, want purged %d drained", i+1, st, err, want)
		}
	}
	if len(rm.keys) != 2 || rm.keys[0] != "raw/it/expired-1" || rm.keys[1] != "raw/it/expired-2" {
		t.Fatalf("removed keys %v", rm.keys)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM raw_object WHERE deleted_at IS NOT NULL AND id IN (?, ?)",
		expired1, expired2); n != 2 {
		t.Fatalf("tombstoned = %d", n)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM raw_object WHERE deleted_at IS NULL AND id = ?", live); n != 1 {
		t.Fatal("live raw_object must stay")
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE tenant_id = 1 AND farm_id = 1
  AND action = 'RETENTION_PURGE' AND chain_seq IS NOT NULL`); n != 1 {
		t.Fatalf("RETENTION_PURGE audits = %d, want 1 (one group)", n)
	}
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001, Roles: []model.Role{model.RoleAgronomist}}
	_, err := repository.RawReader{DB: slot.DB, Obj: noObjects{}, Bucket: "agri-steward-raw"}.Get(t.Context(), scope, expired1)
	if !errors.Is(err, model.ErrRawExpired) {
		t.Fatalf("expired read err = %v, want ErrRawExpired", err)
	}
	checkRetentionRefs(t, slot.DB, now, insert)
}

// checkRetentionRefs 覆盖引用检查：非终态工单的 OPEN 票 / dispatch_attempt / device_receipt、非终态 run 的 checkpoint
// 引用的到期对象不清；工单与 run 进终态后清掉后三者，票据关闭后再清快照。
// 每阶段新建 RetentionService：被引用跳过的行当日不再重试（游标已越过）。
func checkRetentionRefs(t *testing.T, db *repository.DB, now time.Time, insert func(string, time.Time) uint64) {
	t.Helper()
	run := itest.InsertRun(t, db, t0)
	order := itest.InsertOrder(t, db, run, string(model.OrderDispatched), t0)
	snap := insert("raw/it/ref-snapshot", t0)
	cmd := insert("raw/it/ref-command", t0)
	rcpt := insert("raw/it/ref-receipt", t0)
	ckpt := insert("raw/it/ref-checkpoint", t0)
	execSQL(t, db, `INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_seq, risk_level, required_role,
  initiator_id, snapshot_digest, snapshot_raw_id, status, expires_at, created_at, updated_at)
VALUES (1, 1, ?, ?, 1, 'LOW', '["agronomist"]', 1001, REPEAT('b', 64), ?, 'OPEN', ?, ?, ?)`,
		run, order, snap, now.Add(time.Hour), t0, t0)
	ticket := uint64(itest.Count(t, db, "SELECT MAX(id) FROM approval_ticket"))
	execSQL(t, db, `INSERT INTO dispatch_attempt (tenant_id, farm_id, order_id, ticket_id, attempt, device_type, device_id,
  idem_key, mode, status, command_raw_id, flag_read_at, sent_at, receipt_due_at, updated_at)
VALUES (1, 1, ?, ?, 1, 'DRONE', 'uav-1', ?, 'DRY_RUN', 'SENT', ?, ?, ?, ?, ?)`,
		order, ticket, fmt.Sprintf("it-retention-%d", order), cmd, t0, t0, t0.Add(time.Hour), t0)
	execSQL(t, db, `INSERT INTO device_receipt (tenant_id, farm_id, receipt_id, order_id, attempt, device_job_id, norm_status,
  effect, device_ts, source, raw_object_id, created_at)
VALUES (1, 1, 'it-rcpt-1', ?, 1, 'job-1', 'ACCEPTED', 'APPLIED', ?, 'WEBHOOK', ?, ?)`, order, t0, rcpt, t0)
	execSQL(t, db, `INSERT INTO run_checkpoint (tenant_id, farm_id, run_id, seq, raw_object_id, size_bytes, created_at)
VALUES (1, 1, ?, 1, ?, 10, ?)`, run, ckpt, t0)
	sys := repository.NewSystemScope(service.ScanRetention)
	phase := func(name string, wantPurged, wantRefs int, wantGone ...uint64) {
		t.Helper()
		svc := &service.RetentionService{Store: repository.RetentionRepo{DB: db}, Tx: repository.Transactor{DB: db},
			Audit: repository.AuditRepo{}, Objects: &memRemover{}, Bucket: "agri-steward-raw",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		st, err := svc.Tick(t.Context(), sys, now)
		if err != nil || st.Purged != wantPurged || st.Referenced != wantRefs {
			t.Fatalf("%s: %+v err %v, want purged %d referenced %d", name, st, err, wantPurged, wantRefs)
		}
		for _, id := range []uint64{snap, cmd, rcpt, ckpt} {
			gone := itest.Count(t, db, "SELECT COUNT(*) FROM raw_object WHERE id = ? AND deleted_at IS NOT NULL", id) == 1
			if want := slices.Contains(wantGone, id); gone != want {
				t.Fatalf("%s: raw_object %d tombstoned %v, want %v", name, id, gone, want)
			}
		}
	}
	phase("parents open", 0, 4)
	execSQL(t, db, "UPDATE work_order SET status = ? WHERE id = ?", string(model.OrderDone), order)
	execSQL(t, db, "UPDATE agent_run SET status = ? WHERE id = ?", string(model.RunCompleted), run)
	phase("order and run terminal, ticket still open", 3, 1, cmd, rcpt, ckpt)
	execSQL(t, db, "UPDATE approval_ticket SET status = ? WHERE id = ?", string(model.TicketApproved), ticket)
	phase("ticket closed", 1, 0, snap, cmd, rcpt, ckpt)
}

// exportItAlerts 记录导出触发的告警。
type exportItAlerts struct{ got []model.Alert }

func (a *exportItAlerts) Fire(_ context.Context, al model.Alert) { a.got = append(a.got, al) }

// exportItPutter 代替 MinIO，记录写对象次数（集成用例不连 MinIO）。
type exportItPutter struct{ puts int }

func (p *exportItPutter) PutObject(context.Context, string, string, []byte, string) error {
	p.puts++
	return nil
}

// 必败：删掉导出窗口的末行（窗口外后一行仍在）。只校验 [lo, hi] 时链看似完整、静默少导一行；
// 现在校验到 hi+1，报 GAP → FAILED + P0 告警，不产出对象，同事务写 AUDIT_EXPORT result=FAILED。
func TestAuditExportWindowTailDeleted_Integration(t *testing.T) {
	e := itest.Setup(t, "svcl7a")
	slot := e.Fresh(t, t0)
	appendChain(t, slot.DB, 5) // created_at = t0 + 0..4s；窗口 [t0+1s, t0+4s) 含 seq 2-4
	execSQL(t, slot.DB, `DELETE FROM audit_log WHERE tenant_id = 1 AND farm_id = 1 AND chain_seq = 4`)
	putter := &exportItPutter{}
	alerts := &exportItAlerts{}
	svc, err := service.NewAuditExportService(service.AuditExportDeps{Store: repository.AuditExportRepo{DB: slot.DB},
		Chains: repository.AuditChainRepo{DB: slot.DB}, Raw: repository.NewRawStore(slot.DB, putter, "agri-steward-raw"),
		RawAccess: repository.RawAccessRepo{DB: slot.DB}, Tombstone: repository.RetentionRepo{DB: slot.DB},
		Objects: &memRemover{}, RawBucket: "agri-steward-raw", Presign: itPresign{}, Perms: itPerms(t), Alerts: alerts,
		Clock: slot.Clock, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxSpan: 31 * 24 * time.Hour,
		MaskText: func(s string) string { return s }, MaskJSON: func(b json.RawMessage) json.RawMessage { return b }})
	if err != nil {
		t.Fatal(err)
	}
	auditor := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1003, Roles: []model.Role{model.RoleAuditor}}
	st, err := svc.Request(t.Context(), auditor, service.AuditExportRequest{From: t0.Add(time.Second),
		To: t0.Add(4 * time.Second), RequestID: "it-tail-deleted"})
	if err != nil {
		t.Fatal(err)
	}
	row, err := repository.AuditExportRepo{DB: slot.DB}.Get(t.Context(), auditor, st.ExportID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(row.Payload)
	if err != nil {
		t.Fatal(err)
	}
	msg := repository.OutboxMessage{ID: st.ExportID, TenantID: itest.TenantID, FarmID: itest.FarmID,
		Topic: repository.TopicAuditExport, Payload: body, CreatedAt: t0}
	if err := svc.Handle(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Status(t.Context(), auditor, st.ExportID)
	if err != nil || got.State != model.AuditExportFailed || got.Error != model.AuditExportErrChainBroken {
		t.Fatalf("status %+v err %v, want FAILED/%s", got, err, model.AuditExportErrChainBroken)
	}
	if len(alerts.got) != 1 || alerts.got[0].Name != model.AlertAuditChainBroken || alerts.got[0].Level != model.AlertP0 {
		t.Fatalf("alerts %+v", alerts.got)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM raw_object WHERE kind = ?", string(model.RawExport)); putter.puts != 0 || n != 0 {
		t.Fatalf("puts %d export raw_objects %d, want none", putter.puts, n)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE tenant_id = 1 AND farm_id = 1 AND action = ?
  AND result = ? AND JSON_UNQUOTE(JSON_EXTRACT(summary, '$.error')) = ? AND chain_seq IS NOT NULL`,
		string(model.AuditExport), string(model.AuditResultFailed), model.AuditExportErrChainBroken); n != 1 {
		t.Fatalf("AUDIT_EXPORT FAILED audits = %d, want 1", n)
	}
}
