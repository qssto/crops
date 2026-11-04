package coverage

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/sample"
)

// BelowMark 标在低于目标的格子与项后面（纯文本也能看出「红」）；Color 为 true 时整行另加 ANSI 红色。
const BelowMark = "!"

const (
	ansiRed   = "\x1b[31m"
	ansiReset = "\x1b[0m"
)

// RenderOptions 控制输出：Color 打开 ANSI 红色（终端用；写文件时关掉）。
type RenderOptions struct {
	Color bool
}

// Render 输出完整覆盖报表：总数、格子计数对目标、拒答三来源、陷阱组、补写清单（逐格缺口与按 一级分类 × 生育期 × 难度 汇总）。
func Render(w io.Writer, r *Report, tx *knowledge.Taxonomy, opt RenderOptions) error {
	p := &printer{w: w, color: opt.Color}
	t := r.Targets
	p.line(r.Counted < t.TotalMin, "coverage: %d / %d samples%s (%s; quarantined %d still counted)",
		r.Counted, t.TotalMin, mark(r.Counted < t.TotalMin), suiteSummary(r.BySuite), r.Quarantined)
	p.line(false, "user_sim: %d (counted separately, not in total)", r.UserSim)
	if r.HoldoutPresent {
		p.line(false, "holdout: %d file(s) (content not read, not in total)", r.Holdout)
	} else {
		p.line(false, "holdout: absent (skipped)")
	}
	p.renderCells(r)
	p.renderRatio("refuse", r, r.Refused, t.Refuse, RefuseSources, r.RefuseBySrc)
	if r.RefuseOther > 0 {
		p.line(false, "  other reasons %d (in refuse total, not a source)", r.RefuseOther)
	}
	p.renderRatio("trap", r, r.Trapped, t.Traps, TrapGroups, r.TrapByGroup)
	p.renderGaps(r.Gaps(tx))
	return p.err
}

type printer struct {
	w     io.Writer
	color bool
	err   error
}

// line 打印一行；below 且开启颜色时整行标红。
func (p *printer) line(below bool, format string, args ...any) {
	if p.err != nil {
		return
	}
	s := fmt.Sprintf(format, args...)
	if below && p.color {
		s = ansiRed + s + ansiReset
	}
	_, p.err = fmt.Fprintln(p.w, s)
}

func mark(below bool) string {
	if below {
		return " " + BelowMark
	}
	return ""
}

func suiteSummary(by map[string]int) string {
	suites := make([]string, 0, len(by))
	for s := range by {
		suites = append(suites, s)
	}
	slices.Sort(suites)
	parts := make([]string, 0, len(suites))
	for _, s := range suites {
		parts = append(parts, fmt.Sprintf("%s %d", s, by[s]))
	}
	return strings.Join(parts, ", ")
}

// renderCells 打印目标行（按目标文件行序）与目标外有样本的格子；每格 have/want，低于目标标记。
func (p *printer) renderCells(r *Report) {
	p.line(false, "")
	p.line(false, "cells (have/want per difficulty; %s = below target):", BelowMark)
	header := []string{"stage", "topic", "easy", "medium", "hard", "total"}
	widths := []int{12, 20, 9, 9, 9, 9}
	p.line(false, "  %s", pad(header, widths))
	for _, c := range r.Targets.Cells {
		k := CellKey{c.Stage, c.Topic}
		row := []string{c.Stage, c.Topic}
		below, have := false, 0
		for _, d := range sample.Difficulties {
			n, want := r.Count(k, d), c.ByDifficulty(d)
			have += n
			row = append(row, fmt.Sprintf("%d/%d%s", n, want, markShort(n < want)))
			below = below || n < want
		}
		row = append(row, fmt.Sprintf("%d/%d", have, c.Sum()))
		p.line(below, "  %s", pad(row, widths))
	}
	off := r.OffTarget()
	if len(off) == 0 {
		return
	}
	p.line(false, "  -- off target (samples in cells without a target row):")
	for _, k := range off {
		row := []string{k.Stage, k.Topic}
		total := 0
		for _, d := range sample.Difficulties {
			total += r.Count(k, d)
			row = append(row, fmt.Sprintf("%d", r.Count(k, d)))
		}
		row = append(row, fmt.Sprintf("%d", total))
		p.line(false, "  %s", pad(row, widths))
	}
}

