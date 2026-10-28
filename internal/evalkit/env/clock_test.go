package env

import (
	"sync"
	"testing"
	"time"
)

func TestFrozenClock(t *testing.T) {
	start := time.Date(2026, 7, 15, 9, 0, 0, 0, time.FixedZone("CST", 8*3600))
	tests := []struct {
		name string
		op   func(c *FrozenClock)
		want time.Time
	}{
		{name: "frozen", op: func(*FrozenClock) {}, want: start.UTC()},
		{name: "advance", op: func(c *FrozenClock) { c.Advance(90 * time.Minute) }, want: start.UTC().Add(90 * time.Minute)},
		{name: "rewind", op: func(c *FrozenClock) { c.Advance(-time.Hour) }, want: start.UTC().Add(-time.Hour)},
		{name: "set", op: func(c *FrozenClock) { c.Set(time.Unix(0, 0)) }, want: time.Unix(0, 0).UTC()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewFrozenClock(start)
			tt.op(c)
			if got := c.Now(); !got.Equal(tt.want) || got.Location() != time.UTC {
				t.Fatalf("Now = %v, want %v UTC", got, tt.want)
			}
			if a, b := c.Now(), c.Now(); !a.Equal(b) {
				t.Fatalf("clock moved without Advance: %v -> %v", a, b)
			}
		})
	}
}

func TestFrozenClock_Concurrent(t *testing.T) {
	c := NewFrozenClock(time.Unix(0, 0))
	const workers = 50
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Advance(time.Second)
			_ = c.Now()
		}()
	}
	wg.Wait()
	if got := c.Now(); !got.Equal(time.Unix(workers, 0)) {
		t.Fatalf("Now = %v, want +%ds", got, workers)
	}
}

func TestNewPool_RequiresDeps(t *testing.T) {
	if _, err := NewPool(t.Context(), PoolConfig{}); err == nil {
		t.Fatal("want error for empty PoolConfig")
	}
}
