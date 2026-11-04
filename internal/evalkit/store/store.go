// Package store 把评测结果写入 agri_eval.experiment / trial（母文档 §4.6.1）。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/repository"
)

// trial.status 取值（判定见 evalkit.Trial.Status）。
const (
	StatusPass        = evalkit.StatusPass
	StatusFail        = evalkit.StatusFail
	StatusInfraError  = evalkit.StatusInfraError
	StatusGraderError = evalkit.StatusGraderError

	experimentRunning = "RUNNING"
	experimentDone    = "DONE"
	// experimentFailed 是建行后运行中途失败（样本清单写入、回放或落库出错）的实验状态；compare / freeze 只认 DONE。
	experimentFailed = "FAILED"
	gitSHALen        = 40
)

// experiment.trigger_kind 取值（P2 L0：cmd/eval run --trigger）。
const (
	// TriggerManual 是 make eval / 本地手动触发（默认）。
	TriggerManual  = "manual"
	TriggerPR      = "pr"
	TriggerNightly = "nightly"
	TriggerRelease = "release"
)

// Triggers 是合法的 trigger_kind。
var Triggers = []string{TriggerManual, TriggerPR, TriggerNightly, TriggerRelease}

var (
	// ErrBadGitSHA 表示 git_sha 不是 40 位。
	ErrBadGitSHA = errors.New("store: git_sha must be 40 hex chars")
	// ErrEmptySnapshot 表示配置快照缺字段（P2 起新实验必须带完整快照）。
	ErrEmptySnapshot = errors.New("store: snapshot has empty fields")
	// ErrBadTrigger 表示 trigger_kind 不在 Triggers 内。
	ErrBadTrigger = errors.New("store: trigger must be manual|pr|nightly|release")
	// ErrFrozen 表示实验已冻结（frozen = 1），任何修改都被拒绝；要改基线须另跑实验并冻结为新名字（如 .r2）。
	ErrFrozen = errors.New("store: experiment is frozen")
	// ErrNotFound 表示 experiment 不存在。
	ErrNotFound = errors.New("store: experiment not found")
	// ErrNotRunning 表示实验已不在 RUNNING（Fail 只用于中途失败的实验）。
	ErrNotRunning = errors.New("store: experiment is not RUNNING")
)

// Experiments 读写 agri_eval。db 须指向 agri_eval 库。
type Experiments struct {
	db *repository.DB
}

// New 构造 Experiments。
func New(db *repository.DB) *Experiments { return &Experiments{db: db} }

// ValidTrigger 报告 trigger 是否为合法 trigger_kind。
func ValidTrigger(trigger string) bool { return slices.Contains(Triggers, trigger) }

