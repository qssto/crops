package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 一键关停报告（P1 §6.9 技术实现第 4 条，cmd/auditctl kill-switch-report）：以 GLOBAL 行开→关的 FLAG_CHANGE
// （old 为空或 force_off:false，new force_off:true）审计时间为 T0，其后第一条 GLOBAL force_off:false 为解除时刻（计数上界），
// 断言 [T0, 解除) 内 max(sent_at WHERE mode='REAL') − T0 ≤ 60s、发出的 REAL 尝试 flag_read_at 都早于 T0、
// （给了预置数时）各灰度农场 DISPATCH_HELD 之和 ≥ 未发出的预置单数。审计行只认已封链且 row_hash 重算一致的。
// 只有 --drill 写一条 KILL_SWITCH_DRILL 审计（须给 --expect-held ≥ 1）。

const (
	// KillSwitchMaxElapsed 是关停生效上限：T0 之后发出的最后一单不得晚于 T0 + 60s（§6.9 验收 3）。
	KillSwitchMaxElapsed = 60 * time.Second
	// KillSwitchDrillWindow 是 gray-preflight 认可的演练时效（7 天内）。
	KillSwitchDrillWindow = 7 * 24 * time.Hour
	// killSwitchTarget 是 enable_robot_dispatch 的 FLAG_CHANGE / KILL_SWITCH_DRILL 审计 target。
	killSwitchTarget = "flag:" + string(model.FlagEnableRobotDispatch)
	// killSwitchScanLimit 是从 --since 起最多查看的 FLAG_CHANGE 条数（定位第一条 GLOBAL force_off=true）。
	killSwitchScanLimit = 200
)

// ErrKillSwitchNoT0 表示 --since 之后找不到 GLOBAL 行开→关（force_off=true）的 FLAG_CHANGE 审计。
var ErrKillSwitchNoT0 = errors.New("service: no GLOBAL on→off force_off=true FLAG_CHANGE audit")

// ErrKillSwitchScanLimit 表示找到 T0 后扫描到上限仍未见解除行，无法确定计数上界（失败关闭，需收窄 --since）。
var ErrKillSwitchScanLimit = errors.New("service: kill switch FLAG_CHANGE scan limit reached")

// killSwitchSystem 是关停报告跨农场读 dispatch_attempt（idx_mode_sent）的凭证。
var killSwitchSystem = repository.NewSystemScope("kill_switch_report")

// KillSwitchStore 是报告的只读查询（repository.KillSwitchRepo 实现）。
type KillSwitchStore interface {
	FlagChangesSince(ctx context.Context, target string, since time.Time, limit int) ([]repository.AuditEntry, error)
	RealSentAfter(ctx context.Context, sys repository.SystemScope, t0 time.Time, until *time.Time) (*time.Time, int64, error)
	RealReadAfter(ctx context.Context, sys repository.SystemScope, t0 time.Time, until *time.Time) (int64, error)
	HeldSince(ctx context.Context, tenantID, farmID uint64, t0 time.Time, until *time.Time) (int64, error)
}

// FlagRowLister 列出开关全部行（repository.FlagRepo 实现，§6.9 灰度档位 SQL）。
type FlagRowLister interface {
	List(ctx context.Context, key model.FlagKey) ([]model.FlagSetting, error)
}

// KillSwitchDeps 是报告依赖；Audit 只在 Drill 时使用（cmd/auditctl 仅 --drill 时装配可写连接）。
type KillSwitchDeps struct {
	Store KillSwitchStore
	Flags FlagRowLister
	Audit AuditAppender
	Clock clock.Clock
}

// KillSwitchReporter 计算关停报告。
type KillSwitchReporter struct{ d KillSwitchDeps }

// NewKillSwitchReporter 校验依赖。
func NewKillSwitchReporter(d KillSwitchDeps) (*KillSwitchReporter, error) {
	if d.Store == nil || d.Flags == nil || d.Clock == nil {
		return nil, errors.New("service: kill switch report needs store / flags / clock")
	}
	return &KillSwitchReporter{d: d}, nil
}

