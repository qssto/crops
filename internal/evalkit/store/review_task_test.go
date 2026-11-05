package store

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/model"
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

// 红线（P2-L2b）：评测侧不得写 ONLINE_JUDGE 任务（draft_ref 指向业务库行，只能由业务侧入队任务生成）；
// 在碰库前拒绝（nil db 碰库即 panic）。
func TestReviews_InsertRejectsOnlineJudge(t *testing.T) {
	tests := []struct {
		name    string
		tasks   []ReviewTask
		wantErr error
	}{
		{name: "online judge", tasks: []ReviewTask{{TaskType: model.ReviewOnlineJudge, DraftRef: model.OnlineJudgeRef(1, 2, 3)}},
			wantErr: ErrOnlineJudgeTask},
		{name: "mixed batch", tasks: []ReviewTask{{TaskType: TaskBackflowExpect, DraftRef: "d-1"},
			{TaskType: model.ReviewOnlineJudge, DraftRef: model.OnlineJudgeRef(1, 2, 3)}}, wantErr: ErrOnlineJudgeTask},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewReviews(nil).Insert(context.Background(), tt.tasks, time.Unix(0, 0))
			if !errors.Is(err, tt.wantErr) || n != 0 {
				t.Fatalf("Insert = %d, %v, want %v", n, err, tt.wantErr)
			}
		})
	}
	if _, err := NewReviews(nil).Insert(context.Background(), []ReviewTask{{}}, time.Unix(0, 0)); err == nil {
		t.Fatal("empty task_type must be rejected")
	}
}
