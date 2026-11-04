package store

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/repository"
)

// review_task.task_type 取值（P2 §6.3 SQL）。
const (
	TaskCalibration    = "CALIBRATION"
	TaskHumanQueue     = "HUMAN_QUEUE"
	TaskMonthlyRecheck = "MONTHLY_RECHECK"
	TaskBackflowExpect = "BACKFLOW_EXPECT"
)

// review_task.queue_reason 取值（HUMAN_QUEUE 四类入口）。
const (
	ReasonUnknown  = "UNKNOWN"
	ReasonFlaky    = "FLAKY"
	ReasonConflict = "CONFLICT"
	ReasonRandom10 = "RANDOM_10"
)

// review_task.status 取值。
const (
	ReviewOpen     = "OPEN"
	ReviewAssigned = "ASSIGNED"
	ReviewDone     = "DONE"
	ReviewSkipped  = "SKIPPED"
)

// 专家分数范围（1-3 有序量表，与 judge 一致）。
const (
	minExpertScore = 1
	maxExpertScore = 3
	// RaterPrimary / RaterSecond 是主标与双标。
	RaterPrimary = 1
	RaterSecond  = 2
	// reviewInsertBatch 是一条 INSERT 的最大行数。
	reviewInsertBatch = 500
)

var (
	// ErrReviewNotFound 表示 review_task 不存在。
	ErrReviewNotFound = errors.New("store: review task not found")
	// ErrReviewState 表示任务状态不允许该操作（如给 OPEN 任务打分、改 DONE 任务）。
	ErrReviewState = errors.New("store: review task state does not allow this")
	// ErrBadExpertScore 表示专家分数不在 1-3。
	ErrBadExpertScore = errors.New("store: expert score must be 1-3")
	// ErrSameRater 表示主标与双标派给了同一位专家（双标须独立，两个方向都拦）。
	ErrSameRater = errors.New("store: second rater must differ from the primary rater")
)

// ReviewTask 是一行 review_task。可空列用指针 / 空串（空串写 NULL）。
type ReviewTask struct {
	ID           uint64
	TaskType     string
	QueueReason  string
	ExperimentID *uint64
	SampleID     string
	TrialIndex   *int
	DraftRef     string
	Dimension    string
	JudgeVersion string
	JudgeScore   *int // nil 表示 UNKNOWN（或该任务与 judge 无关）
	RaterNo      int
	ExpertScore  *int
	AssigneeID   *uint64
	Status       string
	RefKey       string
	CreatedAt    time.Time
	DoneAt       *time.Time
}

