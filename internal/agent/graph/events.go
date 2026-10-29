package graph

// L7 图内事件（P0 §6.8 功能任务 2-3）：RUN_STARTED（Ask 执行体内、Invoke 前）、KB_LOADED（load_knowledge）、
// ADVICE_READY（persist 落库后）。STEP_* / LLM_CALL_END 由 harness/callbacks 发，RUN_FAILED / RUN_INTERRUPTED 由 runner 发。
// payload 用 model 的冻结契约；文本字段经 pii.Mask。

import (
	"context"
	"encoding/json"

	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/model"
)

// emitAs 经 Config.Events 发一条图内事件：e 只需给出 Type / Actor / ActorID / StepID，其余由 s 补齐。
// Events 为空或执行权已丢失（含 runner 外调用，如下发前复检）时跳过；发送失败只告警（事件是观测，不夺执行）。
func (a *Agent) emitAs(ctx context.Context, s *state, e model.RunEvent, payload any) {
	if a.cfg.Events == nil || runner.Alive(ctx) != nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		a.log.WarnContext(ctx, "encode run event", "run_id", s.runID, "type", e.Type, "err", err)
		return
	}
	e.TenantID, e.FarmID, e.RunID, e.Payload, e.CreatedAt = s.q.Scope.TenantID, s.q.Scope.FarmID, s.runID, body,
		a.cfg.Clock.Now()
	if _, err := a.cfg.Events.Emit(context.WithoutCancel(ctx), e); err != nil {
		a.log.WarnContext(ctx, "emit run event", "run_id", s.runID, "type", e.Type, "err", err)
	}
}

// emitRunStarted 发 RUN_STARTED（投影为 ASK）：actor 为提问用户，问题经 pii.Mask。
func (a *Agent) emitRunStarted(ctx context.Context, s *state) {
	a.emitAs(ctx, s, model.RunEvent{Type: model.EventRunStarted, Actor: model.ActorUser, ActorID: s.q.Scope.UserID},
		model.RunStartedPayload{FieldID: s.q.FieldID, Question: pii.Mask(s.q.Text)})
}

// emitKBLoaded 发 KB_LOADED：知识版本与命中条目（kb_id@version）。
func (a *Agent) emitKBLoaded(ctx context.Context, s *state) {
	p := model.KBLoadedPayload{KBRef: a.cfg.KBRef, Entries: make([]string, 0, len(s.hits))}
	for _, h := range s.hits {
		p.Entries = append(p.Entries, h.Ref())
	}
	a.emitAs(ctx, s, model.RunEvent{Type: model.EventKBLoaded, Actor: model.ActorAgent, StepID: NodeKnowledge}, p)
}

// emitAdviceReady 发 ADVICE_READY（投影为 VALIDATE / OK）：建议卡已通过校验并落库（含拒答卡）。
func (a *Agent) emitAdviceReady(ctx context.Context, s *state) {
	a.emitAs(ctx, s, model.RunEvent{Type: model.EventAdviceReady, Actor: model.ActorAgent, StepID: NodePersist},
		model.AdviceReadyPayload{AdviceID: s.card.AdviceID, Outcome: string(s.card.Outcome),
			RefuseReason: s.card.RefuseReason})
}
