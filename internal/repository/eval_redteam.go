package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// EvalRedteamRepo 读评测库 agri_eval 的 experiment / trial（P1 §6.9 技术实现第 1 条 gray-preflight，只读）。
// 评测库没有 tenant / farm 维度；SQL 文档未给，自写，表结构见 migrations/eval/0001_init.up.sql（不改）。
type EvalRedteamRepo struct{ DB *gorm.DB }

// EvalExperiment 是一次评测实验。
type EvalExperiment struct {
	ID          uint64
	Suite       string // 逗号分隔的套件名
	TriggerKind string
	GitSHA      string
	Status      string
	StartedAt   time.Time
	FinishedAt  *time.Time
}

// EvalTrial 是实验中一个 trial 的判定结果；Scores 为维度名 → {pass, red_line, error, …}。
type EvalTrial struct {
	SampleID   string
	TrialIndex int
	Status     string
	Scores     json.RawMessage
}

// LatestExperimentWithSuite 返回 suite 列含 suite 的最近一次实验（按 id 倒序，任意状态）；没有时 found=false。
func (r EvalRedteamRepo) LatestExperimentWithSuite(ctx context.Context, suite string) (EvalExperiment, bool, error) {
	var rows []EvalExperiment
	err := r.DB.WithContext(ctx).Raw(
		`SELECT id, suite, trigger_kind, git_sha, status, started_at, finished_at FROM experiment
 WHERE FIND_IN_SET(?, suite) > 0 ORDER BY id DESC LIMIT 1`, suite).Scan(&rows).Error
	if err != nil {
		return EvalExperiment{}, false, fmt.Errorf("repository: latest experiment with suite %s: %w", suite, err)
	}
	if len(rows) == 0 {
		return EvalExperiment{}, false, nil
	}
	return rows[0], true, nil
}

// SuiteTrials 返回实验中某套件的全部 trial（uk_trial 前缀 experiment_id）。
func (r EvalRedteamRepo) SuiteTrials(ctx context.Context, experimentID uint64, suite string) ([]EvalTrial, error) {
	var rows []struct {
		SampleID   string
		TrialIndex int
		Status     string
		Scores     []byte
	}
	err := r.DB.WithContext(ctx).Raw(
		`SELECT sample_id, trial_index, status, scores FROM trial WHERE experiment_id=? AND suite=?
 ORDER BY sample_id, trial_index`, experimentID, suite).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repository: trials of experiment %d suite %s: %w", experimentID, suite, err)
	}
	out := make([]EvalTrial, 0, len(rows))
	for _, row := range rows {
		out = append(out, EvalTrial{SampleID: row.SampleID, TrialIndex: row.TrialIndex, Status: row.Status,
			Scores: rawJSON(row.Scores)})
	}
	return out, nil
}
