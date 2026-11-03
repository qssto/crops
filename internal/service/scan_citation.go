package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L7-B 无引用建议发现（红线）：worker 每个扫描周期（30s）查 [now-window, now] 内创建、非 REFUSED 且无 advice_citation 的
// 建议卡，逐张 Fire(advice_no_citation, P0, run_id)。
//
// 去重：每张建议只告警一次。advice_card 每个 run 至多一张（uk_run），告警审计行 ALERT.advice_no_citation 带 run_id，
// 发告警前按 (tenant_id, farm_id, run_id IN …, action, created_at>=from) 查已有审计行，已告警的跳过；
// lock:scan:advice_no_citation 保证多实例同一时刻只有一个在扫。代价：告警审计短写失败时该行缺失，
// 窗口内下一轮会再告警一次（宁可重复不漏报）；worker 停摆超过 window 的建议不再补扫（window 可配）。
//
// 锁内阻塞（审查修复 P2，二选一选「提前结束本轮」，不改异步队列）：Fire 同步写审计（每次 ≤ alerts.audit_write_timeout，
// 缺省 500ms），一轮可能有多张命中。单轮预算 DefaultCitationRoundBudget = scanLockTTL/2（30s，lock:scan TTL 60s 的一半），
// 每个农场与每次 Fire 前检查，用尽即 Warn 并结束本轮；未发出的告警没有审计行，下一轮（30s 后）按去重规则补发，
// window 2m 内约有 4 轮机会。选它而不选异步队列：告警出口保持同步可测，不新增队列满丢弃这一种失败；
// 代价是单轮超过约 60 张命中时告警延后一轮。

// DefaultCitationRoundBudget 是无引用扫描单轮时间预算（lock:scan TTL 的一半，留出余量给锁释放与其余任务）。
const DefaultCitationRoundBudget = scanLockTTL / 2

// ScanAdviceNoCitation 是无引用建议扫描的任务名（lock:scan:advice_no_citation，TTL 60s）。
const ScanAdviceNoCitation = "advice_no_citation"

// DefaultNoCitationWindow 是无引用扫描的回看窗口（最近 2 分钟）。
const DefaultNoCitationWindow = 2 * time.Minute

// CitationScanStore 是无引用扫描的仓储能力，repository.AlertRepo 实现。
type CitationScanStore interface {
	AdviceWithoutCitation(ctx context.Context, sys repository.SystemScope, from, to time.Time) ([]repository.AdviceKey, error)
	AlertedRuns(ctx context.Context, scope model.Scope, action model.AuditAction, runIDs []uint64,
		since time.Time) (map[uint64]bool, error)
}

// citationScanReady 表示无引用扫描的依赖齐全（缺任一不注册该任务）。
func (d ScanDeps) citationScanReady() bool { return d.Citations != nil && d.Alerts != nil }

// adviceNoCitation 按农场分组去重后逐张告警；单个农场查询失败不影响其余，错误汇总返回。
// 单轮受 CitationBudget 约束：查询用带预算的 rctx，每个农场与每次 Fire 前检查预算，用尽即 Warn 并结束本轮（返回已汇总的错误），
// 没发出的告警因按审计行去重，下一轮补发；调用方 ctx 取消时返回 ctx.Err()。
func (s *Scanner) adviceNoCitation(ctx context.Context, now time.Time) error {
	window := s.d.CitationWindow
	if window <= 0 {
		window = DefaultNoCitationWindow
	}
	budget := s.d.CitationBudget
	if budget <= 0 {
		budget = DefaultCitationRoundBudget
	}
	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	from := now.Add(-window)
	rows, err := s.d.Citations.AdviceWithoutCitation(rctx, scanSystem, from, now)
	if err != nil {
		return err
	}
	type farmKey struct{ tenant, farm uint64 }
	byFarm := map[farmKey][]repository.AdviceKey{}
	var order []farmKey
	for _, r := range rows {
		k := farmKey{r.TenantID, r.FarmID}
		if _, ok := byFarm[k]; !ok {
			order = append(order, k)
		}
		byFarm[k] = append(byFarm[k], r)
	}
	var errs []error
	for i, k := range order {
		err := s.alertNoCitation(ctx, rctx, model.Scope{TenantID: k.tenant, FarmID: k.farm}, byFarm[k], from)
		if errors.Is(err, errCitationBudget) {
			s.d.Logger.WarnContext(ctx, "scan: advice_no_citation round budget exhausted, rest deferred to next round",
				"budget", budget, "farms_left", len(order)-i)
			break
		}
		if err != nil {
			errs = append(errs, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

// errCitationBudget 表示本轮预算用尽（不是故障，不进错误汇总）。
var errCitationBudget = errors.New("advice_no_citation round budget exhausted")

// roundStopped 在 rctx 结束时返回停止原因：调用方取消返回 ctx.Err()，否则是预算用尽；rctx 未结束返回 nil。
func roundStopped(ctx, rctx context.Context) error {
	if rctx.Err() == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errCitationBudget
}

func (s *Scanner) alertNoCitation(ctx, rctx context.Context, sc model.Scope, rows []repository.AdviceKey, from time.Time) error {
	if err := roundStopped(ctx, rctx); err != nil {
		return err
	}
	runIDs := make([]uint64, 0, len(rows))
	for _, r := range rows {
		runIDs = append(runIDs, r.RunID)
	}
	done, err := s.d.Citations.AlertedRuns(rctx, sc, model.AlertAdviceNoCitation.AuditAction(), runIDs, from)
	if err != nil {
		if stop := roundStopped(ctx, rctx); stop != nil {
			return stop
		}
		return fmt.Errorf("farm %d: %w", sc.FarmID, err)
	}
	for _, r := range rows {
		if done[r.RunID] {
			continue
		}
		if err := roundStopped(ctx, rctx); err != nil {
			return err
		}
		done[r.RunID] = true
		s.d.Alerts.Fire(ctx, model.Alert{Name: model.AlertAdviceNoCitation, Level: model.AlertP0, Scope: sc, RunID: r.RunID,
			Detail: "advice " + strconv.FormatUint(r.ID, 10) + " has no citation"})
	}
	return nil
}
