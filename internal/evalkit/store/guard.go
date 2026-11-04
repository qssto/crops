package store

import (
	"context"
	"errors"
	"fmt"

	"agri-steward/internal/config"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/repository"
)

// L7-a 门禁状态（P2 §6.8 技术实现 2、3）：两者都不是 DONE，compare / freeze / aa 一律拒绝（Record.Done）。
const (
	// StatusBudgetExceeded 是累计 token 超过 token_budget、停止派发新 trial 的实验状态。
	StatusBudgetExceeded = "BUDGET_EXCEEDED"
	// StatusVoid 是命中作废规则（VoidRules）的实验状态，原因写 void_reason。
	StatusVoid = "VOID"
)

// void_reason 取值。
const (
	VoidInfraErrorRate = "INFRA_ERROR_RATE"
	VoidGraderError    = "GRADER_ERROR"
)

// TrialCounts 是一个实验的 trial 状态计数（Settle 按 SQL 统计）。
type TrialCounts struct {
	Total       int64 `gorm:"column:total"`
	InfraError  int64 `gorm:"column:infra_error"`
	GraderError int64 `gorm:"column:grader_error"`
}

// InfraErrorRate 是 infra_error 占比；没有 trial 时为 0。
func (c TrialCounts) InfraErrorRate() float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.InfraError) / float64(c.Total)
}

// VoidRule 是一条作废规则：Hit 命中即作废，void_reason = Reason。
type VoidRule struct {
	Reason string
	Hit    func(c TrialCounts, maxInfraRate float64) bool
}

// VoidRules 是作废规则注册表，按序判定，第一条命中的写 void_reason；新增规则只追加此表。
var VoidRules = []VoidRule{
	{Reason: VoidInfraErrorRate, Hit: func(c TrialCounts, max float64) bool { return c.InfraErrorRate() > max }},
	{Reason: VoidGraderError, Hit: func(c TrialCounts, _ float64) bool { return c.GraderError > 0 }},
}

// VoidReasons 返回命中的全部规则原因（按注册顺序）；空表示不作废。
func VoidReasons(c TrialCounts, maxInfraRate float64) []string {
	var out []string
	for _, r := range VoidRules {
		if r.Hit(c, maxInfraRate) {
			out = append(out, r.Reason)
		}
	}
	return out
}

// ErrBadVoidRate 表示作废阈值不在 (0, config.MaxEvalVoidInfraErrorRate] 内（含快照未记录阈值的 0）。
var ErrBadVoidRate = errors.New("store: void_infra_error_rate out of (0, 0.05]")

// CheckVoidRate 要求作废阈值在 (0, 5%] 内：settleTx 判定前调用，阈值缺失或被改松一律失败关闭。
func CheckVoidRate(rate float64) error {
	if !config.ValidVoidInfraErrorRate(rate) {
		return fmt.Errorf("%w: got %v", ErrBadVoidRate, rate)
	}
	return nil
}

// ErrBadBudget 表示 token_budget 不是正数。
var ErrBadBudget = errors.New("store: token_budget must be > 0")

// SetTokenBudget 给 RUNNING 实验写 token_budget（不调用即 NULL = 不限）。
func (s *Experiments) SetTokenBudget(ctx context.Context, id uint64, budget int64) error {
	if budget <= 0 {
		return fmt.Errorf("%w: %d", ErrBadBudget, budget)
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		if err := requireStatus(tx, id, experimentRunning); err != nil {
			return err
		}
		return guardedUpdate(tx, id, "token_budget = ?", budget)
	})
	if err != nil {
		return fmt.Errorf("store: token_budget %d: %w", id, err)
	}
	return nil
}

// Settlement 是 Settle 的判定结果。
type Settlement struct {
	Status string
	// VoidReason 是写进 void_reason 的原因（第一条命中的规则）；Hits 是全部命中的规则。
	VoidReason string
	Hits       []string
	Counts     TrialCounts
}

// trialCountsSQL 统计 trial 状态（P2 §6.8 技术实现 3：SUM(status = 'INFRA_ERROR') / COUNT(*)）。
const trialCountsSQL = `SELECT COUNT(*) AS total, COALESCE(SUM(status = 'INFRA_ERROR'), 0) AS infra_error,
  COALESCE(SUM(status = 'GRADER_ERROR'), 0) AS grader_error FROM trial WHERE experiment_id = ?`

