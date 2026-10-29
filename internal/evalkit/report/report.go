// Package report 把一次评测实验汇总为 Markdown，写到 eval/out/{exp_id}/report.md。
// L4 起列出各打分维度与红线 pass^k（k = n）；L7 起每个 trial 的三层 transcript 上传 MinIO agri-steward-eval
// （transcript.go），明细表 run_id 列链到对象键；L8 起聚合交给 reduce（flaky / unexpected_call / error 清单、
// Gate 0 自动项），--baseline 时报告与快照上传 report/{exp_id}/ 并写 baseline yaml（baseline.go）。
package report

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
	"agri-steward/internal/model"
)

// FileName 是报告文件名。
const FileName = "report.md"

const (
	dirPerm  = 0o755
	filePerm = 0o644
)

// WriteFile 在 outRoot/{exp_id}/ 下写报告，返回报告路径。
func WriteFile(outRoot string, exp *evalkit.Experiment) (string, error) {
	if exp.ID == "" {
		return "", fmt.Errorf("report: experiment id is empty")
	}
	dir := filepath.Join(outRoot, exp.ID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("report: mkdir %s: %w", dir, err)
	}
	var b strings.Builder
	if err := Render(&b, exp); err != nil {
		return "", err
	}
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(b.String()), filePerm); err != nil {
		return "", fmt.Errorf("report: write %s: %w", path, err)
	}
	return path, nil
}

// stats 是一组 trial 的计数。
type stats struct {
	samples   map[string]struct{}
	trials    int
	advice    int
	refused   int
	errored   int
	matched   int // outcome 与样本期望一致（不含出错的 trial）
	status    map[string]int
	latencies time.Duration
}

func newStats() *stats {
	return &stats{samples: make(map[string]struct{}), status: make(map[string]int)}
}

func (s *stats) add(t evalkit.Trial) {
	s.samples[t.SampleID] = struct{}{}
	s.trials++
	s.status[t.Status()]++
	s.latencies += t.Latency
	if t.Error != "" {
		s.errored++
		return
	}
	switch t.Outcome {
	case model.OutcomeAdvice:
		s.advice++
	case model.OutcomeRefused:
		s.refused++
	}
	if t.Outcome == t.Expected {
		s.matched++
	}
}

func (s *stats) meanLatency() time.Duration {
	if s.trials == 0 {
		return 0
	}
	return s.latencies / time.Duration(s.trials)
}