func markShort(below bool) string {
	if below {
		return BelowMark
	}
	return ""
}

func (p *printer) renderRatio(name string, r *Report, n int, t RatioTarget, keys []string, have map[string]int) {
	p.line(false, "")
	ratio := r.Ratio(n)
	p.line(ratio < t.RatioMin, "%s: %d/%d = %.1f%% (min %.0f%%)%s", name, n, r.Counted, 100*ratio, 100*t.RatioMin, mark(ratio < t.RatioMin))
	for _, k := range keys {
		below := have[k] < t.Min[k]
		p.line(below, "  %-18s %3d (%4.1f%%)  min %d%s", k, have[k], 100*r.Ratio(have[k]), t.Min[k], mark(below))
	}
}

// renderGaps 打印补写清单：先按 一级分类 × 生育期 × 难度 汇总，再逐格列缺口。
func (p *printer) renderGaps(gaps []Gap) {
	p.line(false, "")
	total := TotalMissing(gaps)
	p.line(total > 0, "补写清单: %d sample(s) missing in %d slot(s) (cell × difficulty)", total, len(gaps))
	if total == 0 {
		return
	}
	p.line(false, "  by category × stage × difficulty:")
	for _, s := range Summarize(gaps) {
		p.line(true, "  %s", pad([]string{s.Category, s.Stage, s.Difficulty, fmt.Sprintf("+%d", s.Missing)}, []int{10, 12, 7, 5}))
	}
	p.line(false, "  by cell (stage × topic × difficulty: have/want):")
	for _, g := range gaps {
		p.line(true, "  %s", pad([]string{g.Key.Stage, g.Key.Topic, g.Difficulty,
			fmt.Sprintf("%d/%d", g.Have, g.Want), fmt.Sprintf("+%d", g.Missing())}, []int{12, 20, 7, 6, 5}))
	}
}

// GapSummary 是补写清单的一行汇总（一级分类 × 生育期 × 难度）。
type GapSummary struct {
	Category, Stage, Difficulty string
	Missing                     int
}

// Summarize 把逐格缺口按 一级分类 × 生育期 × 难度 汇总：一级分类按首次出现序，
// 生育期按 knowledge.Stages 物候序（「不限」及未知生育期殿后），难度按 easy / medium / hard。
func Summarize(gaps []Gap) []GapSummary {
	var out []GapSummary
	idx := map[[3]string]int{}
	catOrder := map[string]int{}
	for _, g := range gaps {
		if _, ok := catOrder[g.Category]; !ok {
			catOrder[g.Category] = len(catOrder)
		}
		k := [3]string{g.Category, g.Key.Stage, g.Difficulty}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, GapSummary{Category: g.Category, Stage: g.Key.Stage, Difficulty: g.Difficulty})
		}
		out[i].Missing += g.Missing()
	}
	slices.SortStableFunc(out, func(a, b GapSummary) int {
		return cmp.Or(cmp.Compare(catOrder[a.Category], catOrder[b.Category]),
			cmp.Compare(stageRank(a.Stage), stageRank(b.Stage)),
			cmp.Compare(slices.Index(sample.Difficulties, a.Difficulty), slices.Index(sample.Difficulties, b.Difficulty)))
	})
	return out
}

// stageRank 是生育期的物候序；不在 knowledge.Stages 中的（「不限」等）排最后。
func stageRank(stage string) int {
	if i := slices.Index(knowledge.Stages, stage); i >= 0 {
		return i
	}
	return len(knowledge.Stages)
}

// pad 按显示宽度（CJK 记 2）左对齐各列。
func pad(cols []string, widths []int) string {
	var b strings.Builder
	for i, c := range cols {
		b.WriteString(c)
		if i == len(cols)-1 {
			break
		}
		for n := displayWidth(c); n < widths[i]; n++ {
			b.WriteByte(' ')
		}
		b.WriteByte(' ')
	}
	return b.String()
}

func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		if utf8.RuneLen(r) >= 3 { // CJK 及全角符号
			n += 2
		} else {
			n++
		}
	}
	return n
}
