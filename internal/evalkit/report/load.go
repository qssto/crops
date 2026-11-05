package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
)

// LoadGapsFile 是交给 L8 的 KB_MISS / 低命中切片清单文件名（P2 L5 功能任务 6），与 report.md 同目录。
const LoadGapsFile = "kb_load_gaps.json"

// loadNote 是加载指标节的口径说明。
const loadNote = "命中率 = must_cite 被加载的比例；精度 = RANKED 条目被卡片引用的比例（分母排除 MUST_INCLUDE / COMMON，与线上 kb_load_precision 同口径）；" +
	"无关注入率 = 与样本标注作物 / 生育期 / 问题类型不匹配的加载条目比例（evalkit 按样本标注离线判定）。各项为 trial 微平均。"

// renderLoad 输出知识加载指标（P2 L5），按问题类型切片；没有加载观测（无 trial 库）时只给一行说明。
func renderLoad(ew *errWriter, trials []evalkit.Trial) {
	r := reduce.ReduceLoad(trials)
	ew.printf("## 知识加载指标\n\n")
	if r.Observed == 0 {
		ew.printf("无加载观测（trial 无 run 详情或旧实验回放）。\n\n")
		return
	}
	ew.printf("> %s策略：%s\n\n", loadNote, joinOr(r.Strategies, "未记录"))
	head := []string{"问题类型", "样本", "trial"}
	for _, m := range reduce.LoadMetrics {
		title := m.Title()
		if m.LowerIsBetter() {
			title += "（越低越好）"
		}
		head = append(head, title)
	}
	ew.printf("| %s |\n|%s\n", strings.Join(head, " | "), strings.Repeat("----|", len(head)))
	for _, row := range append(r.ByType, r.Total) {
		cells := []string{row.Key, fmt.Sprint(row.Samples), fmt.Sprint(row.Trials)}
		for _, ratio := range row.Ratios {
			cells = append(cells, fmtRatio(ratio))
		}
		ew.printf("| %s |\n", strings.Join(cells, " | "))
	}
	ew.printf("\n")
}

// writeLoadGaps 在报告目录写 L8 清单（JSON）；没有加载观测时不写。
func writeLoadGaps(dir string, exp *evalkit.Experiment) error {
	if reduce.ReduceLoad(exp.Trials).Observed == 0 {
		return nil
	}
	raw, err := json.MarshalIndent(reduce.ReduceLoadGaps(exp.Trials), "", "  ")
	if err != nil {
		return fmt.Errorf("report: marshal load gaps: %w", err)
	}
	path := filepath.Join(dir, LoadGapsFile)
	if err := os.WriteFile(path, append(raw, '\n'), filePerm); err != nil {
		return fmt.Errorf("report: write %s: %w", path, err)
	}
	return nil
}
