package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/repository"
)

// ResumableStatuses 是 resume 接受的实验状态：已结束（含作废 / 超预算 / 中途失败）且未冻结；RUNNING 可能仍在跑，拒绝。
var ResumableStatuses = []string{experimentDone, StatusVoid, StatusBudgetExceeded, experimentFailed}

// ErrNotResumable 表示实验状态不允许 resume。
var ErrNotResumable = errors.New("store: experiment cannot be resumed")

// RequireResumable 要求 rec 未冻结且状态在 ResumableStatuses 内。
func RequireResumable(rec *Record) error {
	switch {
	case rec.Frozen:
		return fmt.Errorf("%w: %d", ErrFrozen, rec.ID)
	case !slices.Contains(ResumableStatuses, rec.Status):
		return fmt.Errorf("%w: %d status=%s (want %s)", ErrNotResumable, rec.ID, rec.Status, strings.Join(ResumableStatuses, "|"))
	}
	return nil
}

// ErrManifest 表示样本清单读不到、解析失败或与实验不符。
var ErrManifest = errors.New("store: bad sample manifest")

// GetManifest 读实验 {id}/manifest.json（resume 按它恢复当初的样本集）。
func GetManifest(ctx context.Context, obj repository.ObjectGetter, bucket string, id uint64) (*Manifest, error) {
	key := ManifestKey(id)
	raw, err := obj.GetObject(ctx, bucket, key)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s/%s: %w", ErrManifest, bucket, key, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%w: parse %s/%s: %w", ErrManifest, bucket, key, err)
	}
	if m.ExperimentID != FormatID(id) || len(m.Samples) == 0 {
		return nil, fmt.Errorf("%w: %s/%s experiment_id=%q samples=%d", ErrManifest, bucket, key, m.ExperimentID, len(m.Samples))
	}
	return &m, nil
}

// Resume 在一个事务内把 resume 补跑的 trial 写回同一实验并定终态（仿 FinishSettled）：只删 exp.Trials 补跑覆盖的
// INFRA_ERROR 行（超预算中途停止时未补到的保留），插入 exp.Trials，回写快照与 finished_at 并置 DONE，再 settleTx
// 按 budgetExceeded 与 maxInfraRate（快照记录的阈值）重判作废 / 预算。任何一步失败（含 uk_trial 冲突、阈值不合法、
// 判定失败）整体回滚，实验保持原状态与原 trial。
func (s *Experiments) Resume(ctx context.Context, id uint64, exp *evalkit.Experiment, budgetExceeded bool,
	maxInfraRate float64) (Settlement, error) {
	var out Settlement
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		rec, err := getRecord(tx, id, true)
		if err != nil {
			return err
		}
		if err := RequireResumable(rec); err != nil {
			return err
		}
		exp.Snapshot.WallTime = WallTime(rec.StartedAt, exp.FinishedAt)
		if missing := exp.Snapshot.Missing(); len(missing) > 0 {
			return fmt.Errorf("%w: %s", ErrEmptySnapshot, strings.Join(missing, ","))
		}
		snap, err := json.Marshal(exp.Snapshot)
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		if len(exp.Trials) > 0 {
			query, args := rerunDelete(id, exp)
			if err := tx.Exec(query, args...).Error; err != nil {
				return fmt.Errorf("delete rerun infra_error trials: %w", err)
			}
			query, args, err = trialInsert(id, exp)
			if err != nil {
				return err
			}
			if err := tx.Exec(query, args...).Error; err != nil {
				return fmt.Errorf("insert trials: %w", err)
			}
		}
		if err := guardedUpdate(tx, id, "status = ?, finished_at = ?, snapshot = ?",
			experimentDone, exp.FinishedAt, string(snap)); err != nil {
			return err
		}
		out, err = settleTx(tx, id, budgetExceeded, maxInfraRate)
		return err
	})
	if err != nil {
		return Settlement{}, fmt.Errorf("store: resume %d: %w", id, err)
	}
	return out, nil
}

// rerunDelete 是删除 exp.Trials 补跑覆盖的 INFRA_ERROR 行的 SQL（按 (sample_id, trial_index) 精确匹配）；
// exp.Trials 须非空。
func rerunDelete(id uint64, exp *evalkit.Experiment) (string, []any) {
	keys := make([]string, 0, len(exp.Trials))
	args := make([]any, 0, 2+2*len(exp.Trials))
	args = append(args, id, StatusInfraError)
	for _, t := range exp.Trials {
		keys = append(keys, "(?, ?)")
		args = append(args, t.SampleID, t.TrialIndex)
	}
	return "DELETE FROM trial WHERE experiment_id = ? AND status = ? AND (sample_id, trial_index) IN (" +
		strings.Join(keys, ", ") + ")", args
}
