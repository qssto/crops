package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 灰度前检查（P1 §6.9 技术实现第 1 条，cmd/auditctl gray-preflight）：逐项给出 PASS / FAIL 与原因，任一 FAIL 不得打开
// enable_robot_dispatch。取数失败按该项 FAIL（失败关闭），不中断其余检查。

// 灰度前检查读取的仓库文件（相对仓库根，经 GrayPreflightDeps.Files 读）。
const (
	ExternalDecisionsPath = "policy/external_decisions.yaml"
	RegulationMapPath     = "policy/regulation_map.yaml"
	AlertRulesPath        = "deploy/alerts/p1.yaml"
	// RedteamSuiteDir 是红队样本目录：pass^5 的应有样本集以仓库内文件为准（L8-B 审查 P0-1）。
	RedteamSuiteDir = "eval/suites/redteam"
)

// DefaultRedteamMaxAge 是 redteam 实验 finished_at 的默认时效（夜间全量每天一次，留 12h 余量）。
const DefaultRedteamMaxAge = 36 * time.Hour

// 检查项名（输出与测试共用）。
const (
	CheckD5Closed       = "d5_closed"
	CheckLegalSignoff   = "legal_signoff"
	CheckAlertRules     = "alert_rules"
	CheckKillSwitchTest = "kill_switch_drill"
	CheckRedteamPassK   = "redteam_pass5"
	CheckAgronomist     = "agronomist_bound"
	CheckAdviceOn       = "enable_advice_on"
)

const (
	// grayDecisionD5 是灰度前必须闭环的外部决策（作业事故责任划分与保险方案）。
	grayDecisionD5 = "D5"
	// decisionClosed 是外部决策已闭环的状态值。
	decisionClosed = "CLOSED"
	// redteamSuite 是评测库中红队套件名；redteamK 是 pass^k 的 k（§6.9 验收 1：pass^5 = 1）。
	redteamSuite = "redteam"
	redteamK     = 5
	// experimentDone / trialInfraError 是评测库 experiment.status / trial.status 取值（migrations/eval/0001）。
	experimentDone  = "DONE"
	trialInfraError = "INFRA_ERROR"
)

// DrillReader 读最近一次 KILL_SWITCH_DRILL 审计及其引用的 FLAG_CHANGE（repository.KillSwitchRepo 实现，只读已封链行）。
type DrillReader interface {
	LatestDrill(ctx context.Context, since time.Time) (repository.AuditEntry, bool, error)
	FlagChangeByID(ctx context.Context, id uint64) (repository.AuditEntry, bool, error)
}

// RedteamReader 读评测库最近一次含 redteam 的实验及其 trial（repository.EvalRedteamRepo 实现）。
type RedteamReader interface {
	LatestExperimentWithSuite(ctx context.Context, suite string) (repository.EvalExperiment, bool, error)
	SuiteTrials(ctx context.Context, experimentID uint64, suite string) ([]repository.EvalTrial, error)
}

// FlagRowsReader 读开关三层原始行（repository.FlagRepo 实现），按 mergeFlag 直读合并（不走缓存）。
type FlagRowsReader interface {
	Rows(ctx context.Context, key model.FlagKey, tenantID, farmID uint64) ([]model.FlagRow, error)
}

// GrayPreflightDeps 是灰度前检查的依赖；Files 根为仓库根（cmd 用 os.DirFS，单测用 fstest.MapFS）。
// RedteamMaxAge 为 0 时取 DefaultRedteamMaxAge。
type GrayPreflightDeps struct {
	Files         fs.FS
	Drills        DrillReader
	Redteam       RedteamReader
	FarmRoles     FarmRoleChecker
	Flags         FlagRowsReader
	Clock         clock.Clock
	RedteamMaxAge time.Duration
}

// GrayPreflight 执行灰度前检查。
type GrayPreflight struct{ d GrayPreflightDeps }

// NewGrayPreflight 校验依赖。
func NewGrayPreflight(d GrayPreflightDeps) (*GrayPreflight, error) {
	if d.Files == nil || d.Drills == nil || d.Redteam == nil || d.FarmRoles == nil || d.Flags == nil || d.Clock == nil {
		return nil, errors.New("service: gray preflight needs files / drills / redteam / farm roles / flags / clock")
	}
	switch {
	case d.RedteamMaxAge < 0:
		return nil, fmt.Errorf("%w: redteam max age must be >= 0", ErrBadInput)
	case d.RedteamMaxAge == 0:
		d.RedteamMaxAge = DefaultRedteamMaxAge
	}
	return &GrayPreflight{d: d}, nil
}

// CheckResult 是一项检查的结论。
type CheckResult struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

func checkPass(name, format string, args ...any) CheckResult {
	return CheckResult{Name: name, Pass: true, Detail: fmt.Sprintf(format, args...)}
}

func checkFail(name, format string, args ...any) CheckResult {
	return CheckResult{Name: name, Detail: fmt.Sprintf(format, args...)}
}

// AllPassed 表示全部检查通过。
func AllPassed(rs []CheckResult) bool {
	for _, r := range rs {
		if !r.Pass {
			return false
		}
	}
	return len(rs) > 0
}

// Run 按固定顺序执行全部检查；scope 为目标农场 (tenant, farm)。
func (g *GrayPreflight) Run(ctx context.Context, scope model.Scope) ([]CheckResult, error) {
	if scope.TenantID == 0 || scope.FarmID == 0 {
		return nil, fmt.Errorf("%w: gray preflight needs farm scope, got (%d,%d)", ErrBadInput, scope.TenantID, scope.FarmID)
	}
	return []CheckResult{
		g.checkD5(),
		g.checkSignoff(),
		g.checkAlertRules(),
		g.checkDrill(ctx),
		g.checkRedteam(ctx),
		g.checkAgronomist(ctx, scope),
		g.checkAdvice(ctx, scope),
	}, nil
}
