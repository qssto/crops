package repository

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// scopeRequiredCalls 是 TestScopeRequired 的被测方法（键为「接收者.方法」）；
// TestScopeRequired_Coverage 静态要求带归属入参的导出方法都列在这里。
func scopeRequiredCalls() map[string]func(model.Scope) error {
	ctx := context.Background()
	now := time.Unix(0, 0)
	noFarm := model.OrderRef{TenantID: 1, OrderID: 9}
	noFarmTicket := TicketRow{TenantID: 1, ID: 9}
	noFarmRun := RunKey{TenantID: 1, ID: 1}
	return map[string]func(model.Scope) error{
		"AdviceRepo.SaveAdvice": func(s model.Scope) error {
			_, err := (&AdviceRepo{}).SaveAdvice(ctx, s, model.AdviceCard{})
			return err
		},
		"AdviceRepo.GetByRun": func(s model.Scope) error { _, err := (&AdviceRepo{}).GetByRun(ctx, s, 1); return err },
		"ApprovalRepo.TicketByRun": func(s model.Scope) error {
			_, _, err := ApprovalRepo{}.TicketByRun(ctx, nil, s, 1)
			return err
		},
		"ApprovalRepo.ListOpen":      func(s model.Scope) error { _, err := ApprovalRepo{}.ListOpen(ctx, nil, s); return err },
		"ApprovalRepo.ActivateTx":    func(s model.Scope) error { _, err := ApprovalRepo{}.ActivateTx(ctx, nil, s, 1, now, now); return err },
		"DecisionRepo.TicketTx":      func(s model.Scope) error { _, err := (&DecisionRepo{}).TicketTx(ctx, nil, s, 1); return err },
		"DecisionRepo.ListPending":   func(s model.Scope) error { _, err := (&DecisionRepo{}).ListPending(ctx, s); return err },
		"DecisionRepo.CloseRunTx":    func(s model.Scope) error { return (&DecisionRepo{}).CloseRunTx(ctx, nil, s, 1, "", now) },
		"DecisionRepo.AuditDenied":   func(s model.Scope) error { return (&DecisionRepo{}).AuditDenied(ctx, s, TicketRow{}, "x") },
		"DispatchRepo.RecheckSource": func(s model.Scope) error { _, err := DispatchRepo{}.RecheckSource(ctx, s, 1); return err },
		"DispatchRepo.SupersedeRun":  func(s model.Scope) error { _, err := DispatchRepo{}.SupersedeRun(ctx, s, 1, 2, now); return err },
		"FieldScope.Owns":            func(s model.Scope) error { _, err := FieldScope{}.Owns(ctx, s, 1); return err },
		"LocalOrderRepo.RecentOrders": func(s model.Scope) error {
			_, err := LocalOrderRepo{}.RecentOrders(ctx, s, 1, 0, now)
			return err
		},
		"FieldScopeGuard.CheckField": func(s model.Scope) error { return FieldScopeGuard{}.CheckField(ctx, s, 1, 1) },
		"FieldScopeGuard.Deny":       func(s model.Scope) error { return FieldScopeGuard{}.Deny(ctx, s, 1, "device:x") },
		"FarmRegions.FarmRegion":     func(s model.Scope) error { _, err := FarmRegions{}.FarmRegion(ctx, s); return err },
		"FieldDirectory.FieldCode":   func(s model.Scope) error { _, err := FieldDirectory{}.FieldCode(ctx, s, 1); return err },
		"FieldDirectory.FieldIDs":    func(s model.Scope) error { _, err := FieldDirectory{}.FieldIDs(ctx, s); return err },
		"MemoryRepo.Profile":         func(s model.Scope) error { _, err := (&MemoryRepo{}).Profile(ctx, s, 1); return err },
		"MemoryRepo.Episodic":        func(s model.Scope) error { _, err := (&MemoryRepo{}).Episodic(ctx, s, 1, 5); return err },
		"MetricsRepo.Facts":          func(s model.Scope) error { _, err := (&MetricsRepo{}).Facts(ctx, s, now, now); return err },
		"EventBackfillRepo.EventsAfter": func(s model.Scope) error {
			_, err := EventBackfillRepo{}.EventsAfter(ctx, s, 1, 0)
			return err
		},
		"OrderDraftRepo.CreateDraft": func(s model.Scope) error {
			_, err := (&OrderDraftRepo{}).CreateDraft(ctx, s, model.WorkOrder{})
			return err
		},
		"RawAccessRepo.RawTx": func(s model.Scope) error { _, err := RawAccessRepo{}.RawTx(ctx, nil, s, 1); return err },
		"RawReader.Get":       func(s model.Scope) error { _, err := RawReader{}.Get(ctx, s, 1); return err },
		"RunDetailRepo.Load":  func(s model.Scope) error { _, err := (&RunDetailRepo{}).Load(ctx, s, 1); return err },
		"RunRepo.Summary":     func(s model.Scope) error { _, err := (&RunRepo{}).Summary(ctx, s, 1); return err },
		"RunRepo.LoadRun":     func(s model.Scope) error { _, err := (&RunRepo{}).LoadRun(ctx, s, 1); return err },
		"RunRepo.SaveStep":    func(s model.Scope) error { return (&RunRepo{}).SaveStep(ctx, s, 1, model.RunStep{}) },
		"RunRepo.FinishRun":   func(s model.Scope) error { return (&RunRepo{}).FinishRun(ctx, s, 1, model.RunCompleted, "") },
		"RunRepo.Heartbeat":   func(s model.Scope) error { return (&RunRepo{}).Heartbeat(ctx, s, 1) },
		"RunRepo.MarkResumed": func(s model.Scope) error {
			_, err := (&RunRepo{}).MarkResumed(ctx, s, agent.ResumeRequest{})
			return err
		},
		"RunRepo.DraftOrdersTx": func(s model.Scope) error { _, err := (&RunRepo{}).DraftOrdersTx(ctx, nil, s, 1); return err },
		"RunRepo.RecheckEpoch":  func(s model.Scope) error { _, err := (&RunRepo{}).RecheckEpoch(ctx, s, 1); return err },
		"SummaryRepo.ByRun":     func(s model.Scope) error { _, err := SummaryRepo{}.ByRun(ctx, nil, s, 1); return err },
		"ToolCallRepo.FindOK":   func(s model.Scope) error { _, _, err := (&ToolCallRepo{}).FindOK(ctx, s, "k"); return err },
		"WorkOrderRepo.TransitionsByRun": func(s model.Scope) error {
			_, err := WorkOrderRepo{}.TransitionsByRun(ctx, nil, s, 1)
			return err
		},
		"OutboxRepo.OutboxByAggregate": func(s model.Scope) error {
			_, err := OutboxRepo{}.OutboxByAggregate(ctx, nil, s, 1)
			return err
		},
		"OutboxRepo.RedeliverDone": func(s model.Scope) error {
			_, err := OutboxRepo{}.RedeliverDone(ctx, nil, s, 1, TopicOrderDispatch, now)
			return err
		},
		// 参数自带归属（OrderRef / TicketRow / RunKey）的方法：缺 farm_id 同样拒绝。
		"ApprovalRepo.TicketByOrderSeq": func(model.Scope) error {
			_, _, err := ApprovalRepo{}.TicketByOrderSeq(ctx, nil, noFarm, 1)
			return err
		},
		"DecisionRepo.ExpireTicketTx": func(model.Scope) error {
			_, err := (&DecisionRepo{}).ExpireTicketTx(ctx, nil, noFarmTicket, now)
			return err
		},
		"DispatchRepo.OrderForDispatch": func(model.Scope) error {
			_, err := DispatchRepo{}.OrderForDispatch(ctx, nil, noFarm)
			return err
		},
		"WorkOrderRepo.GetState": func(model.Scope) error { _, err := WorkOrderRepo{}.GetState(ctx, nil, noFarm); return err },
		"RunRepo.MarkSystemError": func(model.Scope) error {
			_, err := (&RunRepo{}).MarkSystemError(ctx, noFarmRun, now)
			return err
		},
		// P1-L1-B 审查 P2-9：补齐此前未列的方法。
		"DecisionRepo.OrdersWithChecks": func(s model.Scope) error {
			_, err := (&DecisionRepo{}).OrdersWithChecks(ctx, s, []uint64{1})
			return err
		},
		"OutboxRepo.MarkDoneTx": func(s model.Scope) error { _, err := OutboxRepo{}.MarkDoneTx(ctx, nil, s, 1, now); return err },
		"ApprovalRepo.UpsertChecksTx": func(model.Scope) error {
			return ApprovalRepo{}.UpsertChecksTx(ctx, nil, noFarm, 1, nil, now)
		},
		"ApprovalRepo.LockTicketByOrderSeqTx": func(model.Scope) error {
			_, _, err := ApprovalRepo{}.LockTicketByOrderSeqTx(ctx, nil, noFarm, 1)
			return err
		},
		"ApprovalRepo.LockOrderTx": func(model.Scope) error {
			return ApprovalRepo{}.LockOrderTx(ctx, nil, noFarm)
		},
		"DecisionRepo.DecideTicketTx": func(model.Scope) error {
			return (&DecisionRepo{}).DecideTicketTx(ctx, nil, noFarmTicket, model.TicketApproved, 1, now)
		},
		"DecisionRepo.SetFirstApproverTx": func(model.Scope) error {
			return (&DecisionRepo{}).SetFirstApproverTx(ctx, nil, noFarmTicket, 1, now)
		},
		"DecisionRepo.VoidPreparingTx": func(model.Scope) error {
			_, err := (&DecisionRepo{}).VoidPreparingTx(ctx, nil, noFarmTicket, now)
			return err
		},
		"DecisionRepo.SetDispatchDeadlineTx": func(model.Scope) error {
			return (&DecisionRepo{}).SetDispatchDeadlineTx(ctx, nil, noFarm, now, now)
		},
		"DecisionRepo.InsertDispatchOutboxTx": func(model.Scope) error {
			return (&DecisionRepo{}).InsertDispatchOutboxTx(ctx, nil, noFarmTicket, []byte(`{}`), now)
		},
		"DecisionRepo.InsertRejectedMemoryTx": func(model.Scope) error {
			return (&DecisionRepo{}).InsertRejectedMemoryTx(ctx, nil, noFarmTicket, "x", now)
		},
		"DispatchRepo.InsertPreDispatchChecks": func(model.Scope) error {
			return DispatchRepo{}.InsertPreDispatchChecks(ctx, noFarm, 1, nil, now)
		},
		// P1-L4-A：下发器绑定设备 / 幂等尝试 / 复检退回。
		"DispatchRepo.BindDeviceTx": func(model.Scope) error {
			return DispatchRepo{}.BindDeviceTx(ctx, nil, noFarm, "uav-1", 1, now)
		},
		"DispatchAttemptRepo.ByIdemKey": func(s model.Scope) error {
			_, err := DispatchAttemptRepo{}.ByIdemKey(ctx, nil, s, "k")
			return err
		},
		"ReopenRepo.Ticket": func(s model.Scope) error { _, err := ReopenRepo{}.Ticket(ctx, nil, s, 1); return err },
		"ReopenRepo.SupersedeTicketTx": func(model.Scope) error {
			return ReopenRepo{}.SupersedeTicketTx(ctx, nil, noFarmTicket, now)
		},
		"ReopenRepo.InsertReopenedTicketTx": func(s model.Scope) error {
			_, err := ReopenRepo{}.InsertReopenedTicketTx(ctx, nil,
				model.ApprovalTicket{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}, now, now)
			return err
		},
		"ReopenRepo.InsertNotifyOutboxTx": func(model.Scope) error {
			_, err := ReopenRepo{}.InsertNotifyOutboxTx(ctx, nil, noFarm, "k", []byte(`{}`), now)
			return err
		},
		// P1-L4 审查修复：中止关票 / 条件收尾 run。
		"DecisionRepo.CloseWaitingRunTx": func(s model.Scope) error {
			_, err := (&DecisionRepo{}).CloseWaitingRunTx(ctx, nil, s, 1, "", now)
			return err
		},
		"DecisionRepo.LiveTicketsTx": func(model.Scope) error {
			_, err := (&DecisionRepo{}).LiveTicketsTx(ctx, nil, noFarm)
			return err
		},
		"DecisionRepo.SupersedeLiveTicketTx": func(model.Scope) error {
			return (&DecisionRepo{}).SupersedeLiveTicketTx(ctx, nil, noFarmTicket, now)
		},
		"OutboxRepo.SetDispatchResultTx": func(model.Scope) error {
			return OutboxRepo{}.SetDispatchResultTx(ctx, nil, noFarm, []byte(`{}`), 0, now)
		},
		"RunRepo.FailInterruptedTx": func(model.Scope) error {
			return (&RunRepo{}).FailInterruptedTx(ctx, nil, noFarmRun, model.InterruptType(""), model.ReasonCode(""))
		},
		"WorkOrderRepo.UpdateStatus": func(model.Scope) error {
			return WorkOrderRepo{}.UpdateStatus(ctx, nil, noFarm, model.OrderApproved, model.OrderDone, 0, now)
		},
		// 归属取自实体字段（或 ctx 中的检查点归属）的方法：用被测 Scope 的 tenant / farm 构造入参。
		"ApprovalRepo.InsertTicketTx": func(s model.Scope) error {
			_, _, err := ApprovalRepo{}.InsertTicketTx(ctx, nil, model.ApprovalTicket{TenantID: s.TenantID, FarmID: s.FarmID}, now)
			return err
		},
		"DecisionRepo.InsertDecisionTx": func(s model.Scope) error {
			_, err := (&DecisionRepo{}).InsertDecisionTx(ctx, nil, DecisionRow{TenantID: s.TenantID, FarmID: s.FarmID})
			return err
		},
		"ProfileRepo.UpsertFarm": func(s model.Scope) error {
			return ProfileRepo{}.UpsertFarm(ctx, nil, model.FarmProfile{TenantID: s.TenantID, FarmID: s.FarmID}, now)
		},
		"ProfileRepo.UpsertField": func(s model.Scope) error {
			return ProfileRepo{}.UpsertField(ctx, nil, model.FieldProfile{TenantID: s.TenantID, FarmID: s.FarmID}, now)
		},
		"ProfileRepo.FieldIDByCode": func(s model.Scope) error {
			_, err := ProfileRepo{}.FieldIDByCode(ctx, nil, s.TenantID, s.FarmID, "field-001")
			return err
		},
		"RawStore.Put": func(s model.Scope) error {
			_, err := (&RawStore{}).Put(ctx, model.RawPut{TenantID: s.TenantID, FarmID: s.FarmID})
			return err
		},
		"RunRepo.CreateRun": func(s model.Scope) error {
			_, err := (&RunRepo{}).CreateRun(ctx, model.RunState{TenantID: s.TenantID, FarmID: s.FarmID}, 1, 1, agent.RunMeta{})
			return err
		},
		"WorkOrderRepo.InsertTransition": func(s model.Scope) error {
			return WorkOrderRepo{}.InsertTransition(ctx, nil, model.OrderTransition{Ref: model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}})
		},
		"EventRepo.AppendTx": func(s model.Scope) error {
			_, err := EventRepo{}.AppendTx(ctx, nil, model.RunEvent{TenantID: s.TenantID, FarmID: s.FarmID})
			return err
		},
		"EventSink.Emit": func(s model.Scope) error {
			_, err := EventSink{}.Emit(ctx, model.RunEvent{TenantID: s.TenantID, FarmID: s.FarmID})
			return err
		},
		"ToolCallRepo.Save": func(s model.Scope) error {
			_, err := (&ToolCallRepo{}).Save(ctx, model.ToolCallRow{TenantID: s.TenantID, FarmID: s.FarmID})
			return err
		},
		"CheckpointRepo.Set": func(s model.Scope) error {
			cctx := agent.WithCheckpointTarget(ctx, agent.CheckpointTarget{Scope: s, RunID: 1, Interrupt: model.InterruptWaitApproval})
			return (&CheckpointRepo{}).Set(cctx, "1", []byte(`{}`))
		},
		"DispatchAttemptRepo.InsertSentTx": func(s model.Scope) error {
			_, err := DispatchAttemptRepo{}.InsertSentTx(ctx, nil, model.DispatchAttempt{TenantID: s.TenantID, FarmID: s.FarmID})
			return err
		},
		"DispatchAttemptRepo.SetJobTx": func(s model.Scope) error {
			return DispatchAttemptRepo{}.SetJobTx(ctx, nil, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}, 1, 0,
				model.DeviceReceipt{Status: model.AttemptAccepted, DeviceJobID: "j"}, now, now)
		},
		"DispatchAttemptRepo.UpdateReceiptTx": func(s model.Scope) error {
			return DispatchAttemptRepo{}.UpdateReceiptTx(ctx, nil, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}, 1, 0,
				model.AttemptDone, "", nil, now, now)
		},
		"DispatchAttemptRepo.ByOrder": func(s model.Scope) error {
			_, err := DispatchAttemptRepo{}.ByOrder(ctx, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1})
			return err
		},
		"DispatchAttemptRepo.PendingReconcile": func(s model.Scope) error {
			_, err := DispatchAttemptRepo{}.PendingReconcile(ctx, s, 10)
			return err
		},
		// P1-L4-B 回执侧
		"DeviceReceiptRepo.InsertReceiptTx": func(s model.Scope) error {
			_, err := DeviceReceiptRepo{}.InsertReceiptTx(ctx, nil, ReceiptRow{TenantID: s.TenantID, FarmID: s.FarmID, ReceiptID: "r",
				DeviceJobID: "j", Source: model.SourceWebhook})
			return err
		},
		"DeviceReceiptRepo.ReceiptsByOrder": func(s model.Scope) error {
			_, err := DeviceReceiptRepo{}.ReceiptsByOrder(ctx, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1})
			return err
		},
		"DeviceReceiptRepo.AttemptTx": func(s model.Scope) error {
			_, err := DeviceReceiptRepo{}.AttemptTx(ctx, nil, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}, 1)
			return err
		},
		"DeviceReceiptRepo.LatestAttemptTx": func(s model.Scope) error {
			_, err := DeviceReceiptRepo{}.LatestAttemptTx(ctx, nil, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1})
			return err
		},
		"DeviceReceiptRepo.MarkNoReceiptTx": func(s model.Scope) error {
			return DeviceReceiptRepo{}.MarkNoReceiptTx(ctx, nil, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}, 1, 0, now)
		},
		"DeviceReceiptRepo.SetDoneAreaTx": func(s model.Scope) error {
			return DeviceReceiptRepo{}.SetDoneAreaTx(ctx, nil, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}, 1, now)
		},
		"DeviceReceiptRepo.InsertWorkDoneMemoryTx": func(s model.Scope) error {
			return DeviceReceiptRepo{}.InsertWorkDoneMemoryTx(ctx, nil, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}, 1, "s", now)
		},
		"DeviceReceiptRepo.InsertFieldHistoryOutboxTx": func(s model.Scope) error {
			return DeviceReceiptRepo{}.InsertFieldHistoryOutboxTx(ctx, nil, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}, 1, []byte(`{}`), now)
		},
		"DeviceReceiptRepo.InsertRetryDispatchOutboxTx": func(s model.Scope) error {
			return DeviceReceiptRepo{}.InsertRetryDispatchOutboxTx(ctx, nil, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}, 1, 2, []byte(`{}`), now)
		},
		"DeviceReceiptRepo.InsertCancelOutboxTx": func(s model.Scope) error {
			_, err := DeviceReceiptRepo{}.InsertCancelOutboxTx(ctx, nil, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1}, 1, []byte(`{}`), now)
			return err
		},
		"DeviceReceiptRepo.WorkDone": func(s model.Scope) error {
			_, err := DeviceReceiptRepo{}.WorkDone(ctx, model.OrderRef{TenantID: s.TenantID, FarmID: s.FarmID, OrderID: 1})
			return err
		},
		"CheckpointRepo.Get": func(s model.Scope) error {
			_, _, err := (&CheckpointRepo{}).Get(agent.WithCheckpointTarget(ctx, agent.CheckpointTarget{Scope: s, RunID: 1}), "1")
			return err
		},
	}
}

