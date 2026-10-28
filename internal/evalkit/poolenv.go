package evalkit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/sample"
)

// DepsFunc 按槽位组装 app.Deps：Clock / DB / Keys 必须取自 slot，其余（Agent、Redis、RawStore 等）由调用方决定。
type DepsFunc func(slot *env.Slot) app.Deps

// PoolEnv 是基于 trial schema 池的 EnvFunc：每个 trial 取一个槽位，按样本 fixtures.clock 重置，
// 经 app.Build 装配；release 归还槽位。
func PoolEnv(pool *env.Pool, deps DepsFunc) EnvFunc {
	return func(ctx context.Context, s *sample.Sample, _ int) (*app.App, func() error, error) {
		at, err := FixtureTime(s)
		if err != nil {
			return nil, nil, err
		}
		slot, err := pool.Acquire(ctx, at)
		if err != nil {
			return nil, nil, err
		}
		d := deps(slot)
		if d.Clock != slot.Clock || d.DB != slot.DB || d.Keys != slot.Keys {
			pool.Release(slot)
			return nil, nil, errors.New("evalkit: DepsFunc must use slot Clock, DB and Keys")
		}
		a, err := app.Build(d)
		if err != nil {
			pool.Release(slot)
			return nil, nil, fmt.Errorf("build app: %w", err)
		}
		return a, func() error { pool.Release(slot); return nil }, nil
	}
}

// FixtureTime 解析样本 fixtures.clock（RFC3339）为 UTC 时刻。
func FixtureTime(s *sample.Sample) (time.Time, error) {
	at, err := time.Parse(time.RFC3339, s.Fixtures.Clock)
	if err != nil {
		return time.Time{}, fmt.Errorf("sample %s: fixtures.clock: %w", s.ID, err)
	}
	return at.UTC(), nil
}