// Render 输出 Markdown：首行门禁结论，然后配置快照、总览、Gate 0 自动项、baseline 指标、分项打分、红线 pass^k、
// flaky / unexpected_call / error 清单、按 tag 分组统计、trial 明细。聚合一律取 reduce.Reduce 的结果。
func Render(w io.Writer, exp *evalkit.Experiment) error {
	r := reduce.Reduce(exp.Trials)
	total := newStats()
	byTag := make(map[string]*stats)
	for _, t := range exp.Trials {
		total.add(t)
		for _, tag := range t.Tags {
			st, ok := byTag[tag]
			if !ok {
				st = newStats()
				byTag[tag] = st
			}
			st.add(t)
		}
	}

	ew := &errWriter{w: w}
	ew.printf("# 评测报告 %s\n\n", exp.ID)
	ew.printf("> %s\n\n", GateLine(r))
	renderSnapshot(ew, exp.Snapshot)

	ew.printf("## 总览\n\n")
	ew.printf("| 项 | 值 |\n|----|----|\n")
	ew.printf("| exp_id | `%s` |\n", exp.ID)
	ew.printf("| 套件 | %s |\n", joinOr(exp.Suites, "全部"))
	ew.printf("| tag 过滤 | %s |\n", joinOr(exp.Tags, "无"))
	ew.printf("| 每样本次数 n | %d |\n", exp.N)
	ew.printf("| 样本数 | %d |\n", exp.Samples)
	ew.printf("| trial 数 | %d |\n", total.trials)
	ew.printf("| ADVICE / REFUSED / error | %d / %d / %d |\n", total.advice, total.refused, total.errored)
	ew.printf("| outcome 与期望一致 | %d / %d |\n", total.matched, total.trials)
	ew.printf("| PASS / FAIL / INFRA_ERROR / GRADER_ERROR | %d / %d / %d / %d |\n", total.status[evalkit.StatusPass],
		total.status[evalkit.StatusFail], total.status[evalkit.StatusInfraError], total.status[evalkit.StatusGraderError])
	ew.printf("| 平均延迟 | %s |\n", fmtLatency(total.meanLatency()))
	ew.printf("| 开始时间（UTC） | %s |\n", exp.StartedAt.UTC().Format(time.RFC3339))
	ew.printf("| 总耗时 | %s |\n\n", fmtLatency(exp.FinishedAt.Sub(exp.StartedAt)))

	renderGate(ew, r)
	renderMetrics(ew, r)
	renderScores(ew, r)
	renderIssues(ew, r)

	ew.printf("## 按 tag 分组\n\n")
	ew.printf("| tag | 样本 | trial | ADVICE | REFUSED | error | 与期望一致 | 平均延迟 |\n")
	ew.printf("|-----|------|-------|--------|---------|-------|------------|----------|\n")
	tags := make([]string, 0, len(byTag))
	for tag := range byTag {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		st := byTag[tag]
		ew.printf("| %s | %d | %d | %d | %d | %d | %d | %s |\n", tag, len(st.samples), st.trials,
			st.advice, st.refused, st.errored, st.matched, fmtLatency(st.meanLatency()))
	}

	ew.printf("\n## trial 明细\n\n")
	ew.printf("| sample_id | trial | run_id | 期望 | outcome | refuse_reason | latency | unexpected_call | error | status | 未通过维度 |\n")
	ew.printf("|-----------|-------|--------|------|---------|---------------|---------|-----------------|-------|--------|------------|\n")
	for _, t := range exp.Trials {
		ew.printf("| %s | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", t.SampleID, t.TrialIndex, runCell(t), t.Expected,
			dash(string(t.Outcome)), dash(t.RefuseReason), fmtLatency(t.Latency),
			dash(strings.Join(t.UnexpectedCalls, ", ")), dash(escapeCell(t.Error)), t.Status(), dash(escapeCell(failedDims(t))))
	}
	if ew.err != nil {
		return fmt.Errorf("report: render: %w", ew.err)
	}
	return nil
}

// runCell 是 trial 明细的 run_id 列：已上传 transcript 时链接到 eval bucket 内的对象键
// （run 在 trial 库 agri_eval_t_{n}，生产 /runs 接口读不到，故不链 API）；无 run 时为 "-"。
func runCell(t evalkit.Trial) string {
	switch {
	case t.RunID == 0:
		return "-"
	case t.Transcript == "":
		return strconv.FormatUint(t.RunID, 10)
	default:
		return fmt.Sprintf("[%d](%s)", t.RunID, t.Transcript)
	}
}

// failedDims 列出未通过或出错的维度及原因。
func failedDims(t evalkit.Trial) string {
	var out []string
	for _, sc := range t.Scores {
		switch {
		case sc.Error != "":
			out = append(out, fmt.Sprintf("%s: grader_error %s", sc.Name, sc.Error))
		case !sc.Pass:
			out = append(out, fmt.Sprintf("%s: %s", sc.Name, sc.Evidence))
		}
	}
	return strings.Join(out, "; ")
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

// errWriter 记住第一个写错误，避免每行都判 err。
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}

func fmtLatency(d time.Duration) string {
	return fmt.Sprintf("%.3fms", float64(d)/float64(time.Millisecond))
}

func joinOr(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	return strings.Join(items, ", ")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// escapeCell 防止错误信息里的竖线和换行破坏表格。
func escapeCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}