// TestScopeRequired：§6.2 验收 2「缺 farm_id 调用报错」。仓储零值依赖（nil DB / Clock / tx）：
// 闸门须在任何 DB 访问之前，碰库即 panic，用例记为失败。
func TestScopeRequired(t *testing.T) {
	bad := map[string]model.Scope{"zero": {}, "no farm": {TenantID: 1}, "no tenant": {FarmID: 1}}
	for name, call := range scopeRequiredCalls() {
		for label, s := range bad {
			t.Run(name+"/"+label, func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("touched dependencies before scope check: %v", r)
					}
				}()
				if err := call(s); !errors.Is(err, ErrScopeRequired) {
					t.Fatalf("err = %v, want ErrScopeRequired", err)
				}
			})
		}
	}
}

// TestSystemScopeRequired：扫描方法拒绝未经 NewSystemScope 构造的零值凭证。
func TestSystemScopeRequired(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(0, 0)
	var zero SystemScope
	calls := map[string]func() error{
		"OutboxRepo.Claim":     func() error { _, err := OutboxRepo{}.Claim(ctx, zero, now, now); return err },
		"OutboxRepo.MarkDone":  func() error { _, err := OutboxRepo{}.MarkDone(ctx, zero, 1, 1, now); return err },
		"OutboxRepo.MarkRetry": func() error { _, err := OutboxRepo{}.MarkRetry(ctx, zero, 1, 1, now, "e", now); return err },
		"OutboxRepo.Renew":     func() error { _, err := OutboxRepo{}.Renew(ctx, zero, 1, 1, now, now); return err },
		"OutboxRepo.Defer":     func() error { _, err := OutboxRepo{}.Defer(ctx, zero, 1, 1, now, now); return err },
		"DecisionRepo.ListExpired": func() error {
			_, err := (&DecisionRepo{}).ListExpired(ctx, zero, now)
			return err
		},
		"DecisionRepo.ListDispatchOverdue": func() error {
			_, err := (&DecisionRepo{}).ListDispatchOverdue(ctx, zero, now)
			return err
		},
		"DecisionRepo.ListPreparing": func() error { _, err := (&DecisionRepo{}).ListPreparing(ctx, zero, now); return err },
		"DecisionRepo.ListApprovedInterrupted": func() error {
			_, err := (&DecisionRepo{}).ListApprovedInterrupted(ctx, zero, now)
			return err
		},
		"RunRepo.ListStuck":       func() error { _, err := (&RunRepo{}).ListStuck(ctx, zero, now); return err },
		"RunRepo.ListSystemError": func() error { _, err := (&RunRepo{}).ListSystemError(ctx, zero); return err },
		"DispatchAttemptRepo.ByJob": func() error {
			_, err := DispatchAttemptRepo{}.ByJob(ctx, zero, "uav", "sim-1")
			return err
		},
		"DeviceReceiptRepo.DueAttempts": func() error { _, err := DeviceReceiptRepo{}.DueAttempts(ctx, zero, now); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("touched dependencies before system scope check: %v", r)
				}
			}()
			if err := call(); !errors.Is(err, ErrScopeRequired) {
				t.Fatalf("err = %v, want ErrScopeRequired", err)
			}
		})
	}
	if got := NewSystemScope("outbox_poll").Task(); got != "outbox_poll" {
		t.Fatalf("Task() = %q", got)
	}
}

