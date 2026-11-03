package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// recAlerts 记录 Fire 调用。
type recAlerts struct {
	mu  sync.Mutex
	got []model.Alert
}

func (r *recAlerts) Fire(_ context.Context, a model.Alert) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, a)
}

func (r *recAlerts) names() []model.AlertName {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]model.AlertName, 0, len(r.got))
	for _, a := range r.got {
		out = append(out, a.Name)
	}
	return out
}

// memAnchors 是内存版 AnchorStore；versions 记每个 key 的额外版本数（模拟被重写）。
type memAnchors struct {
	objs     map[string][]byte
	versions map[string]int
	putErr   error
	puts     int
}

func (m *memAnchors) PutAnchor(_ context.Context, key string, body []byte) error {
	if m.putErr != nil {
		return m.putErr
	}
	m.puts++
	m.objs[key] = append([]byte(nil), body...)
	return nil
}

func (m *memAnchors) GetAnchor(_ context.Context, key string) (repository.AnchorObject, error) {
	b, ok := m.objs[key]
	if !ok {
		return repository.AnchorObject{}, repository.ErrAnchorNotFound
	}
	return repository.AnchorObject{Body: b, Versions: 1 + m.versions[key]}, nil
}

// anchorChains 把 fakeChain 包成 AnchorChains（单链 (1,1)）。
type anchorChains struct{ *fakeChain }

