package callbacks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// rawContentType 是 LLM_IO 对象的 Content-Type。
const rawContentType = "application/json"

// rawDigestPrefix 是 LLM_IO 对象名里的内容摘要前缀长度（同一 step 多次调用按内容区分）。
const rawDigestPrefix = 16

// 错误码（LLMCallPayload.Error，不写错误原文）。
const (
	llmErrUnavailable = "LLM_UNAVAILABLE"
	llmErrRateLimited = "RATE_LIMITED"
	llmErrBadReply    = "BAD_REPLY"
	llmErrCanceled    = "CANCELED"
	llmErrTimeout     = "TIMEOUT"
	llmErrOther       = "LLM_ERROR"
)

// llmIO 是写入 raw_object(LLM_IO) 的原文（user 消息已经过入模脱敏）。
type llmIO struct {
	Tier     llm.Tier      `json:"tier"`
	Task     llm.Task      `json:"task"`
	StepID   string        `json:"step_id,omitempty"`
	Messages []llm.Message `json:"messages"`
	Attempts []llm.Attempt `json:"attempts"`
}

// ObserveLLM 实现 llm.Observer：原文写 raw_object(LLM_IO) → 发 LLM_CALL_END（RawObjectID 关联）。
// 原文落库失败只告警，事件照发（无 raw 关联）；ctx 不带 run 标识时不发。
func (r *Recorder) ObserveLLM(ctx context.Context, c llm.Call) {
	run, ok := writable(ctx)
	if !ok {
		return
	}
	s, _ := stepFrom(ctx)
	rawID := r.putIO(ctx, run, s.id, c)
	p := model.LLMCallPayload{Task: string(c.Task), Model: string(c.Tier), Status: model.LLMStatusOK,
		PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens, LatencyMS: c.Latency.Milliseconds()}
	if c.Err != nil {
		p.Status, p.Error = model.LLMStatusError, llmErrorCode(c.Err)
	}
	body, err := marshal(p)
	if err != nil {
		r.log.WarnContext(ctx, "encode llm payload failed", "run_id", run.RunID, "err", err)
		return
	}
	r.emit(ctx, run, model.RunEvent{StepID: s.id, Type: model.EventLLMCallEnd, Actor: model.ActorAgent,
		Payload: body, RawObjectID: rawID})
	r.tracer.Record(ctx, Span{Kind: SpanLLM, Name: string(c.Task), RunID: run.RunID, StepID: s.id, Status: p.Status,
		LatencyMS: p.LatencyMS, PromptTokens: p.PromptTokens, CompletionTokens: p.CompletionTokens, Error: p.Error})
}

// putIO 把模型输入输出原文写 raw_object；未配置 RawStore 或写入失败返回 nil。
func (r *Recorder) putIO(ctx context.Context, run Run, stepID string, c llm.Call) *uint64 {
	if r.raw == nil {
		return nil
	}
	body, err := json.Marshal(llmIO{Tier: c.Tier, Task: c.Task, StepID: stepID, Messages: c.Messages, Attempts: c.Attempts})
	if err != nil {
		r.log.WarnContext(ctx, "encode llm io failed", "run_id", run.RunID, "err", err)
		return nil
	}
	obj, err := r.raw.Put(context.WithoutCancel(ctx), model.RawPut{TenantID: run.Scope.TenantID, FarmID: run.Scope.FarmID,
		RunID: run.RunID, Kind: model.RawLLMIO, Name: rawName(stepID, c.Task, body), At: r.clk.Now(),
		Body: body, ContentType: rawContentType})
	if err != nil {
		r.log.WarnContext(ctx, "put llm io failed", "run_id", run.RunID, "task", string(c.Task), "err", err)
		return nil
	}
	return &obj.ID
}

// rawName 是 LLM_IO 对象名：llm-{step}-{task}-{内容摘要前缀}（同内容重放幂等，不同内容不复用 key）。
func rawName(stepID string, task llm.Task, body []byte) string {
	if stepID == "" {
		stepID = "none"
	}
	return fmt.Sprintf("llm-%s-%s-%s", stepID, task, digest(body)[:rawDigestPrefix])
}

// llmErrorCode 把模型错误归为码（不含上游错误原文）。
func llmErrorCode(err error) string {
	switch {
	case errors.Is(err, llm.ErrRateLimited):
		return llmErrRateLimited
	case errors.Is(err, llm.ErrBadReply):
		return llmErrBadReply
	case errors.Is(err, context.DeadlineExceeded):
		return llmErrTimeout
	case errors.Is(err, context.Canceled):
		return llmErrCanceled
	case errors.Is(err, llm.ErrUnavailable):
		return llmErrUnavailable
	}
	return llmErrOther
}
