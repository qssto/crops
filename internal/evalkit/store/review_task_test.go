package store

import (
	"crypto/md5"
	"encoding/hex"
	"testing"
)

func TestReviewTask_Key(t *testing.T) {
	exp, idx := uint64(42), 0
	sum := md5.Sum([]byte("CALIBRATION|42|rice-p2-prot-0001|0|fact_consistency||2"))
	want := hex.EncodeToString(sum[:])
	got := ReviewTask{TaskType: TaskCalibration, ExperimentID: &exp, SampleID: "rice-p2-prot-0001", TrialIndex: &idx,
		Dimension: "fact_consistency", RaterNo: RaterSecond}.Key()
	if got != want {
		t.Fatalf("key = %s, want %s", got, want)
	}
	// rater_no 零值按 1；NULL 列记空串（BACKFLOW_EXPECT 只有 draft_ref）。
	sum = md5.Sum([]byte("BACKFLOW_EXPECT|||||d-1|1"))
	if got := (ReviewTask{TaskType: TaskBackflowExpect, DraftRef: "d-1"}).Key(); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("backflow key = %s", got)
	}
}
