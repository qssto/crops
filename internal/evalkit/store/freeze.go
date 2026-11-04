package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/repository"
)

var (
	// ErrNotDone 表示实验未跑完（status 不是 DONE），不能冻结。
	ErrNotDone = errors.New("store: experiment is not DONE")
	// ErrSnapshotExists 表示 --backfill 的实验已有快照（补录只用于 snapshot 为 NULL 的 P1 实验）。
	ErrSnapshotExists = errors.New("store: experiment already has a snapshot, backfill refused")
	// ErrNoSnapshot 表示冻结时实验没有快照且未给补录。
	ErrNoSnapshot = errors.New("store: experiment has no snapshot (P1 experiment needs --backfill)")
	// ErrBackfillMismatch 表示补录内容与库中实验对不上（git_sha / 样本）。
	ErrBackfillMismatch = errors.New("store: backfill does not match experiment")
	// ErrNoTrials 表示实验没有任何 trial（空实验不能当基线）。
	ErrNoTrials = errors.New("store: experiment has no trials")
	// ErrFrozenMismatch 表示实验已冻结，但库中快照与这次要登记的快照不一致（不能只补写指针）。
	ErrFrozenMismatch = errors.New("store: frozen experiment snapshot differs, pointer-only refused")
	// ErrBadCompareMode 表示 compare_mode 不是 FACTOR / PHASE。
	ErrBadCompareMode = errors.New("store: compare_mode must be FACTOR or PHASE")
)

// experiment.compare_mode 取值。
const (
	CompareFactor = "FACTOR"
	ComparePhase  = "PHASE"
)

// Backfill 是冻结 P1 实验时的补录：完整快照（WallTime 为空时按 started_at / finished_at 计算）与 sample_id → version。
// P1 的 trial 没有 sample_version 列（迁移后默认 0），不补则与 P2 实验的配对交集为空。
type Backfill struct {
	Snapshot       evalkit.Snapshot
	SampleVersions map[string]int
}

// Freeze 在一个事务内（行锁）校验并冻结实验：bf 非 nil 时先补录快照与 trial.sample_version，再置 frozen = 1。
// 冻结后 experiment 的任何 UPDATE 都返回 ErrFrozen；返回冻结后的记录。
func (s *Experiments) Freeze(ctx context.Context, id uint64, bf *Backfill) (*Record, error) {
	var out *Record
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		rec, err := getRecord(tx, id, true)
		if err != nil {
			return err
		}
		if rec.Frozen {
			return fmt.Errorf("%w: %d", ErrFrozen, id)
		}
		if err := requireDoneWithTrials(tx, rec); err != nil {
			return err
		}
		if bf != nil {
			if rec.Snapshot, err = backfill(tx, rec, bf); err != nil {
				return err
			}
		}
		if rec.Snapshot == nil {
			return fmt.Errorf("%w: %d", ErrNoSnapshot, id)
		}
		if missing := rec.Snapshot.Missing(); len(missing) > 0 {
			return fmt.Errorf("%w: experiment %d: %s", ErrEmptySnapshot, id, strings.Join(missing, ","))
		}
		res := tx.Exec("UPDATE experiment SET frozen = 1 WHERE id = ? AND frozen = 0", id)
		if res.Error != nil {
			return fmt.Errorf("freeze: %w", res.Error)
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("%w: %d", ErrFrozen, id)
		}
		rec.Frozen = true
		out = rec
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: freeze %d: %w", id, err)
	}
	return out, nil
}