func (a anchorChains) Heads(context.Context, repository.SystemScope) ([]repository.ChainHead, error) {
	return []repository.ChainHead{a.head}, nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// anchorAt 是 2026-11-02 北京时间 00:20（UTC 前一日 16:20）。
var anchorAt = time.Date(2026, 11, 1, 16, 20, 0, 0, time.UTC)

func TestBuildParseAnchor(t *testing.T) {
	f := buildChain(t, 3)
	a, body, err := BuildAnchor("20261102", []repository.ChainHead{f.head}, anchorAt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseAnchor(body)
	if err != nil || got.SHA256 != a.SHA256 || len(got.Chains) != 1 || got.Chains[0].ChainSeq != 3 {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	// 必败用例：改锚点里的 head_hash 而不重算整体 sha256。
	tampered := strings.Replace(string(body), f.head.HeadHash, repository.GenesisHash, 1)
	if _, err := ParseAnchor([]byte(tampered)); !errors.Is(err, ErrAnchorDigest) {
		t.Fatalf("tampered anchor err = %v, want ErrAnchorDigest", err)
	}
}

func TestCheckAnchor(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(f *fakeChain)
		want   AnchorMismatchKind
	}{
		{name: "intact"},
		{name: "appended after anchor is fine", tamper: func(f *fakeChain) {
			g := buildChain(t, 6)
			f.rows, f.head = g.rows, g.head
		}},
		{name: "truncated", want: AnchorTruncated, tamper: func(f *fakeChain) {
			f.rows = f.rows[:3]
			f.head.ChainSeq, f.head.HeadHash = 3, f.rows[2].RowHash
		}},
		// 截尾后重算：攻击者删掉第 4 行起再追加新内容并重算哈希，head 序号不变但锚定位置 row_hash 不同。
		{name: "truncated and recomputed", want: AnchorRecomputed, tamper: func(f *fakeChain) {
			f.rows[3].RowHash = repository.GenesisHash
		}},
		{name: "anchored row deleted", want: AnchorTruncated, tamper: func(f *fakeChain) { f.rows = f.rows[:3] }},
		// 必败：改锚定行内容但保留存储的 row_hash（只比存储值会漏掉）。
		{name: "anchored row content modified, row_hash kept", want: AnchorRecomputed, tamper: func(f *fakeChain) {
			f.rows[3].Rec.Target = "field:9"
		}},
		{name: "anchored row summary not canonicalizable", want: AnchorRecomputed, tamper: func(f *fakeChain) {
			f.rows[3].Rec.Summary = json.RawMessage(`{"a":1e999}`)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := buildChain(t, 4)
			a, _, err := BuildAnchor("20261102", []repository.ChainHead{f.head}, anchorAt)
			if err != nil {
				t.Fatal(err)
			}
			if c.tamper != nil {
				c.tamper(f)
			}
			ms, err := CheckAnchor(t.Context(), f, a)
			if err != nil {
				t.Fatal(err)
			}
			if c.want == "" && len(ms) != 0 || c.want != "" && (len(ms) != 1 || ms[0].Kind != c.want || ms[0].ChainSeq != 4) {
				t.Fatalf("mismatches = %+v, want %q", ms, c.want)
			}
		})
	}
}

func newAnchorFixture(t *testing.T, n int) (*AnchorService, *fakeChain, *memAnchors, *recAlerts) {
	t.Helper()
	f := buildChain(t, n)
	store := &memAnchors{objs: map[string][]byte{}}
	alerts := &recAlerts{}
	return &AnchorService{Chains: anchorChains{f}, Store: store, Alerts: alerts, Logger: discardLogger()}, f, store, alerts
}

func TestAnchorTick(t *testing.T) {
	t.Run("before 00:10 does nothing", func(t *testing.T) {
		s, _, store, _ := newAnchorFixture(t, 2)
		if err := s.Tick(t.Context(), scanSystem, time.Date(2026, 11, 1, 16, 5, 0, 0, time.UTC)); err != nil || store.puts != 0 {
			t.Fatalf("err %v puts %d", err, store.puts)
		}
	})
	t.Run("writes anchor and verify mark once", func(t *testing.T) {
		s, _, store, alerts := newAnchorFixture(t, 3)
		for range 2 {
			if err := s.Tick(t.Context(), scanSystem, anchorAt); err != nil {
				t.Fatal(err)
			}
		}
		if store.puts != 2 || store.objs[AnchorKey("20261102")] == nil || store.objs[AnchorVerifyKey("20261102")] == nil {
			t.Fatalf("puts %d keys %v", store.puts, store.objs)
		}
		if len(alerts.got) != 0 {
			t.Fatalf("alerts %+v", alerts.got)
		}
		// 另一个进程（done 未记）：锚点与标记都已存在，不重写（幂等）。
		s2 := &AnchorService{Chains: s.Chains, Store: store, Alerts: alerts, Logger: discardLogger()}
		if err := s2.Tick(t.Context(), scanSystem, anchorAt); err != nil || store.puts != 2 {
			t.Fatalf("second process: err %v puts %d", err, store.puts)
		}
	})
	t.Run("increment break fires P0", func(t *testing.T) {
		s, f, store, alerts := newAnchorFixture(t, 2)
		prev, body, err := BuildAnchor("20261101", []repository.ChainHead{f.head}, anchorAt.AddDate(0, 0, -1))
		if err != nil {
			t.Fatal(err)
		}
		store.objs[AnchorKey(prev.Date)] = body
		g := buildChain(t, 5)
		g.rows[3].Rec.Summary = json.RawMessage(`{"改":1}`) // 当日增量第 4 行被改
		f.rows, f.head = g.rows, g.head
		if err := s.Tick(t.Context(), scanSystem, anchorAt); err != nil {
			t.Fatal(err)
		}
		mark := string(store.objs[AnchorVerifyKey("20261102")])
		if !strings.Contains(mark, string(BreakRowModified)) || len(alerts.got) != 1 ||
			alerts.got[0].Name != model.AlertAuditChainBroken || alerts.got[0].Level != model.AlertP0 {
			t.Fatalf("mark %s alerts %+v", mark, alerts.got)
		}
	})
	t.Run("previous anchor recomputed fires P0", func(t *testing.T) {
		s, f, store, alerts := newAnchorFixture(t, 4)
		prev, body, err := BuildAnchor("20261101", []repository.ChainHead{f.head}, anchorAt.AddDate(0, 0, -1))
		if err != nil {
			t.Fatal(err)
		}
		store.objs[AnchorKey(prev.Date)] = body
		f.rows[3].RowHash = repository.GenesisHash // 锚定位置被重算
		if err := s.Tick(t.Context(), scanSystem, anchorAt); err != nil {
			t.Fatal(err)
		}
		if len(alerts.got) == 0 || !strings.Contains(alerts.got[0].Detail, string(AnchorRecomputed)) {
			t.Fatalf("alerts %+v", alerts.got)
		}
	})
	t.Run("tampered today anchor alerts and marks done", func(t *testing.T) {
		s, _, store, alerts := newAnchorFixture(t, 2)
		store.objs[AnchorKey("20261102")] = []byte(`{"date":"20261102","chains":[],"sha256":"00"}`)
		if err := s.Tick(t.Context(), scanSystem, anchorAt); !errors.Is(err, ErrAnchorDigest) {
			t.Fatalf("err = %v, want ErrAnchorDigest", err)
		}
		if err := s.Tick(t.Context(), scanSystem, anchorAt); err != nil {
			t.Fatalf("second tick err = %v", err)
		}
		if names := alerts.names(); len(names) != 1 || names[0] != model.AlertAuditChainBroken {
			t.Fatalf("alerts %v", names)
		}
	})
	t.Run("put failure is returned and retried", func(t *testing.T) {
		s, _, store, _ := newAnchorFixture(t, 2)
		store.putErr = errors.New("minio down")
		if err := s.Tick(t.Context(), scanSystem, anchorAt); err == nil {
			t.Fatal("want error")
		}
		store.putErr = nil
		if err := s.Tick(t.Context(), scanSystem, anchorAt); err != nil || store.puts != 2 {
			t.Fatalf("retry err %v puts %d", err, store.puts)
		}
	})
	// 必败：锚点对象被重写出第二个版本（最早版本仍合法）→ P0，且不重写。
	t.Run("rewritten anchor version fires P0", func(t *testing.T) {
		s, f, store, alerts := newAnchorFixture(t, 2)
		_, body, err := BuildAnchor("20261102", []repository.ChainHead{f.head}, anchorAt)
		if err != nil {
			t.Fatal(err)
		}
		store.objs[AnchorKey("20261102")] = body
		store.versions = map[string]int{AnchorKey("20261102"): 1}
		if err := s.Tick(t.Context(), scanSystem, anchorAt); err != nil {
			t.Fatal(err)
		}
		if names := alerts.names(); len(names) != 1 || names[0] != model.AlertAuditChainBroken ||
			!strings.Contains(alerts.got[0].Detail, "2 versions") || store.puts != 1 {
			t.Fatalf("alerts %+v puts %d", alerts.got, store.puts)
		}
	})
	t.Run("existing verify mark rechecked", func(t *testing.T) {
		cases := []struct {
			name   string
			tamper func(f *fakeChain, store *memAnchors)
			alert  bool
		}{
			{name: "consistent mark is silent"},
			// 必败：标记被改写成更少行数。
			{name: "mark rewritten", alert: true, tamper: func(_ *fakeChain, store *memAnchors) {
				store.objs[AnchorVerifyKey("20261102")] = []byte(`{"date":"20261102","prev_date":"20261101","chains":1,"rows":0,"problems":[]}`)
			}},
			// 必败：标记写完之后链被改，重算出现新问题。
			{name: "chain tampered after mark", alert: true, tamper: func(f *fakeChain, _ *memAnchors) {
				f.rows[1].Rec.Target = "field:9"
			}},
			{name: "mark unreadable", alert: true, tamper: func(_ *fakeChain, store *memAnchors) {
				store.objs[AnchorVerifyKey("20261102")] = []byte(`not json`)
			}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				s, f, store, alerts := newAnchorFixture(t, 3)
				if err := s.Tick(t.Context(), scanSystem, anchorAt); err != nil {
					t.Fatal(err)
				}
				if c.tamper != nil {
					c.tamper(f, store)
				}
				s2 := &AnchorService{Chains: s.Chains, Store: store, Alerts: alerts, Logger: discardLogger()}
				if err := s2.Tick(t.Context(), scanSystem, anchorAt); err != nil || store.puts != 2 {
					t.Fatalf("err %v puts %d", err, store.puts)
				}
				names := alerts.names()
				if c.alert != (len(names) == 1) || c.alert && names[0] != model.AlertAuditChainBroken || len(names) > 1 {
					t.Fatalf("alerts %+v, want alert %v", alerts.got, c.alert)
				}
			})
		}
	})
	t.Run("anchor copied from another date fires P0", func(t *testing.T) {
		s, f, store, alerts := newAnchorFixture(t, 2)
		_, body, err := BuildAnchor("20261101", []repository.ChainHead{f.head}, anchorAt)
		if err != nil {
			t.Fatal(err)
		}
		store.objs[AnchorKey("20261102")] = body
		if err := s.Tick(t.Context(), scanSystem, anchorAt); !errors.Is(err, ErrAnchorDigest) {
			t.Fatalf("err = %v, want ErrAnchorDigest", err)
		}
		if names := alerts.names(); len(names) != 1 || names[0] != model.AlertAuditChainBroken {
			t.Fatalf("alerts %v", names)
		}
	})
}
