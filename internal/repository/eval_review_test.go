package repository

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
)

func u64(v uint64) *uint64 { return &v }

// 红线（P2-L2b）：同一 trial 同一维度的主标 / 双标不得是同一专家；他人持有 / 他人完成 / SKIPPED 一律冲突（409），
// 本专家已完成的行返回原行（由调用方判重放）。
func TestClaimable(t *testing.T) {
	const me, other = 9002, 9003
	tests := []struct {
		name    string
		rows    []raterLockRow
		wantErr error
		want    string // 返回行的状态
	}{
		{name: "open unassigned", rows: []raterLockRow{{ID: 1, Status: model.ReviewOpen}}, want: model.ReviewOpen},
		{name: "assigned to me", rows: []raterLockRow{{ID: 1, Status: model.ReviewAssigned, AssigneeID: u64(me)}},
			want: model.ReviewAssigned},
		{name: "done by me", rows: []raterLockRow{{ID: 1, Status: model.ReviewDone, AssigneeID: u64(me)}}, want: model.ReviewDone},
		{name: "assigned to other", rows: []raterLockRow{{ID: 1, Status: model.ReviewAssigned, AssigneeID: u64(other)}},
			wantErr: ErrReviewConflict},
		{name: "done by other", rows: []raterLockRow{{ID: 1, Status: model.ReviewDone, AssigneeID: u64(other)}},
			wantErr: ErrReviewConflict},
		{name: "open but held by other", rows: []raterLockRow{{ID: 1, Status: model.ReviewOpen, AssigneeID: u64(other)}},
			wantErr: ErrReviewConflict},
		{name: "skipped", rows: []raterLockRow{{ID: 1, Status: model.ReviewSkipped}}, wantErr: ErrReviewConflict},
		{name: "other rater is me", rows: []raterLockRow{{ID: 1, Status: model.ReviewOpen},
			{ID: 2, Status: model.ReviewDone, AssigneeID: u64(me)}}, wantErr: ErrReviewConflict},
		{name: "other rater is someone else", rows: []raterLockRow{{ID: 1, Status: model.ReviewOpen},
			{ID: 2, Status: model.ReviewDone, AssigneeID: u64(other)}}, want: model.ReviewOpen},
		{name: "missing", rows: []raterLockRow{{ID: 2, Status: model.ReviewOpen}}, wantErr: model.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := claimable(tt.rows, 1, me)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && got.Status != tt.want {
				t.Fatalf("status = %s, want %s", got.Status, tt.want)
			}
		})
	}
}

// 红线（P2-L2b）：业务库回写的审计须与被回写行同农场；不一致在碰库前拒绝（零值 DB 碰库即 panic）。
func TestReviewJudgeResultAudited_AuditScope(t *testing.T) {
	scope := model.Scope{TenantID: 1, FarmID: 2}
	for _, rec := range []model.AuditRecord{{}, {TenantID: 1}, {TenantID: 1, FarmID: 3}, {TenantID: 9, FarmID: 2}} {
		err := QualityRepo{}.ReviewJudgeResultAudited(context.Background(), scope, 1, 0, 2, 9, rec, time.Unix(0, 0))
		if !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("audit (%d,%d): err = %v, want ErrScopeMismatch", rec.TenantID, rec.FarmID, err)
		}
	}
}

// 红线（P2-L2b）：holdout 判定失败关闭——挂了样本的任务只有找到 trial 且 suite 不是 holdout 才算非 holdout。
func TestReviewSelect_HoldoutFailClosed(t *testing.T) {
	sql := strings.Join(strings.Fields(reviewSelect), " ")
	for _, want := range []string{
		"WHEN rt.experiment_id IS NULL AND rt.sample_id IS NULL THEN 0", // 未挂样本（ONLINE_JUDGE / BACKFLOW_EXPECT）
		"MAX(tr.suite = 'holdout')",
		"tr.experiment_id = rt.experiment_id AND tr.sample_id = rt.sample_id AND tr.trial_index = rt.trial_index), 1)", // 找不到 trial 记 1
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("reviewSelect missing %q", want)
		}
	}
}

// 红线（P2-L2b）：专家回写 online_judge_result 的 UPDATE 条件带 tenant_id 与 farm_id（并带 row_ver 乐观锁）。
func TestReviewJudgeSQL_ScopedWhere(t *testing.T) {
	b, err := os.ReadFile("quality_judge.go")
	if err != nil {
		t.Fatalf("read quality_judge.go: %v", err)
	}
	src := string(b)
	re := regexp.MustCompile("(?s)UPDATE online_judge_result SET expert_score.*?WHERE ([^`]*)`")
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("expert writeback UPDATE not found")
	}
	if !strings.HasPrefix(strings.TrimSpace(m[1]), "tenant_id = ? AND farm_id = ? AND id = ? AND row_ver = ?") {
		t.Fatalf("writeback WHERE = %q", m[1])
	}
	if n := strings.Count(src, "UPDATE online_judge_result"); n != 1 {
		t.Fatalf("%d UPDATE online_judge_result statements, want 1 shared by both writeback paths", n)
	}
	// P2-L2b 评审路径只回写 NEED_EXPERT 行（draft_ref 可被评测库写权限伪造，状态条件挡住 SCORED / REVIEWED 行）。
	if s := reviewJudgeStmt(true); !strings.HasSuffix(s, "AND row_ver = ? AND status = 'NEED_EXPERT'") {
		t.Fatalf("audited writeback = %q, want status = 'NEED_EXPERT' condition", s)
	}
	if s := reviewJudgeStmt(false); strings.Contains(s, "NEED_EXPERT") {
		t.Fatalf("legacy writeback = %q, must stay §6.4 ② verbatim", s)
	}
}