// requireDoneWithTrials 要求实验 DONE 且至少有一条 trial（失败关闭：空实验或未跑完的实验不能冻结为基线）。
func requireDoneWithTrials(tx *repository.DB, rec *Record) error {
	if !rec.Done() {
		return fmt.Errorf("%w: %d status=%s", ErrNotDone, rec.ID, rec.Status)
	}
	var n int64
	if err := tx.Raw("SELECT COUNT(*) FROM trial WHERE experiment_id = ?", rec.ID).Scan(&n).Error; err != nil {
		return fmt.Errorf("count trials of %d: %w", rec.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %d", ErrNoTrials, rec.ID)
	}
	return nil
}

// MatchFrozen 用于「冻结已提交、指针文件没写成」后的重跑：要求实验已冻结、DONE、有 trial，且库中快照与这次要登记的
// 快照一致（bf 非 nil 时按补录口径算出预期快照；bf 为 nil 时库中快照须完整）。一致返回该记录，供只补写指针（pointer-only）。
func (s *Experiments) MatchFrozen(ctx context.Context, id uint64, bf *Backfill) (*Record, error) {
	db := s.db.WithContext(ctx)
	rec, err := getRecord(db, id, false)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if !rec.Frozen {
		return nil, fmt.Errorf("store: %d is not frozen: %w", id, ErrFrozenMismatch)
	}
	if err := requireDoneWithTrials(db, rec); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if rec.Snapshot == nil || len(rec.Snapshot.Missing()) > 0 {
		return nil, fmt.Errorf("store: %w: experiment %d", ErrEmptySnapshot, id)
	}
	if bf == nil {
		return rec, nil
	}
	want := expectedBackfill(rec, bf)
	got, err := json.Marshal(rec.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("store: snapshot of %d: %w", id, err)
	}
	exp, err := json.Marshal(want)
	if err != nil {
		return nil, fmt.Errorf("store: backfill snapshot: %w", err)
	}
	if string(got) != string(exp) {
		return nil, fmt.Errorf("%w: %d stored %s, backfill %s", ErrFrozenMismatch, id, got, exp)
	}
	return rec, nil
}

// expectedBackfill 是补录写入的快照：wall_time 为空时按 started_at / finished_at 计算。
func expectedBackfill(rec *Record, bf *Backfill) evalkit.Snapshot {
	snap := bf.Snapshot
	if snap.WallTime == "" && rec.FinishedAt != nil {
		snap.WallTime = WallTime(rec.StartedAt, *rec.FinishedAt)
	}
	return snap
}

// backfill 写快照与 trial.sample_version；要求 snapshot 原为 NULL、git_sha 与库一致、每个 trial 样本都有版本。
func backfill(tx *repository.DB, rec *Record, bf *Backfill) (*evalkit.Snapshot, error) {
	if rec.Snapshot != nil {
		return nil, fmt.Errorf("%w: %d", ErrSnapshotExists, rec.ID)
	}
	snap := expectedBackfill(rec, bf)
	if snap.GitSHA != rec.GitSHA {
		return nil, fmt.Errorf("%w: git_sha %s != experiment %s", ErrBackfillMismatch, snap.GitSHA, rec.GitSHA)
	}
	if missing := snap.Missing(); len(missing) > 0 {
		return nil, fmt.Errorf("%w: backfill: %s", ErrEmptySnapshot, strings.Join(missing, ","))
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("backfill snapshot: %w", err)
	}
	if err := guardedUpdate(tx, rec.ID, "snapshot = ?", string(b)); err != nil {
		return nil, err
	}
	if err := backfillVersions(tx, rec.ID, bf.SampleVersions); err != nil {
		return nil, err
	}
	return &snap, nil
}

// backfillVersions 把仍为 0 的 trial.sample_version 按样本补齐；任一 trial 样本缺版本即拒绝（整笔回滚）。
func backfillVersions(tx *repository.DB, id uint64, versions map[string]int) error {
	var ids []string
	if err := tx.Raw("SELECT DISTINCT sample_id FROM trial WHERE experiment_id = ? AND sample_version = 0 ORDER BY sample_id",
		id).Scan(&ids).Error; err != nil {
		return fmt.Errorf("read trial samples: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	var unknown []string
	for _, sid := range ids {
		if versions[sid] < 1 {
			unknown = append(unknown, sid)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("%w: no version for trial samples %s", ErrBackfillMismatch, strings.Join(unknown, ","))
	}
	query, args := versionUpdate(id, ids, versions)
	if err := tx.Exec(query, args...).Error; err != nil {
		return fmt.Errorf("backfill sample_version: %w", err)
	}
	return nil
}

// versionUpdate 拼 UPDATE trial SET sample_version = CASE sample_id WHEN ? THEN ? … END（ids 已排序）。
func versionUpdate(id uint64, ids []string, versions map[string]int) (string, []any) {
	var b strings.Builder
	args := make([]any, 0, 2*len(ids)+1)
	b.WriteString("UPDATE trial SET sample_version = CASE sample_id")
	for _, sid := range ids {
		b.WriteString(" WHEN ? THEN ?")
		args = append(args, sid, versions[sid])
	}
	b.WriteString(" END WHERE experiment_id = ? AND sample_version = 0")
	return b.String(), append(args, id)
}

// RecordCompare 把单因子对比结果写入候选实验：baseline_id、compare_mode、factor_diff（[{name, baseline, candidate}]）。
// 阶段级对比不调用本方法（只出报告）。候选已冻结时返回 ErrFrozen。
func (s *Experiments) RecordCompare(ctx context.Context, candidate, baseline uint64, mode string, factorDiff json.RawMessage) error {
	if mode != CompareFactor && mode != ComparePhase {
		return fmt.Errorf("%w: %q", ErrBadCompareMode, mode)
	}
	if !json.Valid(factorDiff) {
		return errors.New("store: factor_diff is not valid json")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		return guardedUpdate(tx, candidate, "baseline_id = ?, compare_mode = ?, factor_diff = ?", baseline, mode, string(factorDiff))
	})
	if err != nil {
		return fmt.Errorf("store: record compare %d: %w", candidate, err)
	}
	return nil
}
