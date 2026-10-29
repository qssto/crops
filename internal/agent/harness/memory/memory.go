// Package memory 是三层记忆的读取与注入裁剪（P0 §6.7 功能任务 6）：
// profile（农场 + 地块 + 常种作物 + 常用药剂偏好）、episodic（近 N 次建议 / 驳回 / 作业结果）、working（当前会话，经 ctx 传入）。
// 只依赖 agent 接缝；读库由 repository.MemoryRepo 实现 Source。
package memory

import (
	"context"
	"fmt"
	"unicode/utf8"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// 记忆种类（MemoryItem.Kind）；episodic 的种类沿用 episodic_memory.kind（ADVICE / REJECTED / WORK_DONE）。
const (
	KindProfile = "PROFILE"
	KindWorking = "WORKING"
)

const (
	// DefaultEpisodicLimit 是默认注入的 episodic 条数。
	DefaultEpisodicLimit = 5
	// DefaultRuneBudget 是注入记忆摘要的总字符上限（超出按相关性从低到高截断）。
	DefaultRuneBudget = 600
)

// Profile 是 farm_profile + field_profile 的注入视图（已渲染为摘要文本）。
type Profile struct {
	Summary string
	Found   bool
}

// Source 是记忆的持久层读取，repository.MemoryRepo 实现。
type Source interface {
	// Profile 读农场与地块画像；地块不在 Scope 内返回 Found=false。
	Profile(ctx context.Context, scope model.Scope, fieldID uint64) (Profile, error)
	// Episodic 读该地块近 limit 条 episodic_memory，按 occurred_at 倒序。
	Episodic(ctx context.Context, scope model.Scope, fieldID uint64, limit int) ([]agent.MemoryItem, error)
}

// Options 是记忆开关与裁剪参数。
type Options struct {
	Enabled       bool
	EpisodicLimit int // 0 取 DefaultEpisodicLimit
	RuneBudget    int // 0 取 DefaultRuneBudget
}

// Store 实现 agent.MemoryStore。关闭时 Recent 恒返回空，不读库。
type Store struct {
	src  Source
	opts Options
}

var _ agent.MemoryStore = (*Store)(nil)

// New 构造 Store。
func New(src Source, opts Options) *Store {
	if opts.EpisodicLimit <= 0 {
		opts.EpisodicLimit = DefaultEpisodicLimit
	}
	if opts.RuneBudget <= 0 {
		opts.RuneBudget = DefaultRuneBudget
	}
	return &Store{src: src, opts: opts}
}

// Enabled 报告记忆注入是否开启。
func (s *Store) Enabled() bool { return s != nil && s.opts.Enabled && s.src != nil }

// Recent 按相关性组装注入记忆：working（当前会话）→ profile → episodic（越近越相关）；limit 限制 episodic 条数
// （≤ 0 取配置），总字符超 RuneBudget 时从最不相关的一端截断。
func (s *Store) Recent(ctx context.Context, scope model.Scope, fieldID uint64, limit int) ([]agent.MemoryItem, error) {
	if !s.Enabled() {
		return nil, nil
	}
	if limit <= 0 || limit > s.opts.EpisodicLimit {
		limit = s.opts.EpisodicLimit
	}
	items := append([]agent.MemoryItem(nil), WorkingFrom(ctx)...)
	p, err := s.src.Profile(ctx, scope, fieldID)
	if err != nil {
		return nil, fmt.Errorf("memory: profile field %d: %w", fieldID, err)
	}
	if p.Found && p.Summary != "" {
		items = append(items, agent.MemoryItem{Kind: KindProfile, RefID: fieldID, Summary: p.Summary})
	}
	eps, err := s.src.Episodic(ctx, scope, fieldID, limit)
	if err != nil {
		return nil, fmt.Errorf("memory: episodic field %d: %w", fieldID, err)
	}
	items = append(items, eps...)
	return truncate(items, s.opts.RuneBudget), nil
}

// truncate 保留按相关性排在前面、累计字符不超过 budget 的条目。
func truncate(items []agent.MemoryItem, budget int) []agent.MemoryItem {
	used := 0
	for i, it := range items {
		used += utf8.RuneCountInString(it.Summary)
		if used > budget {
			return items[:i]
		}
	}
	return items
}

type workingKey struct{}

// WithWorking 把当前会话的工作记忆放进 ctx（P0 单轮问答为空；多轮会话由上层注入）。
func WithWorking(ctx context.Context, items ...agent.MemoryItem) context.Context {
	own := make([]agent.MemoryItem, len(items))
	for i, it := range items {
		it.Kind = KindWorking
		own[i] = it
	}
	return context.WithValue(ctx, workingKey{}, own)
}

// WorkingFrom 取 ctx 中的工作记忆。
func WorkingFrom(ctx context.Context) []agent.MemoryItem {
	items, _ := ctx.Value(workingKey{}).([]agent.MemoryItem)
	return items
}