// FarmHeld 是一个灰度农场（enable_robot_dispatch 有 FARM 行）T0 后的 DISPATCH_HELD 计数。
type FarmHeld struct {
	TenantID uint64          `json:"tenant_id"`
	FarmID   uint64          `json:"farm_id"`
	Value    json.RawMessage `json:"value"`
	Held     int64           `json:"held"`
}

// KillSwitchReport 是报告结果；Failures 为空即全部断言成立。
type KillSwitchReport struct {
	T0            time.Time  `json:"t0"`
	FlagChangeID  uint64     `json:"flag_change_audit_id"`
	ReleasedAt    *time.Time `json:"released_at"`      // 解除关停（GLOBAL force_off:false）时刻，计数上界；nil 表示未解除
	ReleaseID     uint64     `json:"release_audit_id"` // 解除行的 FLAG_CHANGE 审计 id
	MaxSentAt     *time.Time `json:"max_sent_at"`
	ElapsedMS     int64      `json:"elapsed_ms"` // max(sent_at) − T0；T0 后没有 REAL 发出为 0
	RealSentAfter int64      `json:"real_sent_after"`
	FlagReadAfter int64      `json:"flag_read_after"` // 期望 0
	HeldTotal     int64      `json:"held_total"`
	ExpectHeld    int64      `json:"expect_held"` // 未发出的预置单数；0 表示不断言
	Farms         []FarmHeld `json:"farms"`
	Failures      []string   `json:"failures"`
}

// Passed 表示全部断言成立。
func (r KillSwitchReport) Passed() bool { return len(r.Failures) == 0 }

// forceOffValue 是 GLOBAL 行 enable_robot_dispatch 的取值（{"force_off": bool}）。
type forceOffValue struct {
	ForceOff *bool `json:"force_off"`
}

// flagChangeSummary 是 FLAG_CHANGE summary 中定位 T0 / 解除用到的字段；首次设置时 old 为 null。
type flagChangeSummary struct {
	ScopeType model.FlagScopeType `json:"scope_type"`
	Old       *forceOffValue      `json:"old"`
	New       forceOffValue       `json:"new"`
}

// flagChangeKind 是一条 FLAG_CHANGE 对关停报告的意义。
type flagChangeKind int

const (
	flagChangeOther    flagChangeKind = iota
	flagChangeShutdown                // GLOBAL 开→关：old 为空或 force_off:false，new force_off:true（T0）
	flagChangeRelease                 // GLOBAL new force_off:false（解除）
)

// classifyFlagChange 按 summary 判定一条成功的 FLAG_CHANGE 是关停、解除还是无关（关→关的重复写不算 T0）。
func classifyFlagChange(e repository.AuditEntry) (flagChangeKind, error) {
	var s flagChangeSummary
	if err := json.Unmarshal(e.Summary, &s); err != nil {
		return flagChangeOther, fmt.Errorf("FLAG_CHANGE audit %d summary: %w", e.ID, err)
	}
	if e.Result != model.AuditResultOK || s.ScopeType != model.FlagScopeGlobal || s.New.ForceOff == nil {
		return flagChangeOther, nil
	}
	wasOff := s.Old != nil && s.Old.ForceOff != nil && *s.Old.ForceOff
	switch {
	case *s.New.ForceOff && !wasOff:
		return flagChangeShutdown, nil
	case !*s.New.ForceOff:
		return flagChangeRelease, nil
	}
	return flagChangeOther, nil
}

