package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// fakeChain 是内存中的一条链：rows 按 chain_seq 存放，可被测试篡改。
type fakeChain struct {
	head repository.ChainHead
	rows []repository.ChainRow
}

func (f *fakeChain) Head(context.Context, repository.ChainKey) (repository.ChainHead, error) {
	return f.head, nil
}

func (f *fakeChain) Rows(_ context.Context, _ repository.ChainKey, from, to uint64) ([]repository.ChainRow, error) {
	var out []repository.ChainRow
	for _, r := range f.rows {
		if r.ChainSeq >= from && r.ChainSeq <= to {
			out = append(out, r)
		}
	}
	return out, nil
}

// buildChain 用生产哈希函数造一条 n 行的合法链（summary 含中文、浮点、嵌套对象）。
func buildChain(t *testing.T, n int) *fakeChain {
	t.Helper()
	k := repository.ChainKey{TenantID: 1, FarmID: 1}
	prev := repository.GenesisHash
	f := &fakeChain{head: repository.ChainHead{Key: k}}
	at := time.Date(2026, 11, 2, 8, 0, 0, 123456789, time.UTC)
	for i := 1; i <= n; i++ {
		run := uint64(i)
		rec := model.AuditRecord{TenantID: 1, FarmID: 1, RunID: &run, Actor: model.Actor{Type: model.ActorUser, ID: 1001},
			Action: "ASK", Target: "field:1", PayloadDigest: "d", Result: model.AuditResultOK, CreatedAt: at,
			Summary: json.RawMessage(`{"问题":"稻飞虱","剂量":1.50,"嵌套":{"b":[1,2e0],"a":null}}`)}
		row := repository.ChainRow{ID: uint64(100 + i), ChainSeq: uint64(i), PrevHash: prev, Rec: rec}
		h, err := repository.AuditRowHash(prev, row)
		if err != nil {
			t.Fatal(err)
		}
		row.RowHash, prev = h, h
		f.rows = append(f.rows, row)
	}
	f.head.ChainSeq, f.head.HeadHash = uint64(n), prev
	return f
}

func TestVerifyChain(t *testing.T) {
	k := repository.ChainKey{TenantID: 1, FarmID: 1}
	cases := []struct {
		name     string
		tamper   func(f *fakeChain)
		from, to uint64
		want     BreakKind
		wantSeq  uint64
		wantID   uint64
	}{
		{name: "intact"},
		{name: "intact sub range", from: 3, to: 4},
		{name: "summary modified", want: BreakRowModified, wantSeq: 3, wantID: 103,
			tamper: func(f *fakeChain) { f.rows[2].Rec.Summary = json.RawMessage(`{"问题":"稻瘟病"}`) }},
		{name: "summary reformatted by mysql is fine",
			tamper: func(f *fakeChain) {
				f.rows[2].Rec.Summary = json.RawMessage(`{"嵌套": {"a": null, "b": [1, 2]}, "剂量": 1.5, "问题": "稻飞虱"}`)
			}},
		{name: "created_at sub-ms is fine",
			tamper: func(f *fakeChain) { f.rows[0].Rec.CreatedAt = f.rows[0].Rec.CreatedAt.Truncate(time.Millisecond) }},
		{name: "result modified", want: BreakRowModified, wantSeq: 2, wantID: 102,
			tamper: func(f *fakeChain) { f.rows[1].Rec.Result = model.AuditResultDenied }},
		{name: "summary invalid json", want: BreakRowModified, wantSeq: 2, wantID: 102,
			tamper: func(f *fakeChain) { f.rows[1].Rec.Summary = json.RawMessage(`{`) }},
		{name: "row deleted", want: BreakGap, wantSeq: 3, wantID: 104,
			tamper: func(f *fakeChain) { f.rows = append(f.rows[:2], f.rows[3:]...) }},
		{name: "tail deleted", want: BreakGap, wantSeq: 5,
			tamper: func(f *fakeChain) { f.rows = f.rows[:4] }},
		{name: "prev_hash modified", want: BreakLinkBroken, wantSeq: 4, wantID: 104,
			tamper: func(f *fakeChain) { f.rows[3].PrevHash = repository.GenesisHash }},
		{name: "truncated and head rewound is caught by anchor not verify",
			tamper: func(f *fakeChain) {
				f.rows = f.rows[:3]
				f.head.ChainSeq, f.head.HeadHash = 3, f.rows[2].RowHash
			}},
		{name: "head hash mismatch", want: BreakLinkBroken, wantSeq: 5,
			tamper: func(f *fakeChain) { f.head.HeadHash = repository.GenesisHash }},
		{name: "row before range missing", from: 3, want: BreakGap, wantSeq: 2,
			tamper: func(f *fakeChain) { f.rows = append(f.rows[:1], f.rows[2:]...) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := buildChain(t, 5)
			if c.tamper != nil {
				c.tamper(f)
			}
			var visited int
			res, err := VerifyChain(t.Context(), f, k, c.from, c.to, func(repository.ChainRow) error { visited++; return nil })
			if err != nil {
				t.Fatal(err)
			}
			if c.want == "" {
				if res.Break != nil {
					t.Fatalf("unexpected break %s", res.Break)
				}
				if uint64(visited) != res.Rows || res.Rows == 0 {
					t.Fatalf("visited %d rows=%d", visited, res.Rows)
				}
				return
			}
			if res.Break == nil || res.Break.Kind != c.want || res.Break.ChainSeq != c.wantSeq || res.Break.AuditID != c.wantID {
				t.Fatalf("break = %+v, want %s seq=%d id=%d", res.Break, c.want, c.wantSeq, c.wantID)
			}
		})
	}
}

func TestVerifyChain_EmptyChain(t *testing.T) {
	f := &fakeChain{head: repository.ChainHead{HeadHash: repository.GenesisHash}}
	res, err := VerifyChain(t.Context(), f, repository.ChainKey{TenantID: 1, FarmID: 1}, 0, 0, nil)
	if err != nil || res.Break != nil || res.Rows != 0 {
		t.Fatalf("empty chain: %+v %v", res, err)
	}
}

func TestVerifyChain_VisitErrorStops(t *testing.T) {
	boom := errors.New("boom")
	_, err := VerifyChain(t.Context(), buildChain(t, 3), repository.ChainKey{TenantID: 1, FarmID: 1}, 0, 0,
		func(repository.ChainRow) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}
