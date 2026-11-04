package coverage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/sample"
)

// HoldoutSuite 是私有 holdout 套件（L2 以 submodule 挂到 eval/suites/holdout/）：未授权时只数文件，不读内容、不进格子；
// 授权读者（CountWithHoldout）把 holdout 样本计入格子与总数。
const HoldoutSuite = sample.HoldoutDir

// Report 是一次覆盖统计的结果（数据，不含格式）。
type Report struct {
	Targets *Targets
	// Counted 是计入格子与总数的样本数（不含 holdout 套件与 user_sim）。
	Counted int
	// BySuite 是计入样本按套件计数；Quarantined 是其中隔离样本数（仍计入：隔离是暂时的，样本仍在集内）。
	BySuite     map[string]int
	Quarantined int
	UserSim     int
	// Holdout 是 holdout 目录下的样本文件数；HoldoutPresent 为 false 表示目录不存在（跳过）。
	Holdout        int
	HoldoutPresent bool
	// HoldoutCounted 是计入格子的 holdout 样本数，HoldoutReader 是授权读者；未授权时为 0 / 空。
	HoldoutCounted int
	HoldoutReader  string

	// Cells 是 (生育期, 问题类型) → 难度 → 条数，含目标外的格子。
	Cells map[CellKey]map[string]int

	Refused     int
	RefuseBySrc map[string]int // RefuseSources 之一，其余 refuse_reason 记入 RefuseOther
	RefuseOther int
	Trapped     int            // 带任一陷阱组 tag 的样本数
	TrapByGroup map[string]int // 一条样本可属于多个组
}

// Count 统计 samples（已通过静态校验）的覆盖；suitesDir 用来数 holdout 文件。
// holdout 样本（suite 为 holdout 或路径在 suitesDir/holdout 下）不计入，只按文件数计。
func Count(samples []*sample.Sample, tx *knowledge.Taxonomy, t *Targets, suitesDir string) (*Report, error) {
	return count(samples, tx, t, suitesDir, nil)
}

// CountWithHoldout 同 Count，但把 holdout 样本计入格子与总数（P2-L1 待办：公开侧缺的格子由 holdout 补齐）；
// g 须是有效凭证，否则返回 sample.ErrHoldoutDenied。报表对 holdout 只出计数。
func CountWithHoldout(samples []*sample.Sample, tx *knowledge.Taxonomy, t *Targets, suitesDir string, g *sample.HoldoutGrant) (*Report, error) {
	if g.Reader() == "" {
		return nil, sample.ErrHoldoutDenied
	}
	return count(samples, tx, t, suitesDir, g)
}

func count(samples []*sample.Sample, tx *knowledge.Taxonomy, t *Targets, suitesDir string, g *sample.HoldoutGrant) (*Report, error) {
	r := &Report{Targets: t, BySuite: map[string]int{}, Cells: map[CellKey]map[string]int{},
		RefuseBySrc: map[string]int{}, TrapByGroup: map[string]int{}, HoldoutReader: g.Reader()}
	for _, s := range samples {
		holdout := s.Holdout || s.Suite == HoldoutSuite || (s.Path != "" && sample.IsHoldoutPath(suitesDir, s.Path))
		switch {
		case holdout && g == nil:
			continue // 由 countHoldout 按文件数计，不看内容
		case slices.Contains(s.Tags, sample.TagUserSim):
			r.UserSim++
			continue
		case holdout:
			r.HoldoutCounted++
		}
		r.Counted++
		r.BySuite[s.Suite]++
		if _, ok := s.QuarantineReason(); ok {
			r.Quarantined++
		}
		st := sample.StrataOf(s, tx)
		k := CellKey{st.Stage, st.Topic}
		if r.Cells[k] == nil {
			r.Cells[k] = map[string]int{}
		}
		r.Cells[k][st.Difficulty]++
		if s.IsRefusal() {
			r.Refused++
			if slices.Contains(RefuseSources, s.Expect.RefuseReason) {
				r.RefuseBySrc[s.Expect.RefuseReason]++
			} else {
				r.RefuseOther++
			}
		}
		if traps := sample.TrapTags(s); len(traps) > 0 {
			r.Trapped++
			for _, g := range traps {
				r.TrapByGroup[g]++
			}
		}
	}
	n, present, err := countHoldout(filepath.Join(suitesDir, HoldoutSuite))
	if err != nil {
		return nil, err
	}
	r.Holdout, r.HoldoutPresent = n, present
	return r, nil
}