// scopedParamTypes 是携带归属的入参类型：带其一的导出方法必须在入口校验 tenant_id / farm_id。
var scopedParamTypes = []string{"model.Scope", "model.OrderRef", "TicketRow", "RunKey"}

// TestScopeRequired_Coverage（P1-L1-B 审查 P2-9）：静态列出带归属入参、或在方法体内直接调用归属闸门的导出方法，
// 要求都在 scopeRequiredCalls 中；新增仓储方法忘记补闸门用例时在此失败。持 SystemScope 的扫描方法由 TestSystemScopeRequired 覆盖。
func TestScopeRequired_Coverage(t *testing.T) {
	calls := scopeRequiredCalls()
	var missing []string
	found := 0
	repoFuncs(t, func(_ *token.FileSet, fd *ast.FuncDecl) {
		if fd.Recv == nil || !fd.Name.IsExported() || takesSystemScope(fd) || !(hasScopedParam(fd) || callsScopeGuard(fd.Body)) {
			return
		}
		found++
		if _, ok := calls[methodKey(fd)]; !ok {
			missing = append(missing, methodKey(fd))
		}
	})
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("scoped methods missing from TestScopeRequired: %s", strings.Join(missing, ", "))
	}
	if found < 40 {
		t.Fatalf("only %d scoped methods found; scanner broken?", found)
	}
}

