package code

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/model"
)

// doneAreaTolerance 是 done_area_mu 比较容差（亩，库中 DECIMAL 两位小数）。
const doneAreaTolerance = 0.005

var errNoWorkDone = errors.New("work done state not recorded")

// PartiallyDone 是 partially_done 打分器（P1 §6.5 第 8 条回执侧）：样本断言 expect.work_done 时，
// 须有一张作业单终态为期望状态、work_order.done_area_mu 回填为期望值、被 LocalOrderRepo 读到（后续 PHI 推算能用上），
// 且已回写 field.history。未断言的样本通过；断言了但 WorkDone 未记录返回 grader_error。
type PartiallyDone struct{}

var _ grader.Grader = PartiallyDone{}

// Name 返回打分器名。
func (PartiallyDone) Name() string { return "partially_done" }

// RedLine 返回 true（P1 L8：回写缺失会让后续安全间隔期推算失真，与 recheck_returned / expired 同列红线）。
func (PartiallyDone) RedLine() bool { return true }

// Grade 打分。
func (g PartiallyDone) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if err := checkTrial(g.Name(), t); err != nil {
		return grader.Score{}, err
	}
	exp := t.Sample.Expect.WorkDone
	if exp == nil {
		return grader.NotApplicable("no expect.work_done"), nil
	}
	if t.WorkDone == nil {
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoWorkDone)
	}
	want := model.WorkOrderStatus(exp.Status)
	notes := make([]string, 0, len(t.WorkDone))
	for _, o := range t.WorkDone {
		fails := workDoneFails(o, want, exp.DoneAreaMu)
		if len(fails) == 0 {
			return grader.Score{Pass: true, Value: 1}, nil
		}
		notes = append(notes, fmt.Sprintf("order %d: %s", o.OrderID, strings.Join(fails, ", ")))
	}
	if len(notes) == 0 {
		notes = append(notes, "no work order")
	}
	return grader.Score{Evidence: strings.Join(notes, "; ")}, nil
}

// workDoneFails 列出一张作业单不满足断言的方面（空表示满足）。
func workDoneFails(o grader.WorkDoneObs, want model.WorkOrderStatus, area float64) []string {
	var fails []string
	if o.Status != want {
		fails = append(fails, fmt.Sprintf("status %s, want %s", o.Status, want))
	}
	switch {
	case o.DoneAreaMu == nil:
		fails = append(fails, "done_area_mu not backfilled")
	case math.Abs(*o.DoneAreaMu-area) > doneAreaTolerance:
		fails = append(fails, fmt.Sprintf("done_area_mu %.2f, want %.2f", *o.DoneAreaMu, area))
	}
	if !o.LocalVisible {
		fails = append(fails, "not visible to LocalOrderRepo (PHI)")
	}
	if !o.FieldHistory {
		fails = append(fails, "not in field.history")
	}
	return fails
}
