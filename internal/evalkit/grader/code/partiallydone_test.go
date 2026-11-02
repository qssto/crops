package code

import (
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

func TestPartiallyDone(t *testing.T) {
	want := &sample.Sample{ID: "w", Expect: sample.Expect{WorkDone: &sample.WorkDoneExpect{Status: "PARTIALLY_DONE", DoneAreaMu: 4}}}
	area := func(v float64) *float64 { return &v }
	good := grader.WorkDoneObs{OrderID: 7, Status: model.OrderPartiallyDone, DoneAreaMu: area(4), LocalVisible: true, FieldHistory: true}
	with := func(mut func(*grader.WorkDoneObs)) *grader.Trial {
		o := good
		mut(&o)
		return &grader.Trial{Sample: want, WorkDone: []grader.WorkDoneObs{o}}
	}
	runCases(t, PartiallyDone{}, []gradeCase{
		{name: "matching order passes", pass: true, t: &grader.Trial{Sample: want, WorkDone: []grader.WorkDoneObs{good}}},
		{name: "any matching order passes", pass: true, t: &grader.Trial{Sample: want,
			WorkDone: []grader.WorkDoneObs{{OrderID: 6, Status: model.OrderCanceled}, good}}},
		{name: "not asserted passes", pass: true, t: &grader.Trial{Sample: &sample.Sample{ID: "p"}}},
		{name: "not recorded is grader error", wantErr: true, t: &grader.Trial{Sample: want}},
		{name: "no order fails", evidence: "no work order", t: &grader.Trial{Sample: want, WorkDone: []grader.WorkDoneObs{}}},
		{name: "wrong status fails", evidence: "status DONE, want PARTIALLY_DONE",
			t: with(func(o *grader.WorkDoneObs) { o.Status = model.OrderDone })},
		{name: "area not backfilled fails", evidence: "done_area_mu not backfilled",
			t: with(func(o *grader.WorkDoneObs) { o.DoneAreaMu = nil })},
		{name: "area mismatch fails", evidence: "done_area_mu 3.50, want 4.00",
			t: with(func(o *grader.WorkDoneObs) { o.DoneAreaMu = area(3.5) })},
		{name: "invisible to PHI fails", evidence: "not visible to LocalOrderRepo",
			t: with(func(o *grader.WorkDoneObs) { o.LocalVisible = false })},
		{name: "missing field.history fails", evidence: "not in field.history",
			t: with(func(o *grader.WorkDoneObs) { o.FieldHistory = false })},
	})
}