// Report 定位 T0 并逐项计算断言；expectHeld 为预置单中未发出的张数（0 表示不断言 DISPATCH_HELD 下限）。
func (k *KillSwitchReporter) Report(ctx context.Context, since time.Time, expectHeld int64) (KillSwitchReport, error) {
	if expectHeld < 0 {
		return KillSwitchReport{}, fmt.Errorf("%w: expect held must be >= 0", ErrBadInput)
	}
	audit, release, err := k.locateT0(ctx, since)
	if err != nil {
		return KillSwitchReport{}, err
	}
	t0 := audit.CreatedAt
	rep := KillSwitchReport{T0: t0, FlagChangeID: audit.ID, ExpectHeld: expectHeld, Farms: []FarmHeld{}, Failures: []string{}}
	if release != nil {
		at := release.CreatedAt
		rep.ReleasedAt, rep.ReleaseID = &at, release.ID
	}
	if rep.MaxSentAt, rep.RealSentAfter, err = k.d.Store.RealSentAfter(ctx, killSwitchSystem, t0, rep.ReleasedAt); err != nil {
		return KillSwitchReport{}, fmt.Errorf("service: kill switch report: %w", err)
	}
	if rep.FlagReadAfter, err = k.d.Store.RealReadAfter(ctx, killSwitchSystem, t0, rep.ReleasedAt); err != nil {
		return KillSwitchReport{}, fmt.Errorf("service: kill switch report: %w", err)
	}
	if err := k.countHeld(ctx, &rep); err != nil {
		return KillSwitchReport{}, err
	}
	if rep.MaxSentAt != nil {
		rep.ElapsedMS = rep.MaxSentAt.Sub(t0).Milliseconds()
	}
	rep.Failures = killSwitchFailures(rep)
	return rep, nil
}

// locateT0 取 since 之后第一条 GLOBAL 开→关的 FLAG_CHANGE 为 T0，其后第一条 GLOBAL force_off:false 为解除（可为 nil）。
// 两行都重算 row_hash，不一致即报错（不拿被改过的审计当 T0）。
func (k *KillSwitchReporter) locateT0(ctx context.Context, since time.Time) (repository.AuditEntry, *repository.AuditEntry,
	error) {
	rows, err := k.d.Store.FlagChangesSince(ctx, killSwitchTarget, since, killSwitchScanLimit)
	if err != nil {
		return repository.AuditEntry{}, nil, fmt.Errorf("service: kill switch locate T0: %w", err)
	}
	t0 := -1
	for i, r := range rows {
		kind, err := classifyFlagChange(r)
		if err != nil {
			return repository.AuditEntry{}, nil, fmt.Errorf("service: kill switch: %w", err)
		}
		switch {
		case t0 < 0 && kind == flagChangeShutdown:
			if err := r.VerifyRowHash(); err != nil {
				return repository.AuditEntry{}, nil, fmt.Errorf("service: kill switch T0: %w", err)
			}
			t0 = i
		case t0 >= 0 && kind == flagChangeRelease:
			if err := r.VerifyRowHash(); err != nil {
				return repository.AuditEntry{}, nil, fmt.Errorf("service: kill switch release: %w", err)
			}
			return rows[t0], &rows[i], nil
		}
	}
	switch {
	case t0 < 0:
		return repository.AuditEntry{}, nil, fmt.Errorf("%w on %s since %s (scanned %d)", ErrKillSwitchNoT0, killSwitchTarget,
			since.Format(time.RFC3339Nano), len(rows))
	case len(rows) >= killSwitchScanLimit:
		return repository.AuditEntry{}, nil, fmt.Errorf("%w: %d rows after T0 audit %d without release, narrow --since",
			ErrKillSwitchScanLimit, len(rows), rows[t0].ID)
	}
	return rows[t0], nil, nil
}

// countHeld 按灰度农场（enable_robot_dispatch 的 FARM 行）逐个查 DISPATCH_HELD 后求和。
func (k *KillSwitchReporter) countHeld(ctx context.Context, rep *KillSwitchReport) error {
	rows, err := k.d.Flags.List(ctx, model.FlagEnableRobotDispatch)
	if err != nil {
		return fmt.Errorf("service: kill switch gray farms: %w", err)
	}
	for _, r := range rows {
		if r.ScopeType != model.FlagScopeFarm {
			continue
		}
		n, err := k.d.Store.HeldSince(ctx, r.TenantID, r.FarmID, rep.T0, rep.ReleasedAt)
		if err != nil {
			return fmt.Errorf("service: kill switch report: %w", err)
		}
		rep.Farms = append(rep.Farms, FarmHeld{TenantID: r.TenantID, FarmID: r.FarmID, Value: r.Value, Held: n})
		rep.HeldTotal += n
	}
	return nil
}

