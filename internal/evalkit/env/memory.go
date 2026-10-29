package env

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/memory"
	"agri-steward/internal/model"
)

// fixtures.memory 的键。
const (
	memoryProfileKey  = "profile"
	memoryEpisodicKey = "episodic"
)

var errBadMemoryFixture = errors.New("env: bad fixtures.memory")

// FixtureMemory 是样本 fixtures.memory 注入的记忆源，实现 memory.Source（经 memory.Store 走与生产相同的
// 条数与字符裁剪）：
//
//	memory:
//	  profile: {preferred_products: [吡蚜酮]}          # 渲染为 "k: v" 摘要，按键排序
//	  episodic:                                      # 按 occurred_at 倒序返回
//	    - {kind: REJECTED, summary: 作业单驳回：雨前不宜喷药, occurred_at: 2026-07-20T09:00:00+08:00}
//
// 样本不区分地块：任一 fieldID 都返回同一份记忆。
type FixtureMemory struct {
	profile  memory.Profile
	episodic []agent.MemoryItem
}

var _ memory.Source = (*FixtureMemory)(nil)

// NewFixtureMemory 解析 fixtures.memory；为空时返回无记忆的源。
func NewFixtureMemory(raw map[string]any) (*FixtureMemory, error) {
	m := &FixtureMemory{}
	for k := range raw {
		if k != memoryProfileKey && k != memoryEpisodicKey {
			return nil, fmt.Errorf("%w: unknown key %q", errBadMemoryFixture, k)
		}
	}
	if p, ok := raw[memoryProfileKey]; ok {
		summary, err := renderProfile(p)
		if err != nil {
			return nil, err
		}
		m.profile = memory.Profile{Summary: summary, Found: summary != ""}
	}
	if e, ok := raw[memoryEpisodicKey]; ok {
		items, err := parseEpisodic(e)
		if err != nil {
			return nil, err
		}
		m.episodic = items
	}
	return m, nil
}

// Profile 返回 fixture 画像。
func (m *FixtureMemory) Profile(_ context.Context, _ model.Scope, _ uint64) (memory.Profile, error) {
	return m.profile, nil
}

// Episodic 返回前 limit 条 episodic（已按 occurred_at 倒序）。
func (m *FixtureMemory) Episodic(_ context.Context, _ model.Scope, _ uint64, limit int) ([]agent.MemoryItem, error) {
	n := min(max(limit, 0), len(m.episodic))
	return append([]agent.MemoryItem(nil), m.episodic[:n]...), nil
}

func renderProfile(v any) (string, error) {
	p, ok := v.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%w: profile must be a mapping, got %T", errBadMemoryFixture, v)
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %v", k, p[k]))
	}
	return strings.Join(parts, "；"), nil
}

func parseEpisodic(v any) ([]agent.MemoryItem, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: episodic must be a list, got %T", errBadMemoryFixture, v)
	}
	items := make([]agent.MemoryItem, 0, len(list))
	for i, e := range list {
		row, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: episodic[%d] must be a mapping", errBadMemoryFixture, i)
		}
		kind, _ := row["kind"].(string)
		summary, _ := row["summary"].(string)
		if kind == "" || summary == "" {
			return nil, fmt.Errorf("%w: episodic[%d] needs kind and summary", errBadMemoryFixture, i)
		}
		at, err := occurredAt(row["occurred_at"])
		if err != nil {
			return nil, fmt.Errorf("%w: episodic[%d]: %w", errBadMemoryFixture, i, err)
		}
		items = append(items, agent.MemoryItem{Kind: kind, Summary: summary, OccurredAt: at})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].OccurredAt.After(items[j].OccurredAt) })
	return items, nil
}

// occurredAt 接受 RFC3339 字符串或 YAML 解出的 time.Time。
func occurredAt(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), nil
	case string:
		at, err := time.Parse(time.RFC3339, t)
		if err != nil {
			return time.Time{}, fmt.Errorf("occurred_at: %w", err)
		}
		return at.UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("occurred_at must be RFC3339, got %T", v)
	}
}
