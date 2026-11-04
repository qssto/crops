package review

import (
	"time"

	"agri-steward/internal/evalkit/store"
)

// DefaultRecheckSize 是月度复核每月抽检 trial 数（路线 §6.3 功能任务 10）。
const DefaultRecheckSize = 20

// MonthWindow 是 now 所在自然月 [月初, 次月初)，按 loc 切月（复核按北京时间的月份算）；返回 UTC。
func MonthWindow(now time.Time, loc *time.Location) (from, to time.Time) {
	l := now.In(loc)
	from = time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, loc)
	return from.UTC(), from.AddDate(0, 1, 0).UTC()
}

// MonthTag 是 now 在 loc 下的月份（YYYY-MM），种子与报告用。
func MonthTag(now time.Time, loc *time.Location) string { return now.In(loc).Format("2006-01") }

// MonthSeed 是由月份派生的 seed（同月重跑抽到同一批）：YYYYMM。
func MonthSeed(now time.Time, loc *time.Location) uint64 {
	l := now.In(loc)
	return uint64(l.Year()*100 + int(l.Month()))
}

// PlanRecheck 从候选中按 seed 随机抽 size 个 trial 生成 MONTHLY_RECHECK 任务（rater_no=1）。
// 候选不足 size 时全部入选。
func PlanRecheck(expID uint64, dim, version string, cands []Candidate, size int, seed uint64) []store.ReviewTask {
	if size <= 0 {
		size = DefaultRecheckSize
	}
	n := min(size, len(cands))
	out := make([]store.ReviewTask, 0, n)
	for _, i := range newRand(seed).Perm(len(cands))[:n] {
		out = append(out, trialTask(store.TaskMonthlyRecheck, expID, dim, version, cands[i], store.RaterPrimary))
	}
	return out
}
