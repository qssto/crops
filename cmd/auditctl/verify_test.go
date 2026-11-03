package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// memChain 是内存链（按 chain_seq 存放），可被测试篡改。
type memChain struct {
	head repository.ChainHead
	rows []repository.ChainRow
}

func (m *memChain) Head(context.Context, repository.ChainKey) (repository.ChainHead, error) {
	return m.head, nil
}

func (m *memChain) Rows(_ context.Context, _ repository.ChainKey, from, to uint64) ([]repository.ChainRow, error) {
	var out []repository.ChainRow
	for _, r := range m.rows {
		if r.ChainSeq >= from && r.ChainSeq <= to {
			out = append(out, r)
		}
	}
	return out, nil
}

func newMemChain(t *testing.T, n int) *memChain {
	t.Helper()
	k := repository.ChainKey{TenantID: 1, FarmID: 2}
	m := &memChain{head: repository.ChainHead{Key: k}}
	prev := repository.GenesisHash
	at := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		row := repository.ChainRow{ID: uint64(200 + i), ChainSeq: uint64(i), PrevHash: prev, Rec: model.AuditRecord{
			TenantID: 1, FarmID: 2, Actor: model.Actor{Type: model.ActorUser, ID: 9}, Action: "ASK", Target: "field:1",
			PayloadDigest: "d", Result: model.AuditResultOK, CreatedAt: at}}
		h, err := repository.AuditRowHash(prev, row)
		if err != nil {
			t.Fatal(err)
		}
		row.RowHash, prev = h, h
		m.rows = append(m.rows, row)
	}
	m.head.ChainSeq, m.head.HeadHash = uint64(n), prev
	return m
}

type fixedTenants map[uint64][]uint64

func (f fixedTenants) FarmTenants(_ context.Context, farm uint64) ([]uint64, error) {
	return f[farm], nil
}

func TestResolveChain(t *testing.T) {
	tenants := fixedTenants{2: {1}, 3: {1, 4}}
	cases := []struct {
		name    string
		o       verifyOpts
		want    repository.ChainKey
		wantErr string
	}{
		{name: "explicit tenant", o: verifyOpts{Farm: 3, Tenant: 4}, want: repository.ChainKey{TenantID: 4, FarmID: 3}},
		{name: "platform chain", o: verifyOpts{Farm: 0, Tenant: -1}},
		{name: "single tenant", o: verifyOpts{Farm: 2, Tenant: -1}, want: repository.ChainKey{TenantID: 1, FarmID: 2}},
		{name: "multi tenant needs flag", o: verifyOpts{Farm: 3, Tenant: -1}, wantErr: "pass --tenant"},
		{name: "no binding", o: verifyOpts{Farm: 9, Tenant: -1}, wantErr: "no tenant binding"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, err := resolveChain(t.Context(), tenants, c.o)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || k != c.want {
				t.Fatalf("key %+v err %v, want %+v", k, err, c.want)
			}
		})
	}
}

func TestRunVerify(t *testing.T) {
	cases := []struct {
		name    string
		tamper  func(m *memChain)
		from    uint64
		want    string
		wantErr error
	}{
		{name: "intact", want: "OK chain (1,2) range [1,4] rows=4"},
		{name: "row modified", want: "ROW_MODIFIED", wantErr: errBroken,
			tamper: func(m *memChain) { m.rows[1].Rec.Target = "field:2" }},
		{name: "row deleted", want: "GAP", wantErr: errBroken,
			tamper: func(m *memChain) { m.rows = append(m.rows[:2], m.rows[3:]...) }},
		{name: "link broken", want: "LINK_BROKEN", wantErr: errBroken,
			tamper: func(m *memChain) { m.rows[2].PrevHash = repository.GenesisHash }},
		{name: "from-seq at head is ok", from: 4, want: "OK chain (1,2) range [4,4] rows=1"},
		{name: "from-seq beyond head is usage error", from: 5, wantErr: errUsage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMemChain(t, 4)
			if c.tamper != nil {
				c.tamper(m)
			}
			var out bytes.Buffer
			err := runVerify(t.Context(), m, fixedTenants{2: {1}}, verifyOpts{Farm: 2, Tenant: -1, FromSeq: c.from}, &out)
			if c.wantErr == nil && err != nil || c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Fatalf("out %q, want %q", out.String(), c.want)
			}
		})
	}
}

func TestRunAnchorCheck(t *testing.T) {
	at := time.Date(2026, 11, 1, 16, 20, 0, 0, time.UTC)
	cases := []struct {
		name     string
		date     string
		versions int
		tamper   func(m *memChain, body []byte) []byte
		broken   bool
		want     string
	}{
		{name: "intact", date: "20261102", want: "OK anchor 20261102 chains=1"},
		{name: "rewritten anchor object", date: "20261102", versions: 2, broken: true, want: "has 2 versions"},
		{name: "anchored row content modified, row_hash kept", date: "20261102", broken: true,
			want:   string(service.AnchorRecomputed),
			tamper: func(m *memChain, b []byte) []byte { m.rows[3].Rec.Target = "field:9"; return b }},
		{name: "truncated and recomputed", date: "20261102", broken: true, want: string(service.AnchorRecomputed),
			tamper: func(m *memChain, b []byte) []byte { m.rows[3].RowHash = repository.GenesisHash; return b }},
		{name: "truncated", date: "20261102", broken: true, want: string(service.AnchorTruncated),
			tamper: func(m *memChain, b []byte) []byte {
				m.rows = m.rows[:2]
				m.head.ChainSeq, m.head.HeadHash = 2, m.rows[1].RowHash
				return b
			}},
		{name: "anchor file tampered", date: "20261102", broken: true, want: "BROKEN anchor 20261102",
			tamper: func(m *memChain, b []byte) []byte {
				return []byte(strings.Replace(string(b), m.head.HeadHash, repository.GenesisHash, 1))
			}},
		{name: "date mismatch", date: "20261103", broken: true, want: "BROKEN anchor file date"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMemChain(t, 4)
			_, body, err := service.BuildAnchor("20261102", []repository.ChainHead{m.head}, at)
			if err != nil {
				t.Fatal(err)
			}
			if c.tamper != nil {
				body = c.tamper(m, body)
			}
			var out bytes.Buffer
			err = runAnchorCheck(t.Context(), m, c.date, body, max(c.versions, 1), &out)
			if c.broken != errors.Is(err, errBroken) || !c.broken && err != nil {
				t.Fatalf("err = %v, broken %v", err, c.broken)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Fatalf("out %q, want %q", out.String(), c.want)
			}
		})
	}
}

func TestRunUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "no args", want: exitUsage},
		{name: "unknown command", args: []string{"repair"}, want: exitUsage},
		{name: "from-manifest not enabled", args: []string{"verify", "--from-manifest", "m.json"}, want: exitUsage},
		{name: "farm required", args: []string{"verify"}, want: exitUsage},
		{name: "from after to", args: []string{"verify", "--farm", "1", "--from-seq", "5", "--to-seq", "2"}, want: exitUsage},
		{name: "bad date", args: []string{"anchor-check", "--date", "2026-11-02"}, want: exitUsage},
		{name: "unknown flag", args: []string{"verify", "--repair"}, want: exitUsage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := run(c.args, &out, &errOut); got != c.want {
				t.Fatalf("exit = %d, want %d (stderr %q)", got, c.want, errOut.String())
			}
		})
	}
}
