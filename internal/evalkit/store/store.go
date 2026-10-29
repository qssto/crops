// Package store 把评测结果写入 agri_eval.experiment / trial（母文档 §4.6.1）。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/repository"
)

// trial.status 取值（判定见 evalkit.Trial.Status）。
const (
	StatusPass        = evalkit.StatusPass
	StatusFail        = evalkit.StatusFail
	StatusInfraError  = evalkit.StatusInfraError
	StatusGraderError = evalkit.StatusGraderError

	experimentDone = "DONE"
	// TriggerManual 是 make eval / 本地手动触发。
	TriggerManual = "manual"
	gitSHALen     = 40
)

// ErrBadGitSHA 表示 git_sha 不是 40 位。
var ErrBadGitSHA = errors.New("store: git_sha must be 40 hex chars")

// Experiments 读写 agri_eval。db 须指向 agri_eval 库。
type Experiments struct {
	db *repository.DB
}

// New 构造 Experiments。
func New(db *repository.DB) *Experiments { return &Experiments{db: db} }

// Start 插入 RUNNING 状态的 experiment，返回自增 id（同连接取 LastInsertId）。
func (s *Experiments) Start(ctx context.Context, suites []string, trigger, gitSHA string, at time.Time) (uint64, error) {
	if len(gitSHA) != gitSHALen {
		return 0, fmt.Errorf("%w: %q", ErrBadGitSHA, gitSHA)
	}
	var id int64
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		res, err := tx.Statement.ConnPool.ExecContext(ctx,
			"INSERT INTO experiment (suite, trigger_kind, git_sha, status, started_at) VALUES (?, ?, ?, 'RUNNING', ?)",
			strings.Join(suites, ","), trigger, gitSHA, at)
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

// Finish 在一个事务内批量写 trial 并把 experiment 置为 DONE。
func (s *Experiments) Finish(ctx context.Context, id uint64, exp *evalkit.Experiment) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		if len(exp.Trials) > 0 {
			query, args, err := trialInsert(id, exp)
			if err != nil {
				return err
			}
			if err := tx.Exec(query, args...).Error; err != nil {
				return fmt.Errorf("insert trials: %w", err)
			}
		}
		res := tx.Exec("UPDATE experiment SET status = ?, finished_at = ? WHERE id = ?", experimentDone, exp.FinishedAt, id)
		if res.Error != nil {
			return fmt.Errorf("finish experiment: %w", res.Error)
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("finish experiment %d: not found", id)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

const trialCols = "(experiment_id, sample_id, trial_index, suite, run_id, status, passed, scores, latency_ms, created_at)"

// scoreJSON 是 trial.scores 中一个维度的取值；error 非空即 grader_error。
type scoreJSON struct {
	Pass     bool    `json:"pass"`
	Value    float64 `json:"value"`
	RedLine  bool    `json:"red_line,omitempty"`
	Unknown  bool    `json:"unknown,omitempty"`
	Evidence string  `json:"evidence,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// trialInsert 拼批量插入；scores 为维度名 → 结果，passed = status 为 PASS，run_id 为 0 时写 NULL。
func trialInsert(id uint64, exp *evalkit.Experiment) (string, []any, error) {
	rows := make([]string, 0, len(exp.Trials))
	args := make([]any, 0, len(exp.Trials)*10)
	for _, t := range exp.Trials {
		scores := make(map[string]scoreJSON, len(t.Scores))
		for _, sc := range t.Scores {
			scores[sc.Name] = scoreJSON{Pass: sc.Pass, Value: sc.Value, RedLine: sc.RedLine,
				Unknown: sc.Unknown, Evidence: sc.Evidence, Error: sc.Error}
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
		rows = append(rows, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, id, t.SampleID, t.TrialIndex, t.Suite, runID, status, status == StatusPass,
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
	res := s.db.WithContext(ctx).Exec("UPDATE experiment SET report_key = ? WHERE id = ?", key, id)
	if res.Error != nil {
		return fmt.Errorf("store: set report_key: %w", res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("store: set report_key: experiment %d not found", id)
	}
	return nil
}