// countHoldout 只按文件名数 holdout 下的样本 YAML，不打开文件。
func countHoldout(dir string) (int, bool, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, fmt.Errorf("stat holdout: %w", err)
	}
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && sample.IsSampleFile(d.Name()) {
			n++
		}
		return nil
	})
	if err != nil {
		return 0, true, fmt.Errorf("walk holdout: %w", err)
	}
	return n, true, nil
}

// Count 返回某格某档难度的样本数。
func (r *Report) Count(k CellKey, difficulty string) int { return r.Cells[k][difficulty] }

// Ratio 返回 n / Counted（Counted 为 0 时为 0）。
func (r *Report) Ratio(n int) float64 {
	if r.Counted == 0 {
		return 0
	}
	return float64(n) / float64(r.Counted)
}

// Gap 是一个格子某档难度的缺口。
type Gap struct {
	Key        CellKey
	Category   string
	Difficulty string
	Have, Want int
}

// Missing 返回缺多少条。
func (g Gap) Missing() int { return g.Want - g.Have }

// TotalMissing 返回全部缺口条数之和；一个 Gap 是一个「槽位」（格子 × 难度）。
func TotalMissing(gaps []Gap) int {
	n := 0
	for _, g := range gaps {
		n += g.Missing()
	}
	return n
}

// Gaps 返回全部低于目标的格子（按目标文件行序、难度序）。
func (r *Report) Gaps(tx *knowledge.Taxonomy) []Gap {
	var out []Gap
	for _, c := range r.Targets.Cells {
		k := CellKey{c.Stage, c.Topic}
		cat, _ := tx.CategoryOf(c.Topic)
		for _, d := range sample.Difficulties {
			if have, want := r.Count(k, d), c.ByDifficulty(d); have < want {
				out = append(out, Gap{Key: k, Category: cat, Difficulty: d, Have: have, Want: want})
			}
		}
	}
	return out
}

// OffTarget 返回有样本但不在目标文件中的格子（排序）。
func (r *Report) OffTarget() []CellKey {
	known := make(map[CellKey]bool, len(r.Targets.Cells))
	for _, c := range r.Targets.Cells {
		known[CellKey{c.Stage, c.Topic}] = true
	}
	var out []CellKey
	for k := range r.Cells {
		if !known[k] {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Topic != out[j].Topic {
			return out[i].Topic < out[j].Topic
		}
		return out[i].Stage < out[j].Stage
	})
	return out
}

// Shortfalls 返回全部未达标项的一句话说明；空表示覆盖达标（--strict-coverage 据此决定退出码）。
func (r *Report) Shortfalls(tx *knowledge.Taxonomy) []string {
	var out []string
	if r.Counted < r.Targets.TotalMin {
		out = append(out, fmt.Sprintf("total %d < %d", r.Counted, r.Targets.TotalMin))
	}
	if gaps := r.Gaps(tx); len(gaps) > 0 {
		out = append(out, fmt.Sprintf("%d slot(s) below target, %d sample(s) missing", len(gaps), TotalMissing(gaps)))
	}
	out = append(out, ratioShortfalls("refuse", r.Ratio(r.Refused), r.Targets.Refuse, RefuseSources, r.RefuseBySrc)...)
	return append(out, ratioShortfalls("trap", r.Ratio(r.Trapped), r.Targets.Traps, TrapGroups, r.TrapByGroup)...)
}

func ratioShortfalls(name string, ratio float64, t RatioTarget, keys []string, have map[string]int) []string {
	var out []string
	if ratio < t.RatioMin {
		out = append(out, fmt.Sprintf("%s ratio %.1f%% < %.0f%%", name, 100*ratio, 100*t.RatioMin))
	}
	for _, k := range keys {
		if have[k] < t.Min[k] {
			out = append(out, fmt.Sprintf("%s %s %d < %d", name, k, have[k], t.Min[k]))
		}
	}
	return out
}