// killSwitchFailures 逐条核对断言，返回不成立项（人读的原因）。
func killSwitchFailures(r KillSwitchReport) []string {
	out := []string{}
	if r.ElapsedMS > KillSwitchMaxElapsed.Milliseconds() {
		out = append(out, fmt.Sprintf("max(sent_at)-T0 = %dms > %dms", r.ElapsedMS, KillSwitchMaxElapsed.Milliseconds()))
	}
	if r.FlagReadAfter != 0 {
		out = append(out, fmt.Sprintf("%d REAL attempts sent after T0 read the flag at or after T0 (want 0)", r.FlagReadAfter))
	}
	if r.HeldTotal < r.ExpectHeld {
		out = append(out, fmt.Sprintf("DISPATCH_HELD total %d < unsent preset orders %d", r.HeldTotal, r.ExpectHeld))
	}
	return out
}

// killSwitchDrill 是 KILL_SWITCH_DRILL 审计的 summary（gray-preflight 读 elapsed_ms / passed）；
// 只记灰度农场数不记逐农场明细，保证 summary 不超 8KB 被截成长度。
type killSwitchDrill struct {
	T0            time.Time  `json:"t0"`
	FlagChangeID  uint64     `json:"flag_change_audit_id"`
	ReleasedAt    *time.Time `json:"released_at"`
	ElapsedMS     int64      `json:"elapsed_ms"`
	MaxSentAt     *time.Time `json:"max_sent_at"`
	RealSentAfter int64      `json:"real_sent_after"`
	FlagReadAfter int64      `json:"flag_read_after"`
	HeldTotal     int64      `json:"held_total"`
	ExpectHeld    int64      `json:"expect_held"`
	GrayFarms     int        `json:"gray_farms"`
	Failures      []string   `json:"failures"`
	Passed        bool       `json:"passed"`
}

// RecordDrill 经已封链的独立短事务写平台链 (0,0) 的 KILL_SWITCH_DRILL 审计；断言不成立也写（result=FAILED），留作复盘。
// 没有预置单（ExpectHeld < 1）的空演练拒绝写入。
func (k *KillSwitchReporter) RecordDrill(ctx context.Context, rep KillSwitchReport) error {
	if k.d.Audit == nil {
		return errors.New("service: kill switch drill needs audit appender")
	}
	if rep.ExpectHeld < 1 {
		return fmt.Errorf("%w: drill needs preset orders, expect held must be >= 1", ErrBadInput)
	}
	payload, err := json.Marshal(killSwitchDrill{T0: rep.T0, FlagChangeID: rep.FlagChangeID, ReleasedAt: rep.ReleasedAt,
		ElapsedMS: rep.ElapsedMS,
		MaxSentAt: rep.MaxSentAt, RealSentAfter: rep.RealSentAfter, FlagReadAfter: rep.FlagReadAfter, HeldTotal: rep.HeldTotal,
		ExpectHeld: rep.ExpectHeld, GrayFarms: len(rep.Farms), Failures: rep.Failures, Passed: rep.Passed()})
	if err != nil {
		return fmt.Errorf("service: encode KILL_SWITCH_DRILL: %w", err)
	}
	result := model.AuditResultOK
	if !rep.Passed() {
		result = model.AuditResultFailed
	}
	rec := repository.NewAuditRecord(model.Scope{}, nil, model.Actor{Type: model.ActorSystem}, model.AuditKillSwitchDrill,
		killSwitchTarget, payload, result, k.d.Clock.Now())
	if err := k.d.Audit.Append(ctx, rec); err != nil {
		return fmt.Errorf("service: write KILL_SWITCH_DRILL: %w", err)
	}
	return nil
}
