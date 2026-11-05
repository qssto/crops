package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/service"
)

// 仓库 configs/loader 与知识库、taxonomy（与生产同一份）。
func repoLoaders(t *testing.T) (*KnowledgeLoaders, *knowledge.Snapshot, *knowledge.Taxonomy) {
	t.Helper()
	tx, err := knowledge.LoadTaxonomy(repoRoot + "/eval/taxonomy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	snap := repoKnowledge(t)
	l, err := LoadKnowledgeLoaders(filepath.Join(repoRoot, DefaultLoaderDir), tx, snap)
	if err != nil {
		t.Fatal(err)
	}
	return l, snap, tx
}

func repoKnowledge(t *testing.T) *knowledge.Snapshot {
	t.Helper()
	snap, err := knowledge.NewStore(knowledge.NewRepoReader(repoRoot)).Open(context.Background(), knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// loaderSituations 枚举知识库可达情境：作物 × 生育期（含未知）× 区域 × 一级类 × target（含空）。
func loaderSituations(snap *knowledge.Snapshot, tx *knowledge.Taxonomy, category string) []knowledge.Situation {
	crops, targets := []string{"rice"}, []string{""}
	for _, e := range snap.Entries() {
		if e.Crop != knowledge.Wildcard && !slices.Contains(crops, e.Crop) {
			crops = append(crops, e.Crop)
		}
		if e.Target != "" && !slices.Contains(targets, e.Target) {
			targets = append(targets, e.Target)
		}
	}
	var out []knowledge.Situation
	for _, crop := range crops {
		for _, stage := range append([]string{""}, knowledge.Stages...) {
			for _, region := range []string{"jiangsu", "anhui"} {
				for _, c := range tx.Categories {
					if category != "" && c.Code != category {
						continue
					}
					for _, target := range targets {
						out = append(out, knowledge.Situation{Crop: crop, Stage: stage, Region: region, Category: c.Code, Target: target})
					}
				}
			}
		}
	}
	return out
}

// 仓库 v0.yaml 与内置 PhaseZeroSpec（= P1 Select，上限 8）逐情境同序：开关关闭时行为不变的前提。
func TestLoadKnowledgeLoaders_V0EqualsPhaseZero(t *testing.T) {
	l, snap, tx := repoLoaders(t)
	builtin, err := knowledge.NewStrategy(knowledge.PhaseZeroSpec(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if l.V0.Version() != builtin.Version() || l.V0.Limit() != builtin.Limit() {
		t.Fatalf("v0 = %s/%d, want %s/%d", l.V0.Version(), l.V0.Limit(), builtin.Version(), builtin.Limit())
	}
	for _, sit := range loaderSituations(snap, tx, "") {
		got, gerr := l.V0.Select(snap, tx, sit)
		want, werr := builtin.Select(snap, tx, sit)
		if (gerr == nil) != (werr == nil) || !slices.Equal(pickRefs(got), pickRefs(want)) {
			t.Fatalf("%s: v0.yaml %v (%v) vs PhaseZero %v (%v)", sit, pickRefs(got), gerr, pickRefs(want), werr)
		}
	}
}

// protectSafetyIDs 是植保情境下必加的安全条目（common/safety.md 与 common/operation_window.md 中属植保类的全部条目，
// 路线 §6.6 红线）：PHI、桶混上限、抗性轮换、防护、禁限用 / 登记用量、喷雾 / 无人机作业窗口、稀释配比。
// 增删须同时改 configs/loader/v1.yaml 并说明理由。
var protectSafetyIDs = []string{
	"common/safety/phi", "common/safety/mixing", "common/safety/resistance", "common/safety/ppe", "common/safety/registration",
	"common/operation_window/spray", "common/operation_window/uav", "common/operation_window/dosage-dilution",
}

// 红线（P2 §6.6）：v1 下植保问题情境命中里的每条安全条目（逐 kb_id）都必须加载且标 MUST_INCLUDE；
// 施肥问题命中里带公式段的 {crop}/fertilization.md 条目必须全部 MUST_INCLUDE。
func TestLoadKnowledgeLoaders_V1RedLines(t *testing.T) {
	l, snap, tx := repoLoaders(t)
	if l.V1.Version() != "v1" {
		t.Fatalf("v1 version = %s", l.V1.Version())
	}
	for _, id := range protectSafetyIDs {
		if !slices.ContainsFunc(snap.Entries(), func(e *knowledge.Entry) bool { return e.KBID == id }) {
			t.Fatalf("safety entry %s not in knowledge snapshot", id)
		}
	}
	checked := map[string]int{}
	for _, sit := range loaderSituations(snap, tx, "protect") {
		all, err := knowledge.Select(snap, tx, sit, len(snap.Entries()))
		if err != nil {
			continue
		}
		picks, err := l.V1.Select(snap, tx, sit)
		if err != nil {
			t.Fatalf("%s: v1 %v, v0 hit", sit, err)
		}
		for _, id := range protectSafetyIDs {
			if !slices.ContainsFunc(all, func(e *knowledge.Entry) bool { return e.KBID == id }) {
				continue
			}
			checked[id]++
			if !slices.ContainsFunc(picks, func(p knowledge.Pick) bool {
				return p.Entry.KBID == id && p.Reason == knowledge.PickMustInclude
			}) {
				t.Errorf("%s: v1 picks %v miss MUST_INCLUDE %s", sit, pickRefs(picks), id)
			}
		}
		if !slices.ContainsFunc(picks, func(p knowledge.Pick) bool { return p.Reason == knowledge.PickRanked }) {
			t.Errorf("%s: v1 picks %v have no crop / target specific entry", sit, pickRefs(picks))
		}
	}
	for _, sit := range loaderSituations(snap, tx, "fertilize") {
		all, err := knowledge.Select(snap, tx, sit, len(snap.Entries()))
		if err != nil {
			continue
		}
		picks, err := l.V1.Select(snap, tx, sit)
		if err != nil {
			t.Fatalf("%s: v1 %v, v0 hit", sit, err)
		}
		for _, e := range all {
			if e.File != sit.Crop+"/fertilization.md" || len(e.Formulas) == 0 {
				continue
			}
			checked["fertilization formulas"]++
			if !slices.ContainsFunc(picks, func(p knowledge.Pick) bool {
				return p.Entry.KBID == e.KBID && p.Reason == knowledge.PickMustInclude
			}) {
				t.Errorf("%s: v1 picks %v miss formula entry %s", sit, pickRefs(picks), e.KBID)
			}
		}
	}
	for _, k := range append(slices.Clone(protectSafetyIDs), "fertilization formulas") {
		if checked[k] == 0 {
			t.Fatalf("red line %q never exercised: %v", k, checked)
		}
	}
}

// 评审 P0：遍历全部植保情境，v0 加载到的安全条目（common/safety.md、common/operation_window.md）v1 一条不丢。
func TestLoadKnowledgeLoaders_V1KeepsV0SafetyEntries(t *testing.T) {
	l, snap, tx := repoLoaders(t)
	sits := 0
	for _, sit := range loaderSituations(snap, tx, "protect") {
		v0, err := l.V0.Select(snap, tx, sit)
		if err != nil {
			continue
		}
		v1, err := l.V1.Select(snap, tx, sit)
		if err != nil {
			t.Fatalf("%s: v1 %v, v0 hit", sit, err)
		}
		sits++
		for _, p := range v0 {
			if f := p.Entry.File; f != "common/safety.md" && f != "common/operation_window.md" {
				continue
			}
			if !slices.ContainsFunc(v1, func(q knowledge.Pick) bool { return q.Entry.KBID == p.Entry.KBID }) {
				t.Errorf("%s: v0 loads %s, v1 drops it: %v", sit, p.Entry.KBID, pickRefs(v1))
			}
		}
	}
	if sits == 0 {
		t.Fatal("no protect situation loads knowledge")
	}
}

// 配置缺失或非法、必加规则在快照中匹配为空或超上限：启动失败（不回落内置策略）。
func TestLoadKnowledgeLoaders_Rejects(t *testing.T) {
	tx, err := knowledge.LoadTaxonomy(repoRoot + "/eval/taxonomy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	snap := repoKnowledge(t)
	src, err := os.ReadFile(filepath.Join(repoRoot, DefaultLoaderDir, "v0.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const head = "version: v1\nlimit: 10\norder: [authority]\n"
	const fert = "  fertilize:\n    - file: \"{crop}/fertilization.md\"\n      formulas_only: true\n"
	const protect = "  protect:\n    - file: common/safety.md\n    - file: common/operation_window.md\n"
	for name, v1 := range map[string]string{
		"missing":             "",
		"empty file":          "\n",
		"limit over max":      "version: v1\nlimit: 13\norder: [authority]\nmust_include:\n" + protect + fert,
		"unknown sort":        "version: v1\nlimit: 10\norder: [popularity]\nmust_include:\n" + protect + fert,
		"unknown field":       head + "boost: 2\nmust_include:\n" + protect + fert,
		"bad category":        head + "must_include:\n  weather:\n    - kb_id: common/safety/phi\n" + protect + fert,
		"no must_include":     head,
		"empty must_include":  head + "must_include: {}\n",
		"empty protect rules": head + "must_include:\n  protect: []\n" + fert,
		"no fertilize rules":  head + "must_include:\n" + protect,
		"typo kb_id":          head + "must_include:\n" + protect + "    - kb_id: common/safety/phl\n" + fert,
		"typo file":           head + "must_include:\n  protect:\n    - file: commn/safety.md\n" + fert,
		"must over limit":     "version: v1\nlimit: 3\norder: [authority]\nmust_include:\n" + protect + fert,
		"must equals limit":   "version: v1\nlimit: 8\norder: [authority]\nmust_include:\n" + protect + fert,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "v0.yaml"), src, 0o600); err != nil {
				t.Fatal(err)
			}
			if name != "missing" {
				if err := os.WriteFile(filepath.Join(dir, "v1.yaml"), []byte(v1), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LoadKnowledgeLoaders(dir, tx, snap); err == nil {
				t.Fatal("want error")
			}
		})
	}
	dir := t.TempDir()
	for name, body := range map[string][]byte{"v0.yaml": src, "v1.yaml": []byte(head + "must_include:\n" + protect + fert)} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadKnowledgeLoaders(dir, tx, snap); err != nil {
		t.Fatalf("valid v1: %v", err)
	}
	if _, err := LoadKnowledgeLoaders(dir, tx, nil); err == nil {
		t.Fatal("nil snapshot: want error")
	}
}

// worker 登记 kb-load-retention；retention.table_days.kb_load_item 非正数即启动失败；无 DB 不起任务。
func TestKBLoadLoops(t *testing.T) {
	a, err := Build(fullDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	wl, err := a.WorkerLoops()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(loopNames(wl), service.KBLoadRetentionJob) {
		t.Fatalf("worker loops %v missing %s", loopNames(wl), service.KBLoadRetentionJob)
	}
	a.L7.TableDays = map[string]int{kbLoadItemTable: 0}
	if _, err := kbLoadLoops(a); err == nil {
		t.Fatal("table_days.kb_load_item = 0: want error")
	}
	if got, err := kbLoadLoops(&App{}); got != nil || err != nil {
		t.Fatalf("no DB: %v, %v", got, err)
	}
	if (&App{}).knowledgeLoading(nil) != nil {
		t.Fatal("nil loaders must keep graph.Config.Loading nil (P1 behavior)")
	}
}

func pickRefs(ps []knowledge.Pick) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Entry.Ref()+":"+string(p.Reason))
	}
	return out
}
