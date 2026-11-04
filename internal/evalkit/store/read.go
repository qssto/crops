package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/repository"
)

// Record 是一行 experiment（P2 L0 compare / freeze 读取）。Snapshot 为 nil 表示该列为 NULL（P1 及以前的实验）。
type Record struct {
	ID          uint64
	Suite       string
	TriggerKind string
	GitSHA      string
	Status      string
	StartedAt   time.Time
	FinishedAt  *time.Time
	Snapshot    *evalkit.Snapshot
	Frozen      bool
	BaselineID  *uint64
	CompareMode string
}

// experimentRow 是 experiment 的扫描行；可空列用指针。
type experimentRow struct {
	ID          uint64     `gorm:"column:id"`
	Suite       string     `gorm:"column:suite"`
	TriggerKind string     `gorm:"column:trigger_kind"`
	GitSHA      string     `gorm:"column:git_sha"`
	Status      string     `gorm:"column:status"`
	StartedAt   time.Time  `gorm:"column:started_at"`
	FinishedAt  *time.Time `gorm:"column:finished_at"`
	Snapshot    *string    `gorm:"column:snapshot"`
	Frozen      bool       `gorm:"column:frozen"`
	BaselineID  *uint64    `gorm:"column:baseline_id"`
	CompareMode *string    `gorm:"column:compare_mode"`
}

// Done 报告实验是否正常跑完（status = DONE 且有 finished_at）；compare / aa / freeze 只认 DONE。
func (r *Record) Done() bool { return r.Status == experimentDone && r.FinishedAt != nil }

const experimentSelect = `SELECT id, suite, trigger_kind, git_sha, status, started_at, finished_at, snapshot, frozen,
  baseline_id, compare_mode FROM experiment WHERE id = ?`

// Get 读一行 experiment；不存在返回 ErrNotFound。
func (s *Experiments) Get(ctx context.Context, id uint64) (*Record, error) {
	rec, err := getRecord(s.db.WithContext(ctx), id, false)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return rec, nil
}

// getRecord 读 experiment；lock 时 FOR UPDATE（须在事务内）。
func getRecord(db *repository.DB, id uint64, lock bool) (*Record, error) {
	q := experimentSelect
	if lock {
		q += " FOR UPDATE"
	}
	var rows []experimentRow
	if err := db.Raw(q, id).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("read experiment %d: %w", id, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	r := rows[0]
	rec := &Record{ID: r.ID, Suite: r.Suite, TriggerKind: r.TriggerKind, GitSHA: r.GitSHA, Status: r.Status,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Frozen: r.Frozen, BaselineID: r.BaselineID}
	if r.CompareMode != nil {
		rec.CompareMode = *r.CompareMode
	}
	if r.Snapshot != nil {
		var snap evalkit.Snapshot
		if err := json.Unmarshal([]byte(*r.Snapshot), &snap); err != nil {
			return nil, fmt.Errorf("experiment %d snapshot: %w", id, err)
		}
		rec.Snapshot = &snap
	}
	return rec, nil
}

// trialRow 是 trial 的扫描行。
type trialRow struct {
	SampleID      string  `gorm:"column:sample_id"`
	SampleVersion int     `gorm:"column:sample_version"`
	TrialIndex    int     `gorm:"column:trial_index"`
	Suite         string  `gorm:"column:suite"`
	RunID         *uint64 `gorm:"column:run_id"`
	Status        string  `gorm:"column:status"`
	Scores        *string `gorm:"column:scores"`
	LatencyMS     *int64  `gorm:"column:latency_ms"`
}

// infraErrorText 是读回 INFRA_ERROR trial 时填的 Error（库里不存原始错误文本，只保状态）。
const infraErrorText = "infra_error (stored)"

// Trials 读回实验的全部 trial（按 sample_id、trial_index 排序）：scores 还原为 []Score（按维度名排序），
// INFRA_ERROR 的 Error 填 infraErrorText，使 Trial.Status() 与入库状态一致。
func (s *Experiments) Trials(ctx context.Context, id uint64) ([]evalkit.Trial, error) {
	var rows []trialRow
	err := s.db.WithContext(ctx).Raw(`SELECT sample_id, sample_version, trial_index, suite, run_id, status, scores, latency_ms
  FROM trial WHERE experiment_id = ? ORDER BY sample_id, trial_index`, id).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: read trials of %d: %w", id, err)
	}
	out := make([]evalkit.Trial, 0, len(rows))
	for _, r := range rows {
		t, err := r.trial()
		if err != nil {
			return nil, fmt.Errorf("store: trial %s#%d of %d: %w", r.SampleID, r.TrialIndex, id, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func (r trialRow) trial() (evalkit.Trial, error) {
	t := evalkit.Trial{SampleID: r.SampleID, SampleVersion: r.SampleVersion, TrialIndex: r.TrialIndex, Suite: r.Suite,
		Holdout: r.Suite == sample.HoldoutDir}
	if r.RunID != nil {
		t.RunID = *r.RunID
	}
	if r.LatencyMS != nil {
		t.Latency = time.Duration(*r.LatencyMS) * time.Millisecond
	}
	if r.Status == StatusInfraError {
		t.Error = infraErrorText
	}
	if r.Scores == nil {
		return t, nil
	}
	var scores map[string]scoreJSON
	if err := json.Unmarshal([]byte(*r.Scores), &scores); err != nil {
		return t, fmt.Errorf("scores: %w", err)
	}
	for name, sc := range scores {
		t.Scores = append(t.Scores, evalkit.Score{Name: name, Pass: sc.Pass, Value: sc.Value, RedLine: sc.RedLine,
			Unknown: sc.Unknown, Evidence: sc.Evidence, Error: sc.Error, NotApplicable: sc.NotApplicable,
			AdvisoryError: sc.AdvisoryError})
	}
	slices.SortFunc(t.Scores, func(a, b evalkit.Score) int { return strings.Compare(a.Name, b.Name) })
	return t, nil
}

// SampleKey 是配对单位 (sample_id, sample_version)。
type SampleKey struct {
	SampleID      string `gorm:"column:sample_id"`
	SampleVersion int    `gorm:"column:sample_version"`
}

// String 是 sample_id@version。
func (k SampleKey) String() string { return fmt.Sprintf("%s@%d", k.SampleID, k.SampleVersion) }

// pairedSQL 是配对交集（P2 §6.1 ② 照抄），外加排序保证输出稳定。
const pairedSQL = `SELECT b.sample_id, b.sample_version
FROM (SELECT DISTINCT sample_id, sample_version FROM trial WHERE experiment_id = ?) b
JOIN (SELECT DISTINCT sample_id, sample_version FROM trial WHERE experiment_id = ?) c
  USING (sample_id, sample_version)
ORDER BY b.sample_id, b.sample_version`

// PairedSamples 返回 baseline 与 candidate 两个实验 (sample_id, sample_version) 的交集。
func (s *Experiments) PairedSamples(ctx context.Context, baseline, candidate uint64) ([]SampleKey, error) {
	var keys []SampleKey
	if err := s.db.WithContext(ctx).Raw(pairedSQL, baseline, candidate).Scan(&keys).Error; err != nil {
		return nil, fmt.Errorf("store: paired samples %d/%d: %w", baseline, candidate, err)
	}
	return keys, nil
}
