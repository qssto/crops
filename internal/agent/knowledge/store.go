package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"sync"

	"agri-steward/internal/agent"
)

const (
	indexFile  = "index.yaml"
	readmeFile = "README.md"
)

// ErrNotFound 表示 kb_id@version 在该知识版本中不存在。
var ErrNotFound = errors.New("knowledge: entry not found")

// Snapshot 是某个知识版本（ref）的全部条目、索引与登记表，构造后只读、可并发读取。
// 它实现 agent.KnowledgeSource。
type Snapshot struct {
	ref          string
	indexVersion string
	entries      []*Entry // 按 kb_id 排序
	byID         map[string]*Entry
	deprecated   map[string]bool
	registry     *Registry
}

var _ agent.KnowledgeSource = (*Snapshot)(nil)

// Ref 返回快照的 ref：WORKTREE 或完整提交号（读取器支持解析时）。
func (s *Snapshot) Ref() string { return s.ref }

// IndexVersion 返回 index.yaml 的 version。
func (s *Snapshot) IndexVersion() string { return s.indexVersion }

// Registry 返回农药登记表（只读）。
func (s *Snapshot) Registry() *Registry { return s.registry }

// Entries 返回全部条目（含 deprecated），按 kb_id 排序；切片为副本，条目只读。
func (s *Snapshot) Entries() []*Entry { return append([]*Entry(nil), s.entries...) }

// Entry 按 kb_id 查条目（不论版本）。
func (s *Snapshot) Entry(kbID string) (*Entry, bool) {
	e, ok := s.byID[kbID]
	return e, ok
}

// Has 判断 kb_id@version 是否存在于本知识版本。
func (s *Snapshot) Has(kbID, version string) bool {
	e, ok := s.byID[kbID]
	return ok && e.Version == version
}

// Deprecated 判断条目是否在索引中标为废弃。
func (s *Snapshot) Deprecated(kbID string) bool { return s.deprecated[kbID] }

// Load 实现 agent.KnowledgeSource：Body 为条目（front-matter + 正文）的 JSON。
func (s *Snapshot) Load(_ context.Context, kbID, version string) (agent.KnowledgeEntry, error) {
	if !s.Has(kbID, version) {
		return agent.KnowledgeEntry{}, fmt.Errorf("%w: %s@%s at %s", ErrNotFound, kbID, version, s.ref)
	}
	body, err := json.Marshal(s.byID[kbID])
	if err != nil {
		return agent.KnowledgeEntry{}, fmt.Errorf("knowledge: marshal %s: %w", kbID, err)
	}
	return agent.KnowledgeEntry{KBID: kbID, Version: version, Body: body}, nil
}

// CheckTaxonomy 校验条目 category 为一级 code、topics 在枚举内且隶属于条目的 category。
func (s *Snapshot) CheckTaxonomy(tx *Taxonomy) error {
	if tx == nil {
		return errors.New("knowledge: taxonomy is nil")
	}
	var errs []error
	for _, e := range s.entries {
		for _, c := range e.Category {
			if top, ok := tx.CategoryOf(c); !ok || top != c {
				errs = append(errs, fmt.Errorf("%s: category %q is not a taxonomy top-level code", e.KBID, c))
			}
		}
		for _, t := range e.Topics {
			top, ok := tx.CategoryOf(t)
			if !ok || top == t || !slices.Contains(e.Category, top) {
				errs = append(errs, fmt.Errorf("%s: topic %q is not a taxonomy subcode of %v", e.KBID, t, e.Category))
			}
		}
	}
	return errors.Join(errs...)
}

