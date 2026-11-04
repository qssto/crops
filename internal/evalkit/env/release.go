package env

import (
	"context"
	"fmt"
)

// ReleaseClean 在 trial 结束时按前缀删除槽位的 Redis 键（as:eval-t{n}:*，P2 §6.8 技术实现 6）再归还槽位；
// 删除失败也归还，返回错误（MySQL 现场仍保留到下次 Acquire 重置）。
func (p *Pool) ReleaseClean(ctx context.Context, s *Slot) error {
	defer p.Release(s)
	if err := deletePrefix(ctx, p.cfg.Redis, s.Keys.Prefix()); err != nil {
		return fmt.Errorf("env: release %s: %w", s.Keys.Prefix(), err)
	}
	return nil
}

// Size 是池中槽位数（trial 并发度不得超过它）。
func (p *Pool) Size() int { return len(p.all) }