// hasScopedParam 判断方法入参（可带 *）是否含 scopedParamTypes 之一。
func hasScopedParam(fd *ast.FuncDecl) bool {
	for _, p := range fd.Type.Params.List {
		e := p.Type
		if st, ok := e.(*ast.StarExpr); ok {
			e = st.X
		}
		var name string
		switch x := e.(type) {
		case *ast.Ident:
			name = x.Name
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok {
				name = pkg.Name + "." + x.Sel.Name
			}
		}
		if slices.Contains(scopedParamTypes, name) {
			return true
		}
	}
	return false
}

// scopeGuards 是仓储内的归属闸门函数：方法体直接调用其一即视为带归属，须有零 Scope 用例。
var scopeGuards = []string{"requireScope", "requireFarm", "checkpointTarget"}

// callsScopeGuard 判断 n 内是否直接调用 scopeGuards 之一（实体字段带归属的写方法靠它识别）。
func callsScopeGuard(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && slices.Contains(scopeGuards, id.Name) {
				found = true
			}
		}
		return !found
	})
	return found
}

func TestCallsScopeGuard(t *testing.T) {
	tests := []struct {
		src  string
		want bool
	}{
		{`func f(r Row) { if err := requireFarm(r.TenantID, r.FarmID); err != nil { return } }`, true},
		{`func f(s model.Scope) { _ = requireScope(s) }`, true},
		{`func f(r Row) { _ = requireTx(nil); db.Exec("x") }`, false},
		{`func f(r Row) { x.requireFarm(1, 1) }`, false},
	}
	for _, tt := range tests {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+tt.src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := callsScopeGuard(f.Decls[0].(*ast.FuncDecl).Body); got != tt.want {
			t.Errorf("%s: callsScopeGuard = %v, want %v", tt.src, got, tt.want)
		}
	}
}

func TestHasScopedParam(t *testing.T) {
	tests := []struct {
		src  string
		want bool
	}{
		{`func (R) A(ctx context.Context, s model.Scope) {}`, true},
		{`func (R) B(tx *gorm.DB, ref model.OrderRef) {}`, true},
		{`func (R) C(t TicketRow, k *RunKey) {}`, true},
		{`func (R) D(ctx context.Context, id uint64) {}`, false},
		{`func (R) E(sys SystemScope) {}`, false},
	}
	for _, tt := range tests {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+tt.src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := hasScopedParam(f.Decls[0].(*ast.FuncDecl)); got != tt.want {
			t.Errorf("%s: hasScopedParam = %v, want %v", tt.src, got, tt.want)
		}
	}
}
