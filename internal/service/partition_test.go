package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// 0001 已建的分区：p202610…p202709 + pmax。
var seedPartitions = []string{"p202610", "p202611", "p202612", "p202701", "p202702", "p202703", "p202704",
	"p202705", "p202706", "p202707", "p202708", "p202709", "pmax"}

type memPartitions struct {
	parts  map[string][]string
	pmax   map[string]int64
	splits map[string][]string
	err    error
}

func newMemPartitions() *memPartitions {
	return &memPartitions{parts: map[string][]string{
		repository.TableAuditLog: slices.Clone(seedPartitions), repository.TableRunEvent: slices.Clone(seedPartitions)},
		pmax: map[string]int64{}, splits: map[string][]string{}}
}

func (m *memPartitions) Partitions(_ context.Context, table string) ([]string, error) {
	return m.parts[table], nil
}

func (m *memPartitions) SplitMax(_ context.Context, table string, months []time.Time) error {
	if m.err != nil {
		return m.err
	}
	ps := m.parts[table][:len(m.parts[table])-1]
	for _, mo := range months {
		ps = append(ps, repository.PartitionName(mo))
		m.splits[table] = append(m.splits[table], repository.PartitionName(mo))
	}
	m.parts[table] = append(ps, "pmax")
	return nil
}

func (m *memPartitions) CountMax(_ context.Context, table string) (int64, error) {
	return m.pmax[table], nil
}

func cst(y int, mo time.Month, d int) time.Time {
	return time.Date(y, mo, d, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
}

func TestMissingMonths(t *testing.T) {
	tests := []struct {
		name  string
		parts []string
		now   time.Time
		want  []string
	}{
		{name: "covered", parts: seedPartitions, now: cst(2026, 10, 25), want: nil},
		{name: "two ahead", parts: seedPartitions, now: cst(2027, 8, 25), want: []string{"p202710"}},
		{name: "gap filled monthly", parts: seedPartitions, now: cst(2027, 11, 25),
			want: []string{"p202710", "p202711", "p202712", "p202801"}},
		{name: "no month partitions", parts: []string{"pmax"}, now: cst(2027, 12, 25),
			want: []string{"p202712", "p202801", "p202802"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, m := range MissingMonths(tt.parts, tt.now, partitionAhead) {
				got = append(got, repository.PartitionName(m))
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPartitionMaintainer_Tick(t *testing.T) {
	tests := []struct {
		name      string
		now       time.Time
		lockHeld  bool
		splitErr  error
		wantSplit []string
		wantErr   bool
		wantDone  bool
	}{
		{name: "before 25th skips", now: cst(2027, 8, 24)},
		{name: "25th adds months", now: cst(2027, 8, 25), wantSplit: []string{"p202710"}, wantDone: true},
		{name: "already covered is idempotent", now: cst(2026, 10, 25), wantDone: true},
		{name: "lock held skips", now: cst(2027, 8, 25), lockHeld: true},
		{name: "split failure reported", now: cst(2027, 8, 25), splitErr: errors.New("ddl denied"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemPartitions()
			store.err = tt.splitErr
			store.pmax[repository.TableRunEvent] = 3 // 只告警，不阻断
			keys, err := rediskey.New("test")
			if err != nil {
				t.Fatal(err)
			}
			m, err := NewPartitionMaintainer(PartitionDeps{Store: store, Locks: memLocks{held: tt.lockHeld},
				Keys: keys, Clock: fixedClock{tt.now}})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Tick(context.Background()); (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			for _, table := range []string{repository.TableAuditLog, repository.TableRunEvent} {
				if !slices.Equal(store.splits[table], tt.wantSplit) {
					t.Fatalf("%s splits = %v, want %v", table, store.splits[table], tt.wantSplit)
				}
			}
			if (m.done != "") != tt.wantDone {
				t.Fatalf("done = %q, want done=%v", m.done, tt.wantDone)
			}
			if !tt.wantDone {
				return
			}
			// 同月再次 Tick 不再执行
			store.err = errors.New("must not run again")
			if err := m.Tick(context.Background()); err != nil {
				t.Fatalf("second tick: %v", err)
			}
		})
	}
}
