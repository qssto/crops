package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"agri-steward/internal/repository"
)

// gate_verdict 取值（与 internal/evalkit/gate 的 Verdict 一致；store 不依赖 gate 包，测试兜底两边一致）。
const (
	VerdictPass            = "PASS"
	VerdictBlockRedLine    = "BLOCK_REDLINE"
	VerdictBlockQuality    = "BLOCK_QUALITY"
	VerdictNeedSignoff     = "NEED_SIGNOFF"
	VerdictNotAttributable = "NOT_ATTRIBUTABLE"
)

// GateVerdicts 是 experiment.gate_verdict 的合法取值（白名单）。
var GateVerdicts = []string{VerdictPass, VerdictBlockRedLine, VerdictBlockQuality, VerdictNeedSignoff, VerdictNotAttributable}

// StatusDone 是正常跑完的实验状态（只有 DONE 的实验给门禁结论）。
const StatusDone = experimentDone

var (
	// ErrBadVerdict 表示 gate_verdict 不在 GateVerdicts 内。
	ErrBadVerdict = errors.New("store: gate_verdict not in PASS|BLOCK_REDLINE|BLOCK_QUALITY|NEED_SIGNOFF|NOT_ATTRIBUTABLE")
	// ErrPhaseCompare 表示实验已登记为阶段级对比（compare_mode = PHASE）：不写 gate_verdict。
	ErrPhaseCompare = errors.New("store: compare_mode=PHASE experiment gets no gate_verdict")
	// ErrNotSignable 表示实验当前 gate_verdict 不是 NEED_SIGNOFF，不能写 signed_by。
	ErrNotSignable = errors.New("store: signed_by only for gate_verdict = NEED_SIGNOFF")
	// ErrBadSigner 表示 signed_by 不是正数。
	ErrBadSigner = errors.New("store: signed_by must be > 0")
)

func validVerdict(v string) bool { return slices.Contains(GateVerdicts, v) }

// GateVerdict 是 experiment 的门禁结论列。
type GateVerdict struct {
	Verdict  string  `gorm:"column:gate_verdict"`
	SignedBy *uint64 `gorm:"column:signed_by"`
}

// recordGateSQL 一次写对比登记与结论，并总是清空 signed_by：每次 gate 运行都重新校验签字（通过后 SetSignedBy 再写），
// 库里的 signed_by 与本次退出码一致（签字文件被撤、信任 ref 变化后重跑不会留下旧签字）。
const recordGateSQL = "baseline_id = ?, compare_mode = ?, factor_diff = ?, signed_by = NULL, gate_verdict = ?"

// RecordGate 在一个事务内（行锁）把单因子对比与门禁结论写入候选实验：要求未冻结、status = DONE（VOID /
// BUDGET_EXCEEDED 等拒绝给结论）、未登记为阶段级对比；verdict 须在白名单内。
func (s *Experiments) RecordGate(ctx context.Context, candidate, baseline uint64, factorDiff json.RawMessage, verdict string) error {
	if !validVerdict(verdict) {
		return fmt.Errorf("%w: %q", ErrBadVerdict, verdict)
	}
	if !json.Valid(factorDiff) {
		return errors.New("store: factor_diff is not valid json")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		rec, err := getRecord(tx, candidate, true)
		if err != nil {
			return err
		}
		switch {
		case rec.Frozen:
			return fmt.Errorf("%w: %d", ErrFrozen, candidate)
		case !rec.Done():
			return fmt.Errorf("%w: %d status=%s, want %s", ErrBadStatus, candidate, rec.Status, experimentDone)
		case rec.CompareMode == ComparePhase:
			return fmt.Errorf("%w: %d", ErrPhaseCompare, candidate)
		}
		return guardedUpdate(tx, candidate, recordGateSQL, baseline, CompareFactor, string(factorDiff), verdict)
	})
	if err != nil {
		return fmt.Errorf("store: record gate %d: %w", candidate, err)
	}
	return nil
}

// SetSignedBy 写 signed_by：要求未冻结、gate_verdict = NEED_SIGNOFF（签字只解除需签字结论，不能覆盖阻断）。
func (s *Experiments) SetSignedBy(ctx context.Context, id, userID uint64) error {
	if userID == 0 {
		return ErrBadSigner
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		if err := lockUnfrozen(tx, id); err != nil {
			return err
		}
		g, err := gateVerdictOf(tx, id)
		if err != nil {
			return err
		}
		if g.Verdict != VerdictNeedSignoff {
			return fmt.Errorf("%w: %d gate_verdict=%q", ErrNotSignable, id, g.Verdict)
		}
		if g.SignedBy != nil && *g.SignedBy == userID {
			return nil
		}
		return guardedUpdate(tx, id, "signed_by = ?", userID)
	})
	if err != nil {
		return fmt.Errorf("store: signed_by %d: %w", id, err)
	}
	return nil
}

// GateVerdictOf 读 gate_verdict / signed_by（未判定时 Verdict 为空）；不存在返回 ErrNotFound。
func (s *Experiments) GateVerdictOf(ctx context.Context, id uint64) (*GateVerdict, error) {
	g, err := gateVerdictOf(s.db.WithContext(ctx), id)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return g, nil
}

func gateVerdictOf(db *repository.DB, id uint64) (*GateVerdict, error) {
	var rows []struct {
		Verdict  *string `gorm:"column:gate_verdict"`
		SignedBy *uint64 `gorm:"column:signed_by"`
	}
	if err := db.Raw("SELECT gate_verdict, signed_by FROM experiment WHERE id = ?", id).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("read gate_verdict %d: %w", id, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	g := &GateVerdict{SignedBy: rows[0].SignedBy}
	if rows[0].Verdict != nil {
		g.Verdict = *rows[0].Verdict
	}
	return g, nil
}
