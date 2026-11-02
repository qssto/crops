//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// newDispatchPoller 经 app.Build 装配真实 MySQL / Redis 上的下发链路（与 cmd/worker 同一份 buildDispatch / NewOutboxPoller：
// DispatchDeps 的设备注册表、dispatch_attempt、幂等键、选设备、resume_epoch 与 dispatch 段默认配置都取生产装配）。
// 未配 HITL 复检（a.HITL.Recheck 为空）：DRY_RUN 单跳过 ④ 只告警、REAL 单失败关闭，复检分支由单测覆盖。
// itTools 不返回设备：本测试的作业单 device_type=DRONE 不在 deviceKinds 内，DRY_RUN 不绑 device_id 照常下发。
func newDispatchPoller(t *testing.T, e *itest.Env, slot *env.Slot, dev adapter.DryRunner) (*service.OutboxPoller,
	*service.WorkOrderService) {
	t.Helper()
	logger, err := app.NewLogger(os.Stderr, "warn")
	if err != nil {
		t.Fatal(err)
	}
	risk, err := policy.LoadRiskMatrix(filepath.Join(e.Root, "policy", "risk_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	constraints, err := app.LoadConstraintReviewer(filepath.Join(e.Root, "policy", "regulation_map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(app.Deps{Clock: slot.Clock, Logger: logger, DB: slot.DB, Redis: e.Redis, Keys: slot.Keys,
		RawStore: &memRaw{}, Agent: itAgent{}, Tools: itTools{}, Risk: risk, Constraints: constraints,
		Permissions: itPerms(t), Adapter: dev, NotifyTexts: itNotifyTexts(t)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.NewOutboxPoller()
	if err != nil {
		t.Fatal(err)
	}
	return p, a.WorkOrders
}

// itNotifyTexts 读仓库内通知文案（P1 L5：outbox 处理器表含 notify.send）。
func itNotifyTexts(t *testing.T) *service.NotifyTexts {
	t.Helper()
	tx, err := service.LoadNotifyTexts("../../configs/texts/notify.zh-CN.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// itAgent / itTools 是 app.Build 的必填桩：下发链路不经主链路，工具调用一律无数据。
type itAgent struct{}

func (itAgent) Ask(context.Context, agent.Question) (agent.Answer, error) {
	return agent.Answer{Outcome: model.OutcomeRefused}, nil
}

type itTools struct{}

func (itTools) Call(context.Context, model.Scope, agent.ToolCall) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

// insertDispatchOutbox 按生产格式（service.DispatchIdemKey，attempt=1）插入一条到期的 order.dispatch，返回 id；
// 下发器校验 dedup_key 与 payload 一致，测试必须用与 DecisionRepo 相同的键。
func insertDispatchOutbox(t *testing.T, db *repository.DB, orderID, ticketID, runID uint64, at time.Time) uint64 {
	t.Helper()
	payload, err := json.Marshal(map[string]uint64{"order_id": orderID, "ticket_id": ticketID, "run_id": runID})
	if err != nil {
		t.Fatal(err)
	}
	return insertOutboxKey(t, db, repository.TopicOrderDispatch, service.DispatchIdemKey(orderID, ticketID, 1), orderID,
		string(payload), 0, at)
}

// insertOutbox 插入一条到期的 PENDING outbox（dedup_key = topic:payload），返回 id。
func insertOutbox(t *testing.T, db *repository.DB, topic string, orderID uint64, payload string, attempts int, at time.Time) uint64 {
	t.Helper()
	return insertOutboxKey(t, db, topic, topic+":"+payload, orderID, payload, attempts, at)
}

func insertOutboxKey(t *testing.T, db *repository.DB, topic, dedup string, orderID uint64, payload string, attempts int,
	at time.Time) uint64 {
	t.Helper()
	if err := db.WithContext(t.Context()).Exec(`INSERT INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload,
  status, attempts, next_run_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 'PENDING', ?, ?, ?, ?)`,
		itest.TenantID, itest.FarmID, topic, orderID, dedup, payload, attempts, at, at, at).Error; err != nil {
		t.Fatal(err)
	}
	return uint64(itest.Count(t, db, "SELECT id FROM outbox WHERE dedup_key = ?", dedup))
}

// §6.9 ①3-①4 端到端：轮询领取 → 下发 dry-run → 作业单 DONE + dispatch_result + 2 条 DISPATCHED + audit DISPATCH；
// 重复投递不再下发；处理失败按 1s 退避，第 5 次失败 DEAD。
func TestOutboxDispatch_Integration(t *testing.T) {
	e := itest.Setup(t, "dispatch")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	poller, svc := newDispatchPoller(t, e, slot, adapter.MockAdapter{})
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "APPROVED", t0)
	if err := slot.DB.WithContext(ctx).Exec(`UPDATE work_order SET dispatch_deadline_at = ? WHERE id = ?`,
		t0.Add(time.Hour), orderID).Error; err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]uint64{"order_id": orderID, "ticket_id": 1, "run_id": runID})
	if err != nil {
		t.Fatal(err)
	}
	outboxID := insertDispatchOutbox(t, slot.DB, orderID, 1, runID, t0)

	if n, err := poller.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	checks := []struct {
		what string
		q    string
		args []any
		want int64
	}{
		{"order DONE with dry-run result", `SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'DONE'
  AND JSON_EXTRACT(dispatch_result, '$.dry_run') = true AND JSON_EXTRACT(dispatch_result, '$.device_touched') = false
  AND JSON_UNQUOTE(JSON_EXTRACT(dispatch_result, '$.adapter')) = 'mock'`, []any{orderID}, 1},
		{"outbox DONE", `SELECT COUNT(*) FROM outbox WHERE id = ? AND status = 'DONE'`, []any{outboxID}, 1},
		{"transitions APPROVED>DISPATCHED>DONE", `SELECT COUNT(*) FROM work_order_transition WHERE order_id = ?
  AND ((from_status = 'APPROVED' AND to_status = 'DISPATCHED') OR (from_status = 'DISPATCHED' AND to_status = 'DONE'))`,
			[]any{orderID}, 2},
		{"DISPATCHED INTENT + RESULT", `SELECT COUNT(*) FROM run_event WHERE run_id = ? AND event_type = 'DISPATCHED'
  AND JSON_EXTRACT(payload, '$.dry_run') = true`, []any{runID}, 2},
		{"audit DISPATCH dry_run", `SELECT COUNT(*) FROM audit_log WHERE run_id = ? AND action = 'DISPATCH'
  AND JSON_EXTRACT(summary, '$.dry_run') = true`, []any{runID}, 2},
		{"no episodic memory", `SELECT COUNT(*) FROM episodic_memory WHERE kind = 'ORDER_DONE' AND ref_id = ?`, []any{orderID}, 0},
	}
	for _, c := range checks {
		if got := itest.Count(t, slot.DB, c.q, c.args...); got != c.want {
			t.Errorf("%s: got %d, want %d", c.what, got, c.want)
		}
	}
	// 重复投递：作业单已终态，直接 nil，不再追加事件
	dup := repository.OutboxMessage{ID: outboxID, TenantID: itest.TenantID, FarmID: itest.FarmID,
		Topic: repository.TopicOrderDispatch, AggregateID: orderID, Payload: payload}
	if err := svc.Dispatch(ctx, dup); err != nil {
		t.Fatalf("duplicate delivery: %v", err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM run_event WHERE run_id = ? AND event_type = 'DISPATCHED'`, runID); n != 2 {
		t.Errorf("DISPATCHED after duplicate = %d, want 2", n)
	}
	if n, err := poller.RunOnce(ctx); err != nil || n != 0 {
		t.Errorf("second RunOnce = %d, %v; want nothing claimable", n, err)
	}

	// 失败退避：未知 topic 第 1 次失败 next_run_at = now+1s；已失败 4 次的再失败转 DEAD
	retryID := insertOutbox(t, slot.DB, "unknown.topic", orderID, `{"n":1}`, 0, slot.Clock.Now())
	deadID := insertOutbox(t, slot.DB, "unknown.topic", orderID, `{"n":2}`, 4, slot.Clock.Now())
	if n, err := poller.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("failure RunOnce = %d, %v", n, err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM outbox WHERE id = ? AND status = 'PENDING' AND attempts = 1
  AND next_run_at = ? AND last_error IS NOT NULL`, retryID, slot.Clock.Now().Add(time.Second)); n != 1 {
		t.Error("first failure not backed off 1s")
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM outbox WHERE id = ? AND status = 'DEAD' AND attempts = 5`, deadID); n != 1 {
		t.Error("fifth failure not DEAD")
	}
}

// SSE 补读：真实 run_event 上按 seq > after 升序；EventService 经 Redis 订阅续推到 RUN_COMPLETED 后 end。
func TestEventStream_Integration(t *testing.T) {
	e := itest.Setup(t, "sse")
	slot := e.Fresh(t, t0)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	logger, err := app.NewLogger(os.Stderr, "warn")
	if err != nil {
		t.Fatal(err)
	}
	events := repository.EventRepo{Pub: repository.RedisPublisher{Client: e.Redis}, Keys: slot.Keys, Log: logger}
	tx := repository.NewDecisionRepo(slot.DB, slot.Clock)
	runID := itest.InsertRun(t, slot.DB, t0)
	appendEvent := func(typ model.EventType) {
		t.Helper()
		if err := tx.InTx(ctx, func(db *repository.DB) error {
			_, err := events.AppendTx(ctx, db, model.RunEvent{TenantID: itest.TenantID, FarmID: itest.FarmID, RunID: runID,
				Type: typ, Actor: model.ActorAgent, Payload: json.RawMessage(`{}`), CreatedAt: slot.Clock.Now()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(model.EventRunStarted)
	appendEvent(model.EventStepStarted)
	appendEvent(model.EventStepEnded)

	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	back := repository.EventBackfillRepo{DB: slot.DB}
	got, err := back.EventsAfter(ctx, scope, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 || got[0].EventType != model.EventStepStarted ||
		got[0].Actor != model.ActorAgent || got[0].RunID != runID {
		t.Fatalf("EventsAfter(1) = %+v", got)
	}
	other := scope
	other.FarmID = itest.FarmID + 1
	if rows, err := back.EventsAfter(ctx, other, runID, 0); err != nil || len(rows) != 0 {
		t.Fatalf("other farm backfill = %d rows, %v", len(rows), err)
	}

	svc := service.NewEventService(service.EventDeps{Subscriber: repository.RedisSubscriber{Client: e.Redis}, Backfill: back,
		Runs: repository.NewRunRepo(slot.DB, slot.Clock), Keys: slot.Keys, Logger: logger})
	sink := &itSink{seen: make(chan int64, 16)}
	done := make(chan error, 1)
	go func() { done <- svc.Stream(ctx, scope, runID, 1, sink) }()
	for _, want := range []int64{2, 3} {
		if seq := <-sink.seen; seq != want {
			t.Fatalf("backfilled seq %d, want %d", seq, want)
		}
	}
	appendEvent(model.EventRunCompleted) // 订阅已生效：经 PUBLISH 实时推送
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if seq := <-sink.seen; seq != 4 || !sink.ended {
		t.Errorf("live seq %d ended %t, want 4 + end", seq, sink.ended)
	}
}

// itSink 把推送的 seq 送进 channel；End 只在 Stream 的 goroutine 内写，Stream 返回后读。
type itSink struct {
	seen  chan int64
	ended bool
}

func (s *itSink) Event(e service.StreamEvent) error { s.seen <- e.Seq; return nil }
func (s *itSink) Ping() error                       { return nil }
func (s *itSink) End() error                        { s.ended = true; return nil }
