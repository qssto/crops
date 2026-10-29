package runner

import (
	"context"
	"encoding/json"

	"agri-steward/internal/model"
)

// ReasonExecError 是执行体报错（非业务拒答）时 RUN_FAILED 的缺省原因码。
const ReasonExecError = "EXEC_ERROR"

func failReason(code string) string {
	if code == "" {
		return ReasonExecError
	}
	return code
}

// emitEnd 发 run 终态事件（RUN_FAILED / RUN_INTERRUPTED）；未配置 Events 时跳过，失败只告警（终态已落库）。
// 只在仍持有执行权时调用（execute 已校验 guard）；ctx 取消不影响写入。
func (r *Runner) emitEnd(ctx context.Context, scope model.Scope, runID uint64, typ model.EventType, p model.RunEndPayload) {
	if r.events == nil {
		return
	}
	body, err := json.Marshal(p)
	if err != nil {
		r.log.Warn("encode run end payload failed", "run_id", runID, "event_type", string(typ), "err", err)
		return
	}
	if _, err := r.events.Emit(context.WithoutCancel(ctx), model.RunEvent{TenantID: scope.TenantID, FarmID: scope.FarmID,
		RunID: runID, Type: typ, Actor: model.ActorSystem, Payload: body, CreatedAt: r.clk.Now()}); err != nil {
		r.log.Warn("emit run end event failed", "run_id", runID, "event_type", string(typ), "err", err)
	}
}
