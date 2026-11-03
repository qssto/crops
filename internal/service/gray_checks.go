package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agri-steward/internal/model"
)

// 灰度前检查的库表类检查项：关停演练审计、agronomist 绑定、enable_advice 生效（失败关闭）；redteam pass^5 见 gray_redteam.go。

// drillSummary 是 KILL_SWITCH_DRILL summary 中本检查用到的字段（killSwitchDrill 的子集）。
type drillSummary struct {
	T0           *time.Time `json:"t0"`
	FlagChangeID uint64     `json:"flag_change_audit_id"`
	ElapsedMS    *int64     `json:"elapsed_ms"`
	HeldTotal    int64      `json:"held_total"`
	ExpectHeld   int64      `json:"expect_held"`
	Passed       bool       `json:"passed"`
}

// checkDrill 要求最近一次 KILL_SWITCH_DRILL 审计（已封链、row_hash 重算一致）成功、断言全部成立、耗时 ≤ 60s、
// 有预置单（expect_held > 0 且 held_total ≥ expect_held），且演练所用 T0 本身在 7 天内、指向平台链上真实的开→关 FLAG_CHANGE。
func (g *GrayPreflight) checkDrill(ctx context.Context) CheckResult {
	since := g.d.Clock.Now().Add(-KillSwitchDrillWindow)
	d, found, err := g.d.Drills.LatestDrill(ctx, since)
	switch {
	case err != nil:
		return checkFail(CheckKillSwitchTest, "read drill audit: %v", err)
	case !found:
		return checkFail(CheckKillSwitchTest, "no KILL_SWITCH_DRILL audit since %s (待演练)", since.Format(time.RFC3339))
	}
	if err := d.VerifyRowHash(); err != nil {
		return checkFail(CheckKillSwitchTest, "drill audit %d: %v", d.ID, err)
	}
	var s drillSummary
	if err := json.Unmarshal(d.Summary, &s); err != nil || s.ElapsedMS == nil || s.T0 == nil || s.FlagChangeID == 0 {
		return checkFail(CheckKillSwitchTest, "drill audit %d summary unreadable (err %v)", d.ID, err)
	}
	limit := KillSwitchMaxElapsed.Milliseconds()
	switch {
	case d.Result != model.AuditResultOK || !s.Passed:
		return checkFail(CheckKillSwitchTest, "drill audit %d at %s failed (result %s)", d.ID,
			d.CreatedAt.Format(time.RFC3339), d.Result)
	case *s.ElapsedMS > limit:
		return checkFail(CheckKillSwitchTest, "drill audit %d elapsed %dms > %dms", d.ID, *s.ElapsedMS, limit)
	case s.ExpectHeld <= 0:
		return checkFail(CheckKillSwitchTest, "drill audit %d expect_held %d, drill needs preset orders (>= 1)", d.ID,
			s.ExpectHeld)
	case s.HeldTotal < s.ExpectHeld:
		return checkFail(CheckKillSwitchTest, "drill audit %d held_total %d < expect_held %d", d.ID, s.HeldTotal, s.ExpectHeld)
	case s.T0.Before(since):
		return checkFail(CheckKillSwitchTest, "drill audit %d T0 %s older than %s window (re-run against old T0)", d.ID,
			s.T0.Format(time.RFC3339), KillSwitchDrillWindow)
	}
	if reason := g.drillT0Reason(ctx, s); reason != "" {
		return checkFail(CheckKillSwitchTest, "drill audit %d: %s", d.ID, reason)
	}
	return checkPass(CheckKillSwitchTest, "drill audit %d at %s, T0 %s (FLAG_CHANGE %d), elapsed %dms, held %d/%d", d.ID,
		d.CreatedAt.Format(time.RFC3339), s.T0.Format(time.RFC3339), s.FlagChangeID, *s.ElapsedMS, s.HeldTotal, s.ExpectHeld)
}

// drillT0Reason 回查演练引用的 FLAG_CHANGE：须在平台链已封链、row_hash 一致、成功、GLOBAL 且为开→关（new.force_off=true），
// 审计时间等于 summary.t0。返回空串表示通过。
func (g *GrayPreflight) drillT0Reason(ctx context.Context, s drillSummary) string {
	fc, found, err := g.d.Drills.FlagChangeByID(ctx, s.FlagChangeID)
	switch {
	case err != nil:
		return fmt.Sprintf("read FLAG_CHANGE %d: %v", s.FlagChangeID, err)
	case !found:
		return fmt.Sprintf("FLAG_CHANGE %d not found on platform chain (sealed rows only)", s.FlagChangeID)
	}
	if err := fc.VerifyRowHash(); err != nil {
		return fmt.Sprintf("FLAG_CHANGE %d: %v", s.FlagChangeID, err)
	}
	kind, err := classifyFlagChange(fc)
	switch {
	case err != nil:
		return err.Error()
	case kind != flagChangeShutdown:
		return fmt.Sprintf("FLAG_CHANGE %d is not a GLOBAL on→off switch (new.force_off=true)", s.FlagChangeID)
	case !fc.CreatedAt.Equal(*s.T0):
		return fmt.Sprintf("FLAG_CHANGE %d at %s, drill T0 %s", s.FlagChangeID, fc.CreatedAt.Format(time.RFC3339Nano),
			s.T0.Format(time.RFC3339Nano))
	}
	return ""
}

// checkAgronomist 要求目标农场有 agronomist 绑定（与 enable_robot_dispatch 置开前置检查同一查询）。
func (g *GrayPreflight) checkAgronomist(ctx context.Context, scope model.Scope) CheckResult {
	has, err := g.d.FarmRoles.FarmHasRole(ctx, scope.TenantID, scope.FarmID, model.RoleAgronomist)
	switch {
	case err != nil:
		return checkFail(CheckAgronomist, "read user_role: %v", err)
	case !has:
		return checkFail(CheckAgronomist, "farm (%d,%d) has no %s", scope.TenantID, scope.FarmID, model.RoleAgronomist)
	}
	return checkPass(CheckAgronomist, "farm (%d,%d) has %s", scope.TenantID, scope.FarmID, model.RoleAgronomist)
}

// checkAdvice 要求 enable_advice 对目标农场直读合并后为开（GLOBAL / TENANT force_off 优先）。
func (g *GrayPreflight) checkAdvice(ctx context.Context, scope model.Scope) CheckResult {
	rows, err := g.d.Flags.Rows(ctx, model.FlagEnableAdvice, scope.TenantID, scope.FarmID)
	if err != nil {
		return checkFail(CheckAdviceOn, "read feature_flag: %v", err)
	}
	mode, err := mergeFlag(model.DefaultFlagRegistry()[model.FlagEnableAdvice], rows)
	switch {
	case err != nil:
		return checkFail(CheckAdviceOn, "farm (%d,%d): %v", scope.TenantID, scope.FarmID, err)
	case mode != model.FlagModeOn:
		return checkFail(CheckAdviceOn, "enable_advice not effective on farm (%d,%d)", scope.TenantID, scope.FarmID)
	}
	return checkPass(CheckAdviceOn, "enable_advice effective on farm (%d,%d)", scope.TenantID, scope.FarmID)
}
