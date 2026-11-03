package service

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// memExports 是内存版导出 outbox：Get 按 (tenant, farm) 过滤，他场即 not found。
type memExports struct {
	rows   map[uint64]repository.AuditExportRow
	scopes map[uint64]model.Scope
	next   uint64
}

func (m *memExports) InsertTx(_ context.Context, _ *repository.DB, scope model.Scope, p model.AuditExportPayload,
	now time.Time) (uint64, bool, error) {
	for id, r := range m.rows {
		if m.scopes[id].FarmID == scope.FarmID && r.Payload.RequestID == p.RequestID {
			return id, false, nil
		}
	}
	m.next++
	m.rows[m.next] = repository.AuditExportRow{ID: m.next, Status: "PENDING", Payload: p, CreatedAt: now, UpdatedAt: now}
	m.scopes[m.next] = scope
	return m.next, true, nil
}

func (m *memExports) Get(_ context.Context, scope model.Scope, id uint64) (repository.AuditExportRow, error) {
	r, ok := m.rows[id]
	if !ok || m.scopes[id].TenantID != scope.TenantID || m.scopes[id].FarmID != scope.FarmID {
		return repository.AuditExportRow{}, model.ErrNotFound
	}
	return r, nil
}

func (m *memExports) SetPayloadTx(_ context.Context, _ *repository.DB, scope model.Scope, id uint64,
	p model.AuditExportPayload, now time.Time) error {
	r, err := m.Get(context.Background(), scope, id)
	if err != nil {
		return err
	}
	r.Payload, r.UpdatedAt = p, now
	m.rows[id] = r
	return nil
}

// exportChains 给 fakeChain 加 SeqRange（按 created_at 落在 [from, to) 的最小 / 最大 chain_seq）。
type exportChains struct{ *fakeChain }

func (c exportChains) SeqRange(_ context.Context, _ repository.ChainKey, from, to time.Time) (uint64, uint64, bool, error) {
	var lo, hi uint64
	for _, r := range c.rows {
		if r.Rec.CreatedAt.Before(from) || !r.Rec.CreatedAt.Before(to) {
			continue
		}
		if lo == 0 || r.ChainSeq < lo {
			lo = r.ChainSeq
		}
		hi = max(hi, r.ChainSeq)
	}
	return lo, hi, lo != 0, nil
}

func (c exportChains) SeqBefore(_ context.Context, _ repository.ChainKey, t time.Time) (uint64, error) {
	var seq uint64
	for _, r := range c.rows {
		if r.Rec.CreatedAt.Before(t) {
			seq = max(seq, r.ChainSeq)
		}
	}
	return seq, nil
}

// exportObjects 记录 RawStore.Put、删对象与墓碑。
type exportObjects struct {
	puts      []model.RawPut
	removed   []string
	removeErr error
	tombs     []uint64
}

func (o *exportObjects) Put(_ context.Context, p model.RawPut) (model.RawObject, error) {
	o.puts = append(o.puts, p)
	return model.RawObject{ID: 501, TenantID: p.TenantID, FarmID: p.FarmID, Kind: p.Kind, ObjectKey: "raw/export/501"}, nil
}

func (o *exportObjects) RemoveObject(_ context.Context, _, key string) error {
	if o.removeErr != nil {
		return o.removeErr
	}
	o.removed = append(o.removed, key)
	return nil
}

func (o *exportObjects) MarkDeletedTx(_ context.Context, _ *repository.DB, ref repository.RawRef, _ time.Time) (bool, error) {
	o.tombs = append(o.tombs, ref.ID)
	return true, nil
}

const exportPhone = "13812345678"

// maskPhone 是测试用 pii_mask：只掩手机号。
func maskPhone(s string) string { return strings.ReplaceAll(s, exportPhone, "138****5678") }

var (
	exportNow     = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	exportAuditor = model.Scope{TenantID: 1, FarmID: 1, UserID: 77, Roles: []model.Role{model.RoleAuditor}}
)

type exportFixture struct {
	svc    *AuditExportService
	chain  *fakeChain
	store  *memExports
	raw    *memRawAccess
	objs   *exportObjects
	alerts *recAlerts
}

