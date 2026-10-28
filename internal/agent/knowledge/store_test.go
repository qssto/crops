package knowledge

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// repoRoot 返回仓库根目录（本文件位于 internal/agent/knowledge）。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func repoTaxonomy(t *testing.T) *Taxonomy {
	t.Helper()
	tx, err := LoadTaxonomy(filepath.Join(repoRoot(t), "eval", "taxonomy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func repoSnapshot(t *testing.T) *Snapshot {
	t.Helper()
	snap, err := NewStore(NewRepoReader(repoRoot(t))).Open(context.Background(), RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// 仓库内知识库必须能完整加载，且与 taxonomy 一致。
func TestStore_OpenRepoWorktree(t *testing.T) {
	snap := repoSnapshot(t)
	if got := len(snap.Entries()); got != 38 {
		t.Errorf("entries = %d, want 38", got)
	}
	if got := len(snap.Registry().Products); got != 21 {
		t.Errorf("products = %d, want 21", got)
	}
	if err := snap.CheckTaxonomy(repoTaxonomy(t)); err != nil {
		t.Error(err)
	}
	if !snap.Has("rice/pest/planthopper", "v1") || snap.Has("rice/pest/planthopper", "v2") {
		t.Error("Has(planthopper) wrong")
	}
}

// git 读取路径：HEAD 上的知识库可加载，缓存键为完整提交号。
func TestStore_OpenGitRef(t *testing.T) {
	snap, err := NewStore(NewRepoReader(repoRoot(t))).Open(context.Background(), "HEAD")
	if err != nil {
		t.Skipf("git unavailable or HEAD has no knowledge/: %v", err)
	}
	if len(snap.Ref()) != 40 {
		t.Errorf("ref = %q, want resolved 40-char sha", snap.Ref())
	}
	if len(snap.Entries()) == 0 {
		t.Error("no entries at HEAD")
	}
}

const (
	fmPlanthopper = `---
kb_id: rice/pest/planthopper
title: 稻飞虱
crop: rice
stage: [分蘖期]
region: jiangsu
category: [protect]
topics: [protect.timing]
target: planthopper
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
basis:
  - {source: 测试, level: 专家经验, verified: false}
---
## 适用生育期
分蘖期
`
	idxPlanthopper = `version: v1
entries:
  - kb_id: rice/pest/planthopper
    file: rice/pest/planthopper.md
    version: v1
    crop: rice
    stage: [分蘖期]
    region: jiangsu
    category: [protect]
    topics: [protect.timing]
    target: planthopper
    authority_level: 专家经验
`
	regMinimal = `version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
targets: {planthopper: 稻飞虱}
products:
  - id: pymetrozine-50wg
    common_name: 吡蚜酮
    formulation: 水分散粒剂
    content: 50%
    registration_no: null
    registrations:
      - {crop: rice, target: planthopper, dose: {min: 8, max: 12, unit: g/亩}, method: 喷雾,
         dilution_note: x, phi_days: 14, max_times_per_season: 2, regions: ["*"], confidence: medium}
`
)

func fsWith(files map[string]string) FSReader {
	m := fstest.MapFS{}
	for k, v := range files {
		m[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return FSReader{FS: m}
}

func validFiles() map[string]string {
	return map[string]string{
		"index.yaml":               idxPlanthopper,
		"rice/pest/planthopper.md": fmPlanthopper,
		RegistryFile:               regMinimal,
		"README.md":                "# 不是条目，不解析\n",
	}
}

// 校验失败一律拒绝加载（P0 §6.4-2）。
func TestStore_OpenRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr string // 空表示应成功
	}{
		{name: "valid", mutate: func(map[string]string) {}},
		{name: "missing authority_level", wantErr: "authority_level",
			mutate: func(m map[string]string) {
				m["rice/pest/planthopper.md"] = strings.Replace(fmPlanthopper, "authority_level: 专家经验\n", "", 1)
			}},
		{name: "missing version", wantErr: "version is required",
			mutate: func(m map[string]string) {
				m["rice/pest/planthopper.md"] = strings.Replace(fmPlanthopper, "version: v1\n", "", 1)
			}},
		{name: "pest without target", wantErr: "target is required",
			mutate: func(m map[string]string) {
				m["rice/pest/planthopper.md"] = strings.Replace(fmPlanthopper, "target: planthopper\n", "", 1)
			}},
		{name: "unknown front-matter field", wantErr: "front-matter",
			mutate: func(m map[string]string) {
				m["rice/pest/planthopper.md"] = strings.Replace(fmPlanthopper, "title:", "titel: x\ntitle:", 1)
			}},
		{name: "topic outside category", wantErr: "does not belong",
			mutate: func(m map[string]string) {
				m["rice/pest/planthopper.md"] = strings.Replace(fmPlanthopper, "[protect.timing]", "[irrigate.depth]", 1)
			}},
		{name: "stage not in enum", wantErr: "stage",
			mutate: func(m map[string]string) {
				m["rice/pest/planthopper.md"] = strings.Replace(fmPlanthopper, "[分蘖期]", "[分蘖末期]", 1)
			}},
		{name: "claude-draft signed", wantErr: "claude-draft",
			mutate: func(m map[string]string) {
				m["rice/pest/planthopper.md"] = strings.Replace(fmPlanthopper, "pending_expert", "signed", 1)
			}},
		{name: "index version differs", wantErr: "version differs",
			mutate: func(m map[string]string) {
				m["index.yaml"] = strings.Replace(idxPlanthopper, "version: v1\n    crop", "version: v2\n    crop", 1)
			}},
		{name: "index stage differs", wantErr: "stage differs",
			mutate: func(m map[string]string) {
				m["index.yaml"] = strings.Replace(idxPlanthopper, "[分蘖期]", "[秧苗期]", 1)
			}},
		{name: "index lists missing entry", wantErr: "has no entry",
			mutate: func(m map[string]string) {
				m["index.yaml"] = strings.Replace(idxPlanthopper, "rice/pest/planthopper\n", "rice/pest/ghost\n", 1)
			}},
		{name: "entry missing from index", wantErr: "missing from index",
			mutate: func(m map[string]string) {
				m["rice/pest/blast.md"] = strings.NewReplacer("planthopper", "blast").Replace(fmPlanthopper)
				m[RegistryFile] = strings.Replace(regMinimal, "{planthopper: 稻飞虱}", "{planthopper: 稻飞虱, blast: 稻瘟病}", 1)
			}},
		{name: "entry in wrong file", wantErr: "belongs in",
			mutate: func(m map[string]string) {
				delete(m, "rice/pest/planthopper.md")
				m["rice/pest/other.md"] = fmPlanthopper
			}},
		{name: "body contains separator", wantErr: "unbalanced",
			mutate: func(m map[string]string) { m["rice/pest/planthopper.md"] = fmPlanthopper + "---\n尾巴\n" }},
		{name: "target not in registry", wantErr: "not in common/pesticide_registry.yaml targets",
			mutate: func(m map[string]string) {
				m["rice/pest/planthopper.md"] = strings.Replace(fmPlanthopper, "target: planthopper", "target: ghost", 1)
				m["index.yaml"] = strings.Replace(idxPlanthopper, "target: planthopper", "target: ghost", 1)
			}},
		{name: "registry dose min > max", wantErr: "min <= max",
			mutate: func(m map[string]string) { m[RegistryFile] = strings.Replace(regMinimal, "min: 8", "min: 80", 1) }},
		{name: "missing index", wantErr: "index.yaml",
			mutate: func(m map[string]string) { delete(m, "index.yaml") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := validFiles()
			tt.mutate(files)
			snap, err := NewStore(fsWith(files)).Open(context.Background(), "")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
			if snap != nil {
				t.Fatal("snapshot must be nil on error")
			}
		})
	}
}

func TestNormalizeRef(t *testing.T) {
	tests := []struct {
		in, want string
		bad      bool
	}{
		{in: "", want: RefWorktree},
		{in: "WORKTREE", want: RefWorktree},
		{in: "kb@4ce4e8b", want: "4ce4e8b"},
		{in: "HEAD", want: "HEAD"},
		{in: "kb@TBD", bad: true},
		{in: "--output=/tmp/x", bad: true},
		{in: "a..b", bad: true},
	}
	for _, tt := range tests {
		got, err := NormalizeRef(tt.in)
		if tt.bad {
			if !errors.Is(err, ErrBadRef) {
				t.Errorf("NormalizeRef(%q) err = %v, want ErrBadRef", tt.in, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("NormalizeRef(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

// countingReader 统计 List 调用次数，验证缓存与并发只读一次。
type countingReader struct {
	TreeReader
	mu    sync.Mutex
	lists int
}

func (c *countingReader) List(ctx context.Context, ref string) ([]string, error) {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.TreeReader.List(ctx, ref)
}

func TestStore_CacheConcurrent(t *testing.T) {
	cr := &countingReader{TreeReader: fsWith(validFiles())}
	st := NewStore(cr)
	var wg sync.WaitGroup
	snaps := make([]*Snapshot, 16)
	for i := range snaps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := st.Open(context.Background(), "")
			if err != nil {
				t.Error(err)
			}
			snaps[i] = s
		}(i)
	}
	wg.Wait()
	if cr.lists != 1 {
		t.Errorf("List called %d times, want 1", cr.lists)
	}
	for _, s := range snaps[1:] {
		if s != snaps[0] {
			t.Fatal("concurrent Open must return the same snapshot")
		}
	}
}

// 失败结果不缓存：修好文件后重新 Open 可成功。
func TestStore_ErrorNotCached(t *testing.T) {
	m := fstest.MapFS{"index.yaml": {Data: []byte("bad: [")}}
	st := NewStore(FSReader{FS: m})
	if _, err := st.Open(context.Background(), ""); err == nil {
		t.Fatal("want error")
	}
	for k, v := range validFiles() {
		m[k] = &fstest.MapFile{Data: []byte(v)}
	}
	if _, err := st.Open(context.Background(), ""); err != nil {
		t.Fatalf("after fix: %v", err)
	}
}

func TestSnapshot_LoadImplementsKnowledgeSource(t *testing.T) {
	snap := repoSnapshot(t)
	got, err := snap.Load(context.Background(), "rice/pest/planthopper", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.KBID != "rice/pest/planthopper" || got.Version != "v1" || !strings.Contains(string(got.Body), `"target":"planthopper"`) {
		t.Errorf("Load = %+v", got)
	}
	if _, err := snap.Load(context.Background(), "rice/pest/planthopper", "v9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing version err = %v, want ErrNotFound", err)
	}
}
