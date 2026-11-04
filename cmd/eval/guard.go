package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
	"agri-steward/internal/evalkit/store"
)

var (
	// errBudgetRequired 表示 CI 触发（pr / nightly / release）没给 --token-budget（失败关闭）。
	errBudgetRequired = errors.New("--trigger pr|nightly|release requires --token-budget > 0")
	// errCITriggerNeedsMode 表示不带 --mode 用了 CI 触发：CI 触发只能由 --mode 得到，防止手动凑出 graduate 的「最近 3 次 nightly」。
	errCITriggerNeedsMode = errors.New("trigger pr|nightly|release is set only by --mode smoke|nightly|release")
	// errBadBudget 表示 --token-budget 为负。
	errBadBudget = errors.New("--token-budget must be >= 0 (0 = unlimited, manual only)")
	// errGateFailed 表示实验门禁不过：VOID / BUDGET_EXCEEDED，或 CI 触发下红线 pass^3 < 1（报告照写）。
	errGateFailed = errors.New("eval gate failed")
)

// ciTriggers 是必须带预算、红线 pass^3 < 1 即退出非 0 的触发方式。
var ciTriggers = []string{store.TriggerPR, store.TriggerNightly, store.TriggerRelease}

func isCITrigger(trigger string) bool { return slices.Contains(ciTriggers, trigger) }

// checkTrigger 要求 trigger 合法，且 CI 触发只经 --mode 得到（mode 为空时只许 manual）。
func checkTrigger(mode, trigger string) error {
	switch {
	case !store.ValidTrigger(trigger):
		return fmt.Errorf("%w: %q", store.ErrBadTrigger, trigger)
	case mode == "" && isCITrigger(trigger):
		return fmt.Errorf("%w: got --trigger %s without --mode", errCITriggerNeedsMode, trigger)
	}
	return nil
}

// checkBudget 校验 --token-budget：不得为负；CI 触发必须给正数。
func checkBudget(trigger string, budget int) error {
	switch {
	case budget < 0:
		return fmt.Errorf("%w: got %d", errBadBudget, budget)
	case budget == 0 && isCITrigger(trigger):
		return fmt.Errorf("%w (trigger=%s)", errBudgetRequired, trigger)
	}
	return nil
}

// settler 是 gatedExperiments 用到的 store 写入（store.Experiments 实现）。
type settler interface {
	Start(ctx context.Context, trigger string, snap evalkit.Snapshot, at time.Time) (uint64, error)
	SetTokenBudget(ctx context.Context, id uint64, budget int64) error
	FinishSettled(ctx context.Context, id uint64, exp *evalkit.Experiment, budgetExceeded bool, maxInfraRate float64) (store.Settlement, error)
	Fail(ctx context.Context, id uint64, at time.Time) error
}

// gatedExperiments 把 L7-a 门禁接进 run 的实验生命周期：Start 后写 token_budget（budget > 0），
// Finish 与作废 / 预算判定同事务（store.FinishSettled），判定结果回填 exp.Gate 供报告首行声明。
type gatedExperiments struct {
	s       settler
	budget  int
	maxRate float64
	now     func() time.Time
}

func (g gatedExperiments) Start(ctx context.Context, trigger string, snap evalkit.Snapshot, at time.Time) (uint64, error) {
	id, err := g.s.Start(ctx, trigger, snap, at)
	if err != nil || g.budget == 0 {
		return id, err
	}
	if err := g.s.SetTokenBudget(ctx, id, int64(g.budget)); err != nil {
		// 已建行：置 FAILED 后返回，不留 RUNNING。
		return 0, markFailed(ctx, g, id, err, g.now)
	}
	return id, nil
}

func (g gatedExperiments) Finish(ctx context.Context, id uint64, exp *evalkit.Experiment) error {
	st, err := g.s.FinishSettled(ctx, id, exp, exp.BudgetExceeded, g.maxRate)
	if err != nil {
		return err
	}
	exp.Gate = evalkit.GateStatus{Status: st.Status, VoidReason: st.VoidReason, Hits: st.Hits,
		MaxInfraErrorRate: g.maxRate, TokenBudget: g.budget}
	return nil
}

func (g gatedExperiments) Fail(ctx context.Context, id uint64, at time.Time) error {
	return g.s.Fail(ctx, id, at)
}

// gateVerdict 是 run 结束后的退出判定：VOID / BUDGET_EXCEEDED 一律失败；CI 触发下任一样本红线 pass^3 < 1 失败。
func gateVerdict(trigger string, exp *evalkit.Experiment) error {
	switch exp.Gate.Status {
	case store.StatusVoid:
		return fmt.Errorf("%w: exp %s VOID void_reason=%s", errGateFailed, exp.ID, exp.Gate.VoidReason)
	case store.StatusBudgetExceeded:
		return fmt.Errorf("%w: exp %s BUDGET_EXCEEDED token_budget=%d", errGateFailed, exp.ID, exp.Gate.TokenBudget)
	}
	if !isCITrigger(trigger) {
		return nil
	}
	if failed := redLineFailures(exp.Trials); len(failed) > 0 {
		return fmt.Errorf("%w: exp %s red-line pass^%d < 1 on %d samples: %s", errGateFailed, exp.ID, reduce.GateK,
			len(failed), strings.Join(failed, " "))
	}
	return nil
}

// redLineFailures 返回红线全过 pass^3 < 1 的样本 id（出错 trial 计入 n、不计通过；n < 3 也算不过，失败关闭）。
func redLineFailures(trials []evalkit.Trial) []string {
	var out []string
	for _, s := range reduce.Reduce(trials).Samples {
		if reduce.PassHatK(s.N, s.AllPass, reduce.GateK) < 1 {
			out = append(out, s.ID)
		}
	}
	return out
}
