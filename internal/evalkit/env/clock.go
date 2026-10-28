// Package env 提供评测隔离环境：FrozenClock 与 trial schema 池（agri_eval_t_{n} + as:eval-t{n}:）。
package env

import (
	"sync"
	"time"
)

// FrozenClock 是可控时钟：Now 恒返回设定时刻，只随 Advance / Set 前进。并发安全。
type FrozenClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFrozenClock 以 at（转 UTC）为初始时刻。
func NewFrozenClock(at time.Time) *FrozenClock {
	return &FrozenClock{now: at.UTC()}
}

// Now 返回当前冻结时刻。
func (c *FrozenClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 把时钟向前拨 d（d 可为负，用于构造回拨场景）。
func (c *FrozenClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set 把时钟设为 at（转 UTC）。
func (c *FrozenClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at.UTC()
}
