//go:build integration

package repository_test

import (
	"errors"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 验收 2 / 3：EventSink 短事务写 run_event 时同事务写 audit_log 投影，提交后 PUBLISH sse:run:{run_id}；
// 非审计事件不投影；业务事务回滚时 run_event / audit_log 都不落、也不发布。
func TestEventAuditAndSSE_Integration(t *testing.T) {
	e := itest.Setup(t, "repol7")
	slot := e.Fresh(t, t0)
	runID := itest.InsertRun(t, slot.DB, t0)
	repo := repository.EventRepo{Pub: repository.RedisPublisher{Client: e.Redis}, Keys: slot.Keys}
	sink := repository.EventSink{DB: slot.DB, Events: repo}
	sub := e.Redis.Subscribe(t.Context(), slot.Keys.SSERun(runID))
	defer func() {
		if err := sub.Close(); err != nil {
			t.Errorf("close subscription: %v", err)
		}
	}()
	if _, err := sub.Receive(t.Context()); err != nil { // 等订阅确认，避免丢首条
		t.Fatal(err)
	}
	ev := func(typ model.EventType, payload string) model.RunEvent {
		return model.RunEvent{TenantID: itest.TenantID, FarmID: itest.FarmID, RunID: runID, Type: typ,
			Actor: model.ActorAgent, StepID: "enrich", Payload: []byte(payload), CreatedAt: t0}
	}
	if _, err := sink.Emit(t.Context(), ev(model.EventToolCallEnd, `{"tool":"weather.forecast","status":"OK"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Emit(t.Context(), ev(model.EventStepStarted, `{"node":"enrich"}`)); err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE run_id=? AND action='TOOL_CALL'
		AND target='tool:weather.forecast' AND result='OK' AND CHAR_LENGTH(payload_digest)=64`, runID); n != 1 {
		t.Fatalf("TOOL_CALL audit rows = %d, want 1", n)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM audit_log WHERE run_id=?", runID); n != 1 {
		t.Fatalf("audit rows = %d, want 1 (STEP_STARTED not projected)", n)
	}
	for range 2 {
		msg, err := sub.ReceiveMessage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if msg.Payload == "" {
			t.Fatal("empty SSE message")
		}
	}

	errAbort := errors.New("abort")
	err := repository.Transactor{DB: slot.DB}.InTx(t.Context(), func(tx *repository.DB) error {
		if _, err := repo.AppendTx(t.Context(), tx, ev(model.EventValidationFailed, `{"check":"dose"}`)); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("tx err = %v", err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM audit_log WHERE run_id=? AND action='VALIDATE'", runID); n != 0 {
		t.Fatalf("rolled-back audit rows = %d", n)
	}
	select {
	case m := <-sub.Channel():
		t.Fatalf("rolled-back event published: %s", m.Payload)
	case <-time.After(300 * time.Millisecond):
	}

	if _, err := sink.Emit(t.Context(), ev(model.EventToolCallEnd, `not json`)); err == nil {
		t.Fatal("bad payload must fail")
	}
}

// 分区 REORGANIZE：pmax 拆出下一月（已存在的月不重复建），新 pmax 仍在最后；pmax 计数可读。
func TestPartitionRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repol7")
	slot := e.Fresh(t, t0)
	repo := repository.PartitionRepo{DB: slot.DB}
	for _, table := range []string{repository.TableAuditLog, repository.TableRunEvent} {
		names, err := repo.Partitions(t.Context(), table)
		if err != nil {
			t.Fatal(err)
		}
		if len(names) < 2 || names[len(names)-1] != "pmax" {
			t.Fatalf("%s partitions = %v", table, names)
		}
		last, err := time.Parse("p200601", names[len(names)-2])
		if err != nil {
			t.Fatal(err)
		}
		next := last.AddDate(0, 1, 0)
		if err := repo.SplitMax(t.Context(), table, []time.Time{next}); err != nil {
			t.Fatal(err)
		}
		after, err := repo.Partitions(t.Context(), table)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(names)+1 || after[len(after)-2] != repository.PartitionName(next) || after[len(after)-1] != "pmax" {
			t.Fatalf("%s after split = %v", table, after)
		}
		if _, err := repo.CountMax(t.Context(), table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.Partitions(t.Context(), "agent_run"); err == nil {
		t.Fatal("non-whitelisted table must be rejected")
	}
}