// Start 插入 RUNNING 状态的 experiment（suite 取 snap.Suites 逗号拼接，snapshot 写配置快照），返回自增 id（同连接取 LastInsertId）。
func (s *Experiments) Start(ctx context.Context, trigger string, snap evalkit.Snapshot, at time.Time) (uint64, error) {
	if !ValidTrigger(trigger) {
		return 0, fmt.Errorf("%w: %q", ErrBadTrigger, trigger)
	}
	if missing := snap.MissingConfig(); len(missing) > 0 {
		return 0, fmt.Errorf("%w: %s", ErrEmptySnapshot, strings.Join(missing, ","))
	}
	if len(snap.GitSHA) != gitSHALen {
		return 0, fmt.Errorf("%w: %q", ErrBadGitSHA, snap.GitSHA)
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("store: snapshot: %w", err)
	}
	var id int64
	err = s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		res, err := tx.Statement.ConnPool.ExecContext(ctx,
			"INSERT INTO experiment (suite, trigger_kind, git_sha, status, started_at, snapshot) VALUES (?, ?, ?, ?, ?, ?)",
			strings.Join(snap.Suites, ","), trigger, snap.GitSHA, experimentRunning, at, string(b))
		if err != nil {
			return fmt.Errorf("insert experiment: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("experiment id: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return uint64(id), nil
}

// Finish 在一个事务内批量写 trial、回填快照的 wall_time（token_used / cost 由调用方填入 exp.Snapshot）并把 experiment 置为 DONE。
func (s *Experiments) Finish(ctx context.Context, id uint64, exp *evalkit.Experiment) error {
	snap, err := finishSnapshot(exp)
	if err != nil {
		return err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error { return finishTx(tx, id, exp, snap) })
	if err != nil {
		return fmt.Errorf("store: finish %d: %w", id, err)
	}
	return nil
}

// finishSnapshot 回填 wall_time、校验快照完整并序列化（Finish / FinishSettled 共用，先于开事务）。
func finishSnapshot(exp *evalkit.Experiment) (string, error) {
	exp.Snapshot.WallTime = WallTime(exp.StartedAt, exp.FinishedAt)
	if missing := exp.Snapshot.Missing(); len(missing) > 0 {
		return "", fmt.Errorf("%w: %s", ErrEmptySnapshot, strings.Join(missing, ","))
	}
	b, err := json.Marshal(exp.Snapshot)
	if err != nil {
		return "", fmt.Errorf("store: snapshot: %w", err)
	}
	return string(b), nil
}

// finishTx 是 Finish 的事务体：写 trial 并置 DONE（FinishSettled 与作废判定同事务复用）。
func finishTx(tx *repository.DB, id uint64, exp *evalkit.Experiment, snap string) error {
	if err := lockUnfrozen(tx, id); err != nil {
		return err
	}
	if len(exp.Trials) > 0 {
		query, args, err := trialInsert(id, exp)
		if err != nil {
			return err
		}
		if err := tx.Exec(query, args...).Error; err != nil {
			return fmt.Errorf("insert trials: %w", err)
		}
	}
	return guardedUpdate(tx, id, "status = ?, finished_at = ?, snapshot = ?", experimentDone, exp.FinishedAt, snap)
}

// Fail 把仍在 RUNNING 的实验置 FAILED 并记 finished_at（走带冻结守卫的 UPDATE）；已结束或已冻结返回错误。
func (s *Experiments) Fail(ctx context.Context, id uint64, at time.Time) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		rec, err := getRecord(tx, id, true)
		if err != nil {
			return err
		}
		if rec.Status != experimentRunning {
			return fmt.Errorf("%w: %d status=%s", ErrNotRunning, id, rec.Status)
		}
		return guardedUpdate(tx, id, "status = ?, finished_at = ?", experimentFailed, at)
	})
	if err != nil {
		return fmt.Errorf("store: fail %d: %w", id, err)
	}
	return nil
}

// WallTime 是快照 wall_time 的文本：墙钟时长取整到秒。
func WallTime(started, finished time.Time) string {
	return finished.Sub(started).Round(time.Second).String()
}

// guardedUpdateSQL 是 experiment 的唯一 UPDATE 形状：一律带 AND frozen = 0（P2 L0，冻结后只读）。
func guardedUpdateSQL(set string) string {
	return "UPDATE experiment SET " + set + " WHERE id = ? AND frozen = 0"
}

// guardedUpdate 执行带冻结守卫的 UPDATE；0 行时区分「已冻结」「不存在」与「值未变」（驱动按 changed rows 计数）。
func guardedUpdate(tx *repository.DB, id uint64, set string, args ...any) error {
	res := tx.Exec(guardedUpdateSQL(set), append(args, id)...)
	if res.Error != nil {
		return fmt.Errorf("update experiment: %w", res.Error)
	}
	if res.RowsAffected == 1 {
		return nil
	}
	return frozenState(tx, id, false)
}

// lockUnfrozen 在事务内对 experiment 行加锁并要求未冻结。
func lockUnfrozen(tx *repository.DB, id uint64) error { return frozenState(tx, id, true) }