// Settle 对已写完 trial（status = DONE）的实验按门禁规则定终态（resume 补跑后重判用）；见 settleTx。
func (s *Experiments) Settle(ctx context.Context, id uint64, budgetExceeded bool, maxInfraRate float64) (Settlement, error) {
	var out Settlement
	err := s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		if err := requireStatus(tx, id, experimentDone); err != nil {
			return err
		}
		var err error
		out, err = settleTx(tx, id, budgetExceeded, maxInfraRate)
		return err
	})
	if err != nil {
		return Settlement{}, fmt.Errorf("store: settle %d: %w", id, err)
	}
	return out, nil
}

// FinishSettled 在同一事务内 Finish（写 trial、置 DONE）并 settleTx 定终态：作废判定失败则整体回滚，
// 不会留下未判定的 DONE（失败关闭；调用方随后把 RUNNING 实验置 FAILED）。
func (s *Experiments) FinishSettled(ctx context.Context, id uint64, exp *evalkit.Experiment, budgetExceeded bool,
	maxInfraRate float64) (Settlement, error) {
	snap, err := finishSnapshot(exp)
	if err != nil {
		return Settlement{}, err
	}
	var out Settlement
	err = s.db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		if err := finishTx(tx, id, exp, snap); err != nil {
			return err
		}
		var err error
		out, err = settleTx(tx, id, budgetExceeded, maxInfraRate)
		return err
	})
	if err != nil {
		return Settlement{}, fmt.Errorf("store: finish %d: %w", id, err)
	}
	return out, nil
}

// settleTx 按门禁规则定终态：budgetExceeded 时置 BUDGET_EXCEEDED；否则按 SQL 统计 trial，命中 VoidRules 置 VOID
// 并写 void_reason；都不命中保持 DONE 并清空 void_reason（resume 重判后恢复 DONE）。Hits / Counts 总是填写。
// 阈值不合法（CheckVoidRate）时报错，调用方事务整体回滚。
func settleTx(tx *repository.DB, id uint64, budgetExceeded bool, maxInfraRate float64) (Settlement, error) {
	var out Settlement
	if err := CheckVoidRate(maxInfraRate); err != nil {
		return out, err
	}
	if err := tx.Raw(trialCountsSQL, id).Scan(&out.Counts).Error; err != nil {
		return out, fmt.Errorf("count trials: %w", err)
	}
	out.Hits = VoidReasons(out.Counts, maxInfraRate)
	switch {
	case budgetExceeded:
		out.Status = StatusBudgetExceeded
		return out, guardedUpdate(tx, id, "status = ?, void_reason = NULL", StatusBudgetExceeded)
	case len(out.Hits) > 0:
		out.Status, out.VoidReason = StatusVoid, out.Hits[0]
		return out, guardedUpdate(tx, id, "status = ?, void_reason = ?", StatusVoid, out.VoidReason)
	}
	out.Status = experimentDone
	return out, guardedUpdate(tx, id, "void_reason = NULL")
}

// ErrBadStatus 表示实验状态不是本操作要求的状态。
var ErrBadStatus = errors.New("store: unexpected experiment status")

// requireStatus 在事务内锁行并要求未冻结、status = want。
func requireStatus(tx *repository.DB, id uint64, want string) error {
	rec, err := getRecord(tx, id, true)
	if err != nil {
		return err
	}
	switch {
	case rec.Frozen:
		return fmt.Errorf("%w: %d", ErrFrozen, id)
	case rec.Status != want:
		return fmt.Errorf("%w: %d status=%s, want %s", ErrBadStatus, id, rec.Status, want)
	}
	return nil
}

// Gate 是 experiment 的门禁列（Record 未含，单独读）。
type Gate struct {
	Status      string  `gorm:"column:status"`
	VoidReason  *string `gorm:"column:void_reason"`
	TokenBudget *int64  `gorm:"column:token_budget"`
}

// GateOf 读 experiment 的 status / void_reason / token_budget；不存在返回 ErrNotFound。
func (s *Experiments) GateOf(ctx context.Context, id uint64) (*Gate, error) {
	var rows []Gate
	if err := s.db.WithContext(ctx).Raw("SELECT status, void_reason, token_budget FROM experiment WHERE id = ?", id).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: read gate %d: %w", id, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("store: %w: %d", ErrNotFound, id)
	}
	return &rows[0], nil
}
