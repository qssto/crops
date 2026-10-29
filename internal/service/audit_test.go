package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

var rawNow = time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

// memRawAccess 是内存原文仓储：rows 按 id 索引（已按 scope 过滤），txAudits / audits 分别记录同事务与独立写的审计。
type memRawAccess struct {
	rows      map[uint64]repository.RawObjectRow
	txAudits  []model.AuditRecord
	audits    []model.AuditRecord
	committed bool
}

func (m *memRawAccess) InTx(_ context.Context, fn func(*repository.DB) error) error {
	staged := len(m.txAudits)
	if err := fn(nil); err != nil {
		m.txAudits = m.txAudits[:staged] // 回滚
		return err
	}
	m.committed = true
	return nil
}

func (m *memRawAccess) RawTx(_ context.Context, _ *repository.DB, _ model.Scope, id uint64) (repository.RawObjectRow, error) {
	r, ok := m.rows[id]
	if !ok {
		return repository.RawObjectRow{}, model.ErrNotFound
	}
	return r, nil
}

func (m *memRawAccess) AuditTx(_ context.Context, _ *repository.DB, rec model.AuditRecord) error {
	m.txAudits = append(m.txAudits, rec)
	return nil
}

func (m *memRawAccess) Audit(_ context.Context, rec model.AuditRecord) error {
	m.audits = append(m.audits, rec)
	return nil
}

type fakePresign struct{ keys []string }

func (p *fakePresign) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	if ttl != RawReadTTL {
		return "", errors.New("unexpected ttl")
	}
	p.keys = append(p.keys, key)
	return "https://minio.local/" + key + "?sig=x", nil
}

func TestAuditService_ReadRaw(t *testing.T) {
	agronomist := model.Scope{TenantID: 1, FarmID: 2, UserID: 30, Roles: []model.Role{model.RoleAgronomist}}
	operator := model.Scope{TenantID: 1, FarmID: 2, UserID: 31, Roles: []model.Role{model.RoleOperator}}
	tests := []struct {
		name      string
		scope     model.Scope
		runID     uint64
		rawID     uint64
		wantErr   error
		wantCode  string // DENIED 审计的 code；空表示成功
		wantRawID bool   // 审计行带 raw_object_id
	}{
		{name: "authorized", scope: agronomist, runID: 9, rawID: 101, wantRawID: true},
		{name: "operator denied", scope: operator, runID: 9, rawID: 101, wantErr: ErrForbidden, wantCode: RawDenyRole},
		{name: "cross run", scope: agronomist, runID: 8, rawID: 101, wantErr: ErrForbidden, wantCode: RawDenyRunMismatch,
			wantRawID: true},
		{name: "other farm or missing", scope: agronomist, runID: 9, rawID: 999, wantErr: ErrForbidden,
			wantCode: RawDenyNotFound},
		{name: "zero ids", scope: agronomist, wantErr: ErrBadInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &memRawAccess{rows: map[uint64]repository.RawObjectRow{
				101: {ID: 101, RunID: 9, Kind: model.RawLLMIO, ObjectKey: "raw/1/2/2026/10/29/9/LLM_IO/x"}}}
			pre := &fakePresign{}
			svc, err := NewAuditService(AuditDeps{Store: store, Presign: pre, Clock: fixedClock{rawNow}})
			if err != nil {
				t.Fatal(err)
			}
			link, err := svc.ReadRaw(context.Background(), tt.scope, tt.runID, tt.rawID)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == ErrBadInput {
				if len(store.audits)+len(store.txAudits) != 0 {
					t.Fatalf("bad input must not audit: %+v %+v", store.audits, store.txAudits)
				}
				return
			}
			var rec model.AuditRecord
			if tt.wantErr == nil {
				if len(store.txAudits) != 1 || len(store.audits) != 0 || !store.committed {
					t.Fatalf("want one in-tx audit: tx=%+v sep=%+v", store.txAudits, store.audits)
				}
				rec = store.txAudits[0]
				if link.URL == "" || !link.ExpiresAt.Equal(rawNow.Add(RawReadTTL)) || len(pre.keys) != 1 {
					t.Fatalf("link = %+v presigned = %v", link, pre.keys)
				}
			} else {
				if len(store.audits) != 1 || len(store.txAudits) != 0 || len(pre.keys) != 0 || link.URL != "" {
					t.Fatalf("want one separate audit, no URL: tx=%+v sep=%+v keys=%v", store.txAudits, store.audits, pre.keys)
				}
				rec = store.audits[0]
			}
			wantResult := model.AuditResultOK
			if tt.wantCode != "" {
				wantResult = model.AuditResultDenied
			}
			if rec.Action != string(model.AuditRawRead) || rec.Result != wantResult || rec.Target != "raw:"+strconv.FormatUint(tt.rawID, 10) ||
				rec.Actor != (model.Actor{Type: model.ActorUser, ID: tt.scope.UserID}) || rec.RunID == nil ||
				*rec.RunID != tt.runID || (rec.RawObjectID != nil) != tt.wantRawID || len(rec.PayloadDigest) != 64 {
				t.Fatalf("audit = %+v", rec)
			}
			var p struct {
				Code string `json:"code"`
				URL  string `json:"url"`
			}
			if err := json.Unmarshal(rec.Summary, &p); err != nil {
				t.Fatal(err)
			}
			if p.Code != tt.wantCode || p.URL != "" {
				t.Fatalf("summary = %s", rec.Summary)
			}
		})
	}
}