// frozenState 读 experiment.frozen：行不存在返回 ErrNotFound，已冻结返回 ErrFrozen；lock 时 FOR UPDATE。
func frozenState(tx *repository.DB, id uint64, lock bool) error {
	q := "SELECT frozen FROM experiment WHERE id = ?"
	if lock {
		q += " FOR UPDATE"
	}
	var frozen []bool
	if err := tx.Raw(q, id).Scan(&frozen).Error; err != nil {
		return fmt.Errorf("read experiment.frozen: %w", err)
	}
	switch {
	case len(frozen) == 0:
		return fmt.Errorf("%w: %d", ErrNotFound, id)
	case frozen[0]:
		return fmt.Errorf("%w: %d", ErrFrozen, id)
	}
	return nil
}

const trialCols = "(experiment_id, sample_id, sample_version, trial_index, suite, run_id, status, passed, scores, latency_ms, created_at)"

// trialColCount 是 trialCols 的列数。
const trialColCount = 11

// scoreJSON 是 trial.scores 中一个维度的取值；error 非空即 grader_error。
type scoreJSON struct {
	Pass     bool    `json:"pass"`
	Value    float64 `json:"value"`
	RedLine  bool    `json:"red_line,omitempty"`
	Unknown  bool    `json:"unknown,omitempty"`
	Evidence string  `json:"evidence,omitempty"`
	Error    string  `json:"error,omitempty"`
	// NotApplicable 是样本未声明该维度期望时的放行（P1-L8-A 审查修复）。
	NotApplicable bool `json:"not_applicable,omitempty"`
	// AdvisoryError 是门禁未开的 judge 出错（不改 trial 状态，L2 评审裁决）。
	AdvisoryError string `json:"advisory_error,omitempty"`
}

// trialInsert 拼批量插入；scores 为维度名 → 结果，passed = status 为 PASS，run_id 为 0 时写 NULL。
func trialInsert(id uint64, exp *evalkit.Experiment) (string, []any, error) {
	rows := make([]string, 0, len(exp.Trials))
	args := make([]any, 0, len(exp.Trials)*trialColCount)
	for _, t := range exp.Trials {
		scores := make(map[string]scoreJSON, len(t.Scores))
		for _, sc := range t.Scores {
			scores[sc.Name] = scoreJSON{Pass: sc.Pass, Value: sc.Value, RedLine: sc.RedLine, Unknown: sc.Unknown,
				Evidence: sc.Evidence, Error: sc.Error, NotApplicable: sc.NotApplicable, AdvisoryError: sc.AdvisoryError}
		}
		b, err := json.Marshal(scores)
		if err != nil {
			return "", nil, fmt.Errorf("trial scores: %w", err)
		}
		var runID any
		if t.RunID != 0 {
			runID = t.RunID
		}
		status := t.Status()
		suite := t.Suite
		if t.IsHoldout() {
			suite = sample.HoldoutDir // 路径标为准（L2 评审 P1-2）：读回按 suite 列判定 holdout
		}
		rows = append(rows, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, id, t.SampleID, t.SampleVersion, t.TrialIndex, suite, runID, status, status == StatusPass,
			string(b), t.Latency.Milliseconds(), exp.FinishedAt)
	}
	return "INSERT INTO trial " + trialCols + " VALUES " + strings.Join(rows, ", "), args, nil
}

// FormatID 把 experiment id 转为报告中的 exp_id。
func FormatID(id uint64) string { return strconv.FormatUint(id, 10) }

// reportKeyMax 是 experiment.report_key 的列宽 varchar(255)。
const reportKeyMax = 255

// ErrBadReportKey 表示 report_key 为空或超出列宽。
var ErrBadReportKey = errors.New("store: report_key must be 1..255 chars")

// SetReportKey 记下报告在 agri-steward-eval 内的对象键（L8 baseline 上传后写入）。
func (s *Experiments) SetReportKey(ctx context.Context, id uint64, key string) error {
	if key == "" || len(key) > reportKeyMax {
		return fmt.Errorf("%w: %q", ErrBadReportKey, key)
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		return guardedUpdate(tx, id, "report_key = ?", key)
	})
	if err != nil {
		return fmt.Errorf("store: set report_key: %w", err)
	}
	return nil
}