// readSnapshot 读取并校验 ref 下的全部知识文件；任一校验失败即拒绝加载（不返回部分结果）。
func readSnapshot(ctx context.Context, r TreeReader, ref string) (*Snapshot, error) {
	files, err := r.List(ctx, ref)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{ref: ref, byID: map[string]*Entry{}, deprecated: map[string]bool{}}
	var errs []error
	var idxRaw, regRaw []byte
	for _, f := range files {
		switch {
		case f == indexFile:
			idxRaw, err = r.ReadFile(ctx, ref, f)
		case f == RegistryFile:
			regRaw, err = r.ReadFile(ctx, ref, f)
		case path.Ext(f) == ".md" && path.Base(f) != readmeFile:
			err = snap.addFile(ctx, r, f)
		default:
			continue
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if idxRaw == nil || regRaw == nil {
		errs = append(errs, fmt.Errorf("knowledge: %s and %s are required", indexFile, RegistryFile))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("knowledge: load %s: %w", ref, err)
	}
	idx, err := parseIndex(idxRaw)
	if err == nil {
		err = checkIndex(idx, snap.byID)
	}
	if err != nil {
		return nil, fmt.Errorf("knowledge: load %s: %w", ref, err)
	}
	if snap.registry, err = parseRegistry(regRaw); err != nil {
		return nil, fmt.Errorf("knowledge: load %s: %w", ref, err)
	}
	if err := snap.checkTargets(); err != nil {
		return nil, fmt.Errorf("knowledge: load %s: %w", ref, err)
	}
	snap.indexVersion = idx.Version
	for _, ie := range idx.Entries {
		snap.deprecated[ie.KBID] = ie.Deprecated
	}
	sort.Slice(snap.entries, func(i, j int) bool { return snap.entries[i].KBID < snap.entries[j].KBID })
	return snap, nil
}

func (s *Snapshot) addFile(ctx context.Context, r TreeReader, f string) error {
	raw, err := r.ReadFile(ctx, s.ref, f)
	if err != nil {
		return err
	}
	entries, err := ParseEntries(f, raw)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if prev, dup := s.byID[e.KBID]; dup {
			return fmt.Errorf("%s: duplicate kb_id %s (first in %s)", f, e.KBID, prev.File)
		}
		s.byID[e.KBID] = e
		s.entries = append(s.entries, e)
	}
	return nil
}

// checkTargets：条目的 target 必须是登记表 targets 中的 id。
func (s *Snapshot) checkTargets() error {
	var errs []error
	for _, e := range s.entries {
		if e.Target == "" {
			continue
		}
		if _, ok := s.registry.Targets[e.Target]; !ok {
			errs = append(errs, fmt.Errorf("%s: target %q not in %s targets", e.KBID, e.Target, RegistryFile))
		}
	}
	return errors.Join(errs...)
}

// Store 按 ref 打开知识快照并在进程内缓存（成功结果才缓存；同一 ref 并发打开只读取一次）。
type Store struct {
	reader TreeReader

	mu    sync.Mutex
	calls map[string]*openCall
}

type openCall struct {
	done chan struct{}
	snap *Snapshot
	err  error
}

// NewStore 用给定读取器构造 Store；生产用 NewStore(NewRepoReader(repoDir))。
func NewStore(r TreeReader) *Store {
	return &Store{reader: r, calls: map[string]*openCall{}}
}

// Open 打开 ref 对应的知识版本：空串 / WORKTREE 读工作区，kb@<sha> 或 git ref 经 git 读取。
func (s *Store) Open(ctx context.Context, ref string) (*Snapshot, error) {
	key, err := NormalizeRef(ref)
	if err != nil {
		return nil, err
	}
	if rr, ok := s.reader.(RefResolver); ok {
		if key, err = rr.Resolve(ctx, key); err != nil {
			return nil, fmt.Errorf("knowledge: resolve %s: %w", ref, err)
		}
	}
	s.mu.Lock()
	c, ok := s.calls[key]
	if !ok {
		c = &openCall{done: make(chan struct{})}
		s.calls[key] = c
	}
	s.mu.Unlock()
	if !ok {
		c.snap, c.err = readSnapshot(ctx, s.reader, key)
		if c.err != nil {
			s.mu.Lock()
			delete(s.calls, key)
			s.mu.Unlock()
		}
		close(c.done)
	}
	select {
	case <-c.done:
		return c.snap, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