// Key 是去重键 ref_key = md5(task_type|experiment_id|sample_id|trial_index|dimension|draft_ref|rater_no)（NULL 记空串，
// rater_no 零值按 1）。
func (t ReviewTask) Key() string {
	exp, idx := "", ""
	if t.ExperimentID != nil {
		exp = strconv.FormatUint(*t.ExperimentID, 10)
	}
	if t.TrialIndex != nil {
		idx = strconv.Itoa(*t.TrialIndex)
	}
	raw := strings.Join([]string{t.TaskType, exp, t.SampleID, idx, t.Dimension, t.DraftRef, strconv.Itoa(t.rater())}, "|")
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (t ReviewTask) rater() int {
	if t.RaterNo == 0 {
		return RaterPrimary
	}
	return t.RaterNo
}

// Reviews 读写 agri_eval.review_task。
type Reviews struct {
	db *repository.DB
}

// NewReviews 构造 Reviews。db 须指向 agri_eval 库。
func NewReviews(db *repository.DB) *Reviews { return &Reviews{db: db} }

// Insert 批量插入 OPEN 任务，ON DUPLICATE KEY UPDATE id = id：重跑不重复派单。返回新插入的行数（已存在的行影响数为 0）。
func (r *Reviews) Insert(ctx context.Context, tasks []ReviewTask, at time.Time) (int, error) {
	inserted := 0
	err := r.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		for start := 0; start < len(tasks); start += reviewInsertBatch {
			batch := tasks[start:min(start+reviewInsertBatch, len(tasks))]
			var sb strings.Builder
			sb.WriteString(`INSERT INTO review_task (task_type, queue_reason, experiment_id, sample_id, trial_index, draft_ref,
  dimension, judge_version, judge_score, rater_no, status, ref_key, created_at) VALUES `)
			args := make([]any, 0, len(batch)*13)
			for i, t := range batch {
				if t.TaskType == "" {
					return fmt.Errorf("review task %d: empty task_type", start+i)
				}
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
				args = append(args, t.TaskType, nullStr(t.QueueReason), t.ExperimentID, nullStr(t.SampleID), t.TrialIndex,
					nullStr(t.DraftRef), nullStr(t.Dimension), nullStr(t.JudgeVersion), t.JudgeScore, t.rater(),
					ReviewOpen, t.Key(), at)
			}
			sb.WriteString(" ON DUPLICATE KEY UPDATE id = id")
			res := tx.Exec(sb.String(), args...)
			if res.Error != nil {
				return fmt.Errorf("insert review tasks: %w", res.Error)
			}
			inserted += int(res.RowsAffected)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return inserted, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Assign 把 OPEN / ASSIGNED 任务派给 assignee（改派允许）。同一 trial 同一维度的主标与双标不得是同一专家，
// 两个方向都查（L2 评审 P1-4）：事务内按 id 顺序 FOR UPDATE 锁住这一对的两行，再判状态与对方的专家，并发派单也串行。
func (r *Reviews) Assign(ctx context.Context, id, assignee uint64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		t, err := getReview(tx, id, false)
		if err != nil {
			return err
		}
		primary, second := t, t
		primary.RaterNo, second.RaterNo = RaterPrimary, RaterSecond
		var rows []raterRow
		if err := tx.Raw("SELECT id, status, assignee_id FROM review_task WHERE ref_key IN (?, ?) ORDER BY id FOR UPDATE",
			primary.Key(), second.Key()).Scan(&rows).Error; err != nil {
			return fmt.Errorf("store: lock raters of review task %d: %w", id, err)
		}
		self := -1
		for i, row := range rows {
			if row.ID == id {
				self = i
			}
		}
		if self < 0 {
			return fmt.Errorf("%w: %d", ErrReviewNotFound, id)
		}
		if st := rows[self].Status; st != ReviewOpen && st != ReviewAssigned {
			return fmt.Errorf("%w: assign task %d in %s", ErrReviewState, id, st)
		}
		for i, row := range rows {
			if i != self && row.AssigneeID != nil && *row.AssigneeID == assignee {
				return fmt.Errorf("%w: task %d and %d both assigned to %d", ErrSameRater, id, row.ID, assignee)
			}
		}
		return tx.Exec("UPDATE review_task SET assignee_id = ?, status = ? WHERE id = ?", assignee, ReviewAssigned, id).Error
	})
}

// raterRow 是 Assign 锁住的主标 / 双标行。
type raterRow struct {
	ID         uint64
	Status     string
	AssigneeID *uint64
}

// Score 写专家分数并置 DONE（只允许 ASSIGNED，且须是派给 assignee 的任务）。专家分数即金标准。
func (r *Reviews) Score(ctx context.Context, id, assignee uint64, expert int, at time.Time) error {
	if expert < minExpertScore || expert > maxExpertScore {
		return fmt.Errorf("%w: %d", ErrBadExpertScore, expert)
	}
	res := r.db.WithContext(ctx).Exec(`UPDATE review_task SET expert_score = ?, status = ?, done_at = ?
  WHERE id = ? AND status = ? AND assignee_id = ?`, expert, ReviewDone, at, id, ReviewAssigned, assignee)
	if res.Error != nil {
		return fmt.Errorf("store: score review task %d: %w", id, res.Error)
	}
	if res.RowsAffected == 1 {
		return nil
	}
	t, err := getReview(r.db.WithContext(ctx), id, false)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: score task %d (status %s)", ErrReviewState, id, t.Status)
}

// reviewRow 是 review_task 的扫描行。
type reviewRow struct {
	ID           uint64
	TaskType     string
	QueueReason  *string
	ExperimentID *uint64
	SampleID     *string
	TrialIndex   *int
	DraftRef     *string
	Dimension    *string
	JudgeVersion *string
	JudgeScore   *int
	RaterNo      int
	ExpertScore  *int
	AssigneeID   *uint64
	Status       string
	RefKey       string
	CreatedAt    time.Time
	DoneAt       *time.Time
}

const reviewCols = `id, task_type, queue_reason, experiment_id, sample_id, trial_index, draft_ref, dimension, judge_version,
  judge_score, rater_no, expert_score, assignee_id, status, ref_key, created_at, done_at`

func (w reviewRow) task() ReviewTask {
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return ReviewTask{ID: w.ID, TaskType: w.TaskType, QueueReason: deref(w.QueueReason), ExperimentID: w.ExperimentID,
		SampleID: deref(w.SampleID), TrialIndex: w.TrialIndex, DraftRef: deref(w.DraftRef), Dimension: deref(w.Dimension),
		JudgeVersion: deref(w.JudgeVersion), JudgeScore: w.JudgeScore, RaterNo: w.RaterNo, ExpertScore: w.ExpertScore,
		AssigneeID: w.AssigneeID, Status: w.Status, RefKey: w.RefKey, CreatedAt: w.CreatedAt, DoneAt: w.DoneAt}
}

func getReview(db *repository.DB, id uint64, forUpdate bool) (ReviewTask, error) {
	q := "SELECT " + reviewCols + " FROM review_task WHERE id = ?"
	if forUpdate {
		q += " FOR UPDATE"
	}
	var rows []reviewRow
	if err := db.Raw(q, id).Scan(&rows).Error; err != nil {
		return ReviewTask{}, fmt.Errorf("store: read review task %d: %w", id, err)
	}
	if len(rows) == 0 {
		return ReviewTask{}, fmt.Errorf("%w: %d", ErrReviewNotFound, id)
	}
	return rows[0].task(), nil
}

// GetReview 读一条任务。
func (r *Reviews) GetReview(ctx context.Context, id uint64) (ReviewTask, error) {
	return getReview(r.db.WithContext(ctx), id, false)
}

// ReviewFilter 是任务列表条件；零值字段不过滤。
type ReviewFilter struct {
	TaskType     string
	Dimension    string
	JudgeVersion string
	Status       string
	AssigneeID   *uint64
	Limit        int
}

// List 按条件列任务（id 升序；Limit ≤ 0 不限）。
func (r *Reviews) List(ctx context.Context, f ReviewFilter) ([]ReviewTask, error) {
	var where []string
	var args []any
	for _, c := range []struct {
		col string
		v   string
	}{{"task_type", f.TaskType}, {"dimension", f.Dimension}, {"judge_version", f.JudgeVersion}, {"status", f.Status}} {
		if c.v != "" {
			where = append(where, c.col+" = ?")
			args = append(args, c.v)
		}
	}
	if f.AssigneeID != nil {
		where = append(where, "assignee_id = ?")
		args = append(args, *f.AssigneeID)
	}
	q := "SELECT " + reviewCols + " FROM review_task"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id"
	if f.Limit > 0 {
		q += " LIMIT " + strconv.Itoa(f.Limit)
	}
	var rows []reviewRow
	if err := r.db.WithContext(ctx).Raw(q, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list review tasks: %w", err)
	}
	out := make([]ReviewTask, 0, len(rows))
	for _, w := range rows {
		out = append(out, w.task())
	}
	return out, nil
}

// CountCreated 是 [from, to) 内建的 taskType / dim 任务数（月度复核按月只派一次）。
func (r *Reviews) CountCreated(ctx context.Context, taskType, dim string, from, to time.Time) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Raw(`SELECT COUNT(*) FROM review_task
  WHERE task_type = ? AND dimension = ? AND created_at >= ? AND created_at < ?`, taskType, dim, from, to).Scan(&n).Error
	if err != nil {
		return 0, fmt.Errorf("store: count review tasks: %w", err)
	}
	return n, nil
}