func newExportFixture(t *testing.T, maskText func(string) string) *exportFixture {
	t.Helper()
	f := &exportFixture{chain: buildChain(t, 3), store: &memExports{rows: map[uint64]repository.AuditExportRow{},
		scopes: map[uint64]model.Scope{}}, raw: &memRawAccess{rows: map[uint64]repository.RawObjectRow{}},
		objs: &exportObjects{}, alerts: &recAlerts{}}
	f.chain.rows[1].Rec.Target = "phone:" + exportPhone // 重算哈希使链仍合法
	prev := repository.GenesisHash
	for i := range f.chain.rows {
		f.chain.rows[i].PrevHash = prev
		h, err := repository.AuditRowHash(prev, f.chain.rows[i])
		if err != nil {
			t.Fatal(err)
		}
		f.chain.rows[i].RowHash, prev = h, h
	}
	f.chain.head.HeadHash = prev
	svc, err := NewAuditExportService(AuditExportDeps{Store: f.store, Chains: exportChains{f.chain}, Raw: f.objs,
		RawAccess: f.raw, Tombstone: f.objs, Objects: f.objs, RawBucket: "raw", Presign: &fakePresign{},
		Perms: testPerms(t), Alerts: f.alerts, Clock: fixedClock{exportNow}, Logger: discardLogger(),
		MaxSpan: 31 * 24 * time.Hour, MaskText: maskText, MaskJSON: func(b json.RawMessage) json.RawMessage { return b }})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

// request 写一条 PENDING 导出并返回对应的 outbox 消息。
func (f *exportFixture) request(t *testing.T) repository.OutboxMessage {
	t.Helper()
	st, err := f.svc.Request(t.Context(), exportAuditor, AuditExportRequest{From: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
		To: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), RequestID: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(f.store.rows[st.ExportID].Payload)
	if err != nil {
		t.Fatal(err)
	}
	return repository.OutboxMessage{ID: st.ExportID, TenantID: 1, FarmID: 1, Topic: repository.TopicAuditExport, Payload: body}
}

func (f *exportFixture) payload(id uint64) model.AuditExportPayload { return f.store.rows[id].Payload }

// unzipEntry 读出导出包内某个条目。
func unzipEntry(t *testing.T, body []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range zr.File {
		if e.Name != name {
			continue
		}
		rc, err := e.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		if cerr := rc.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Fatalf("entry %s missing", name)
	return ""
}

func TestAuditExportHandle(t *testing.T) {
	t.Run("masked export done and downloadable", func(t *testing.T) {
		f := newExportFixture(t, maskPhone)
		msg := f.request(t)
		if err := f.svc.Handle(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
		p := f.payload(msg.ID)
		if p.State != model.AuditExportDone || p.Rows != 3 || p.RawObjectID != 501 || p.ChainFrom != 1 || p.ChainTo != 3 {
			t.Fatalf("payload %+v", p)
		}
		if len(f.objs.puts) != 1 || f.objs.puts[0].Kind != model.RawExport || f.objs.puts[0].RunID != 0 {
			t.Fatalf("puts %+v", f.objs.puts)
		}
		lines := unzipEntry(t, f.objs.puts[0].Body, exportLogEntry)
		if strings.Count(lines, "\n") != 3 || strings.Contains(lines, exportPhone) || !strings.Contains(lines, "138****5678") {
			t.Fatalf("lines %s", lines)
		}
		if m := unzipEntry(t, f.objs.puts[0].Body, exportManifestEntry); !strings.Contains(m, `"chain_verified": true`) {
			t.Fatalf("manifest %s", m)
		}
		if len(f.raw.txAudits) != 1 || f.raw.txAudits[0].Action != string(model.AuditExport) ||
			f.raw.txAudits[0].RawObjectID == nil || *f.raw.txAudits[0].RawObjectID != 501 {
			t.Fatalf("export audit %+v", f.raw.txAudits)
		}
		// 下载：签发 5 分钟 URL，同事务写 RAW_READ。
		f.raw.rows[501] = repository.RawObjectRow{ID: 501, Kind: model.RawExport, ObjectKey: "raw/export/501"}
		st, err := f.svc.Status(t.Context(), exportAuditor, msg.ID)
		if err != nil || st.URL == "" || st.ExpiresAt == nil || !st.ExpiresAt.Equal(exportNow.Add(RawReadTTL)) {
			t.Fatalf("status %+v err %v", st, err)
		}
		if len(f.raw.txAudits) != 2 || f.raw.txAudits[1].Action != string(model.AuditRawRead) ||
			strings.Contains(string(f.raw.txAudits[1].Summary), "sig=") {
			t.Fatalf("download audit %+v", f.raw.txAudits)
		}
		if len(f.alerts.got) != 0 {
			t.Fatalf("alerts %+v", f.alerts.got)
		}
		// 重放终态消息：不重新生成。
		body, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		msg.Payload = body
		if err := f.svc.Handle(t.Context(), msg); err != nil || len(f.objs.puts) != 1 {
			t.Fatalf("replay err %v puts %d", err, len(f.objs.puts))
		}
	})
	t.Run("chain broken fails without file and fires P0", func(t *testing.T) {
		f := newExportFixture(t, maskPhone)
		f.chain.rows[2].Rec.Result = model.AuditResultDenied // 必败：改行不重算
		msg := f.request(t)
		if err := f.svc.Handle(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
		if p := f.payload(msg.ID); p.State != model.AuditExportFailed || p.Error != model.AuditExportErrChainBroken {
			t.Fatalf("payload %+v", p)
		}
		if names := f.alerts.names(); len(f.objs.puts) != 0 || len(names) != 1 || names[0] != model.AlertAuditChainBroken ||
			f.alerts.got[0].Level != model.AlertP0 {
			t.Fatalf("puts %d alerts %+v", len(f.objs.puts), f.alerts.got)
		}
		assertFailedAudit(t, f, model.AuditExportErrChainBroken)
	})
	t.Run("pii leak deletes object and fires P0", func(t *testing.T) {
		f := newExportFixture(t, func(s string) string { return s }) // 必败：漏掩码
		msg := f.request(t)
		if err := f.svc.Handle(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
		p := f.payload(msg.ID)
		if p.State != model.AuditExportFailed || p.Error != model.AuditExportErrPIILeak {
			t.Fatalf("payload %+v", p)
		}
		if len(f.objs.removed) != 1 || len(f.objs.tombs) != 1 || f.objs.tombs[0] != 501 {
			t.Fatalf("removed %v tombs %v", f.objs.removed, f.objs.tombs)
		}
		if names := f.alerts.names(); len(names) != 1 || names[0] != model.AlertAuditExportPIILeak ||
			strings.Contains(f.alerts.got[0].Detail, exportPhone) {
			t.Fatalf("alerts %+v", f.alerts.got)
		}
		assertFailedAudit(t, f, model.AuditExportErrPIILeak)
		if strings.Contains(string(f.raw.txAudits[0].Summary), exportPhone) {
			t.Fatalf("failed audit leaks pii: %s", f.raw.txAudits[0].Summary)
		}
	})
	t.Run("leak remove failure retries", func(t *testing.T) {
		f := newExportFixture(t, func(s string) string { return s })
		f.objs.removeErr = errors.New("minio down")
		msg := f.request(t)
		if err := f.svc.Handle(t.Context(), msg); err == nil {
			t.Fatal("want error for retry")
		}
		if p := f.payload(msg.ID); p.State != model.AuditExportPending || len(f.alerts.got) != 0 {
			t.Fatalf("payload %+v alerts %+v", p, f.alerts.got)
		}
	})
	t.Run("empty range exports zero rows", func(t *testing.T) {
		f := newExportFixture(t, maskPhone)
		f.chain.rows, f.chain.head = nil, repository.ChainHead{HeadHash: repository.GenesisHash}
		msg := f.request(t)
		if err := f.svc.Handle(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
		if p := f.payload(msg.ID); p.State != model.AuditExportDone || p.Rows != 0 || len(f.objs.puts) != 1 {
			t.Fatalf("payload %+v puts %d", p, len(f.objs.puts))
		}
	})
	t.Run("object key dated by enqueue time", func(t *testing.T) {
		f := newExportFixture(t, maskPhone)
		msg := f.request(t)
		msg.CreatedAt = exportNow.AddDate(0, 0, -2) // 跨日重放：key 仍按入队日
		if err := f.svc.Handle(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
		if len(f.objs.puts) != 1 || !f.objs.puts[0].At.Equal(msg.CreatedAt) {
			t.Fatalf("puts %+v", f.objs.puts)
		}
	})
}

// assertFailedAudit 断言写了且只写了一条 AUDIT_EXPORT result=FAILED，summary 带原因码。
func assertFailedAudit(t *testing.T, f *exportFixture, code string) {
	t.Helper()
	if len(f.raw.txAudits) != 1 {
		t.Fatalf("audits %+v", f.raw.txAudits)
	}
	a := f.raw.txAudits[0]
	if a.Action != string(model.AuditExport) || a.Result != model.AuditResultFailed ||
		!strings.Contains(string(a.Summary), `"error":"`+code+`"`) {
		t.Fatalf("failed audit %+v summary %s", a, a.Summary)
	}
}

// TestAuditExportWindowEdges 覆盖窗口边界被篡改：只校验 [lo, hi] 时这些都会静默漏行。
func TestAuditExportWindowEdges(t *testing.T) {
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	// 5 行链：1、5 在窗口外（前一日 / 后一日），2-4 在窗口 [11-02, 11-03) 内。
	setTimes := func(f *exportFixture) {
		at := []time.Time{day.Add(-time.Hour), day.Add(8 * time.Hour), day.Add(9 * time.Hour), day.Add(10 * time.Hour),
			day.Add(25 * time.Hour)}
		for i := range f.chain.rows {
			f.chain.rows[i].Rec.CreatedAt = at[i]
		}
	}
	cases := []struct {
		name   string
		tamper func(f *exportFixture)
		broken bool
		rows   uint64
	}{
		{name: "intact", rows: 3},
		{name: "window last row deleted", broken: true, tamper: func(f *exportFixture) {
			f.chain.rows = append(f.chain.rows[:3], f.chain.rows[4:]...)
		}},
		{name: "window first row deleted", broken: true, tamper: func(f *exportFixture) {
			f.chain.rows = append(f.chain.rows[:1], f.chain.rows[2:]...)
		}},
		{name: "window last row moved out (created_at changed)", broken: true, tamper: func(f *exportFixture) {
			f.chain.rows[3].Rec.CreatedAt = day.Add(26 * time.Hour)
		}},
		{name: "whole window deleted", broken: true, tamper: func(f *exportFixture) {
			f.chain.rows = []repository.ChainRow{f.chain.rows[0], f.chain.rows[4]}
		}},
		{name: "window is chain tail and deleted", broken: true, tamper: func(f *exportFixture) {
			f.chain.rows = f.chain.rows[:1] // head 仍为 5
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newExportFixture(t, maskPhone)
			g := buildChain(t, 5)
			f.chain.rows, f.chain.head = g.rows, g.head
			setTimes(f)
			rehash(t, f.chain)
			if c.tamper != nil {
				c.tamper(f)
			}
			msg := f.request(t)
			if err := f.svc.Handle(t.Context(), msg); err != nil {
				t.Fatal(err)
			}
			p := f.payload(msg.ID)
			if !c.broken {
				if p.State != model.AuditExportDone || p.Rows != c.rows || len(f.objs.puts) != 1 {
					t.Fatalf("payload %+v puts %d", p, len(f.objs.puts))
				}
				return
			}
			if p.State != model.AuditExportFailed || p.Error != model.AuditExportErrChainBroken || len(f.objs.puts) != 0 {
				t.Fatalf("payload %+v puts %d", p, len(f.objs.puts))
			}
			if names := f.alerts.names(); len(names) != 1 || names[0] != model.AlertAuditChainBroken {
				t.Fatalf("alerts %+v", f.alerts.got)
			}
		})
	}
}

// rehash 按当前行内容重算整条链（造合法链后再篡改）。
func rehash(t *testing.T, c *fakeChain) {
	t.Helper()
	prev := repository.GenesisHash
	for i := range c.rows {
		c.rows[i].PrevHash = prev
		h, err := repository.AuditRowHash(prev, c.rows[i])
		if err != nil {
			t.Fatal(err)
		}
		c.rows[i].RowHash, prev = h, h
	}
	c.head.HeadHash = prev
}
func TestAuditExportRequest(t *testing.T) {
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	admin := exportAuditor
	admin.Roles = []model.Role{model.RoleAdmin}
	platform := exportAuditor
	platform.Roles = []model.Role{model.RolePlatformAdmin}
	cases := []struct {
		name  string
		scope model.Scope
		req   AuditExportRequest
		want  error
	}{
		{name: "auditor ok", scope: exportAuditor, req: AuditExportRequest{From: day, To: day.AddDate(0, 0, 31), RequestID: "r-1"}},
		{name: "platform_admin ok", scope: platform, req: AuditExportRequest{From: day, To: day.Add(time.Hour), RequestID: "r-1"}},
		{name: "admin forbidden", scope: admin, want: ErrForbidden,
			req: AuditExportRequest{From: day, To: day.Add(time.Hour), RequestID: "r-1"}},
		{name: "span over max", scope: exportAuditor, want: ErrBadInput,
			req: AuditExportRequest{From: day, To: day.AddDate(0, 0, 31).Add(time.Millisecond), RequestID: "r-1"}},
		{name: "from not before to", scope: exportAuditor, want: ErrBadInput,
			req: AuditExportRequest{From: day, To: day, RequestID: "r-1"}},
		{name: "missing to", scope: exportAuditor, want: ErrBadInput, req: AuditExportRequest{From: day, RequestID: "r-1"}},
		{name: "bad request_id", scope: exportAuditor, want: ErrBadInput,
			req: AuditExportRequest{From: day, To: day.Add(time.Hour), RequestID: "a b"}},
		{name: "empty request_id", scope: exportAuditor, want: ErrBadInput,
			req: AuditExportRequest{From: day, To: day.Add(time.Hour)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newExportFixture(t, maskPhone)
			st, err := f.svc.Request(t.Context(), c.scope, c.req)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.want != nil {
				if len(f.store.rows) != 0 {
					t.Fatalf("rejected request wrote outbox: %+v", f.store.rows)
				}
				return
			}
			if st.State != model.AuditExportPending || st.ExportID == 0 {
				t.Fatalf("status %+v", st)
			}
			again, err := f.svc.Request(t.Context(), c.scope, c.req) // 同 request_id 幂等
			if err != nil || again.ExportID != st.ExportID || len(f.store.rows) != 1 {
				t.Fatalf("replay %+v err %v rows %d", again, err, len(f.store.rows))
			}
		})
	}
}

func TestAuditExportStatus(t *testing.T) {
	t.Run("other farm is not found", func(t *testing.T) {
		f := newExportFixture(t, maskPhone)
		msg := f.request(t)
		other := exportAuditor
		other.FarmID = 2
		if _, err := f.svc.Status(t.Context(), other, msg.ID); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("forbidden role", func(t *testing.T) {
		f := newExportFixture(t, maskPhone)
		msg := f.request(t)
		farmer := exportAuditor
		farmer.Roles = []model.Role{model.RoleFarmer}
		if _, err := f.svc.Status(t.Context(), farmer, msg.ID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})
	t.Run("expired file", func(t *testing.T) {
		f := newExportFixture(t, maskPhone)
		msg := f.request(t)
		if err := f.svc.Handle(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
		f.raw.rows[501] = repository.RawObjectRow{ID: 501, Kind: model.RawExport, ObjectKey: "raw/export/501"}
		f.raw.expired = map[uint64]bool{501: true}
		audits := len(f.raw.txAudits)
		st, err := f.svc.Status(t.Context(), exportAuditor, msg.ID)
		if !errors.Is(err, model.ErrRawExpired) || st.URL != "" || len(f.raw.txAudits) != audits {
			t.Fatalf("err %v status %+v audits %d", err, st, len(f.raw.txAudits))
		}
	})
	t.Run("dead outbox reports retry exhausted", func(t *testing.T) {
		f := newExportFixture(t, maskPhone)
		msg := f.request(t)
		r := f.store.rows[msg.ID]
		r.Status = "DEAD"
		f.store.rows[msg.ID] = r
		st, err := f.svc.Status(t.Context(), exportAuditor, msg.ID)
		if err != nil || st.State != model.AuditExportFailed || st.Error != "RETRY_EXHAUSTED" || st.URL != "" {
			t.Fatalf("status %+v err %v", st, err)
		}
	})
}
