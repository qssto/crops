package compare

import (
	"fmt"
	"io"
	"strings"
)

// MultiFactorWarning 是单因子模式下差异因子 > 1 时报告首行的标红警告。
const MultiFactorWarning = `<span style="color:red">**多因子对比，结论不可归因**</span>`

// maxListed 是报告里清单（被排除样本 / flaky / 知识变更）最多列出的条数。
const maxListed = 50

// Render 写 Markdown 对比报告。单因子模式首行为多因子警告（不可归因时）或结论；阶段级模式首行并列两边 knowledge_ref 与知识变更。
func Render(w io.Writer, r *Result) error {
	var b strings.Builder
	r.renderHead(&b)
	r.renderFactors(&b)
	r.renderPairing(&b)
	r.renderMetrics(&b)
	_, err := io.WriteString(w, b.String())
	if err != nil {
		return fmt.Errorf("compare: write report: %w", err)
	}
	return nil
}

func (r *Result) renderHead(b *strings.Builder) {
	if r.Mode == ModePhase {
		fmt.Fprintf(b, "knowledge_ref：baseline `%s` | candidate `%s`；知识条目变更 %d 个文件%s\n\n",
			r.Baseline.KnowledgeRef, r.Candidate.KnowledgeRef, len(r.KnowledgeChanges), listInline(r.KnowledgeChanges))
		fmt.Fprintf(b, "# 阶段级对比：exp %d → exp %d\n\n", r.BaselineID, r.CandidateID)
		fmt.Fprintf(b, "整体结论：**%s**（阶段级只给整体结论，不做因子归因、不写门禁字段、不用于 PR 门禁）\n\n", r.Overall)
		return
	}
	if !r.Attributable() {
		fmt.Fprintf(b, "%s\n\n", MultiFactorWarning)
	}
	fmt.Fprintf(b, "# 单因子对比：exp %d → exp %d\n\n", r.BaselineID, r.CandidateID)
	release := "不可放行"
	if r.Releasable() {
		release = "可放行"
	}
	fmt.Fprintf(b, "结论：**%s**（%s）；整体：%s\n\n", r.Conclusion, release, r.Overall)
}

func (r *Result) renderFactors(b *strings.Builder) {
	b.WriteString("## 因子差异（忽略 git_sha）\n\n")
	if len(r.Diffs) == 0 {
		b.WriteString("无差异因子。\n\n")
	} else {
		b.WriteString("| 因子 | baseline | candidate |\n|----|----|----|\n")
		for _, d := range r.Diffs {
			fmt.Fprintf(b, "| %s | %s | %s |\n", d.Name, dash(d.Baseline), dash(d.Candidate))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "git_sha：baseline `%s` · candidate `%s`；samples_hash 一致：%t\n\n",
		r.Baseline.GitSHA, r.Candidate.GitSHA, r.Baseline.SamplesHash == r.Candidate.SamplesHash)
}

func (r *Result) renderPairing(b *strings.Builder) {
	fmt.Fprintf(b, "## 配对\n\n配对样本（(sample_id, sample_version) 交集）：%d；被排除样本：%d", r.Paired, len(r.Excluded))
	keys := make([]string, 0, len(r.Excluded))
	for _, e := range r.Excluded {
		keys = append(keys, e.Key+"（仅 "+e.Side+"）")
	}
	b.WriteString(listInline(keys) + "\n\n")
	if u := r.Uncovered(); len(u) > 0 {
		// 门禁据此判 INCOMPLETE，全部列出不截断。
		fmt.Fprintf(b, "baseline 样本未被 candidate 配对：%d：%s。candidate 须覆盖 baseline 的全部 sample_id@version，"+
			"否则结论为 INCOMPLETE，不可放行；样本改了版本时按当前样本重跑 baseline 配置（同一时段）后再对比。\n\n",
			len(u), strings.Join(u, "、"))
	}
	fmt.Fprintf(b, "统计：样本 n 次 trial 先合成样本级值（红线指标任一 trial 失败或出错即样本失败），配对 bootstrap %d 次，seed %d，差值 = candidate − baseline，95%% 区间。\n\n",
		r.Resamples, r.Seed)
	for _, side := range []string{sideBaseline, sideCandidate, sideReference} {
		if c, ok := r.Errors[side]; ok {
			fmt.Fprintf(b, "- %s：infra_error %d、grader_error %d；受影响样本 %d%s\n", side, c[0], c[1],
				len(r.ErrorSamples[side]), listInline(r.ErrorSamples[side]))
		}
	}
	if r.Incomplete() {
		b.WriteString("\n出错 trial 的处理：红线指标按失败计（样本内取最差）；非红线指标跳过出错 trial；baseline / candidate 有出错 trial 时结论为 INCOMPLETE，不可放行。\n")
	}
	b.WriteString("\n")
}

func (r *Result) renderMetrics(b *strings.Builder) {
	b.WriteString("## 质量指标\n\n")
	ref := r.ReferenceID != 0
	if ref {
		fmt.Fprintf(b, "第三列为参考实验 exp %d：知识侧 = reference − baseline，agent 侧 = candidate − reference（只出报告不判定）。\n\n", r.ReferenceID)
		b.WriteString("| 指标 | 样本 | baseline | reference | candidate | 差值 [95% 区间] | 知识侧 | agent 侧 | 结论 |\n|----|----|----|----|----|----|----|----|----|\n")
	} else {
		b.WriteString("| 指标 | 样本 | baseline | candidate | 差值 [95% 区间] | 结论 |\n|----|----|----|----|----|----|\n")
	}
	var flaky []string
	for _, m := range r.Metrics {
		name := m.Title + "（" + m.Name + "）"
		if m.Pending != "" || m.Samples == 0 {
			note := string(m.Verdict)
			if m.Pending != "" {
				note = m.Pending
			}
			if ref {
				fmt.Fprintf(b, "| %s | - | - | - | - | - | - | - | %s |\n", name, note)
			} else {
				fmt.Fprintf(b, "| %s | - | - | - | - | %s |\n", name, note)
			}
			continue
		}
		if ref {
			fmt.Fprintf(b, "| %s | %d | %.3f | %.3f | %.3f | %s | %s | %s | %s |\n", name, m.Samples, m.BaselineMean,
				m.ReferenceMean, m.CandidateMean, ivText(m.Diff), ivPtr(m.Knowledge), ivPtr(m.Agent), m.Verdict)
		} else {
			fmt.Fprintf(b, "| %s | %d | %.3f | %.3f | %s | %s |\n", name, m.Samples, m.BaselineMean, m.CandidateMean, ivText(m.Diff), m.Verdict)
		}
		for _, k := range m.Flaky {
			flaky = append(flaky, m.Name+":"+k)
		}
	}
	fmt.Fprintf(b, "\nflaky 配对样本（照样本级值参与，红线按失败）：%d%s\n", len(flaky), listInline(flaky))
}

func ivText(iv Interval) string { return fmt.Sprintf("%+.3f [%+.3f, %+.3f]", iv.Mean, iv.Low, iv.High) }

func ivPtr(iv *Interval) string {
	if iv == nil {
		return "-"
	}
	return ivText(*iv)
}

// listInline 把清单写成「：a、b、…」（超过 maxListed 截断并注明总数）；空清单为空串。
func listInline(items []string) string {
	if len(items) == 0 {
		return ""
	}
	shown := items[:min(len(items), maxListed)]
	s := "：" + strings.Join(shown, "、")
	if len(items) > maxListed {
		s += fmt.Sprintf(" 等（共 %d）", len(items))
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
