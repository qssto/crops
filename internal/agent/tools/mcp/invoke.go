package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"agri-steward/internal/model"
)

// invoke 调 Server：单次超时 t.timeout，read 且幂等的工具在 TIMEOUT / UPSTREAM 时重试；
// 错误归一为 TIMEOUT / EMPTY / MALFORMED / UPSTREAM / REJECTED。error 只表示调用方 ctx 已取消。
func (g *Gateway) invoke(ctx context.Context, t *tool, args json.RawMessage) (outcome, error) {
	var o outcome
	for i := range t.tries() {
		res, err := invokeOnce(ctx, t, args)
		if cerr := ctx.Err(); cerr != nil {
			return outcome{}, cerr
		}
		if err == nil {
			return t.checkOutput(res), nil
		}
		o = normalize(err)
		if !retryable(o.code) {
			return o, nil
		}
		g.opt.Logger.WarnContext(ctx, "mcp tool retry", "tool", t.spec.Name, "attempt", i+1, "error_code", o.code, "err", o.msg)
	}
	return o, nil
}

// invokeOnce 在独立超时下调用一次；Server 不响应 ctx 时也按超时返回（其 goroutine 在 Server 返回后退出）。
func invokeOnce(ctx context.Context, t *tool, args json.RawMessage) (Result, error) {
	actx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	type reply struct {
		res Result
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		res, err := t.server.Invoke(actx, t.spec.Name, args)
		ch <- reply{res, err}
	}()
	select {
	case r := <-ch:
		return r.res, r.err
	case <-actx.Done():
		return Result{}, actx.Err()
	}
}

// normalize 把 Server 错误归一：*ToolError 原样，超时为 TIMEOUT，其余为 UPSTREAM。
func normalize(err error) outcome {
	var te *ToolError
	switch {
	case errors.As(err, &te):
		return failed(te.Code, fromInvoke, "%s", te.Msg)
	case errors.Is(err, context.DeadlineExceeded):
		return failed(ErrTimeout, fromInvoke, "%v", err)
	default:
		return failed(ErrUpstream, fromInvoke, "%v", err)
	}
}

func retryable(code ErrorCode) bool { return code == ErrTimeout || code == ErrUpstream }

// checkOutput 校验 Result.Data 是 JSON 对象且符合 OutputSchema，否则 MALFORMED。
func (t *tool) checkOutput(res Result) outcome {
	dec := json.NewDecoder(bytes.NewReader(res.Data))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil || data == nil {
		return failed(ErrMalformed, fromInvoke, "data is not a JSON object")
	}
	if dec.More() {
		return failed(ErrMalformed, fromInvoke, "trailing data after JSON object")
	}
	if t.output != nil {
		if err := t.output.Validate(data); err != nil {
			return failed(ErrMalformed, fromInvoke, "data violates output schema: %v", err)
		}
	}
	return outcome{res: res, from: fromInvoke}
}

// statusOf 把错误码映射为 tool_call.status。
func statusOf(code ErrorCode) string {
	switch code {
	case "":
		return model.ToolStatusOK
	case ErrTimeout:
		return model.ToolStatusTimeout
	case ErrEmpty:
		return model.ToolStatusEmpty
	case ErrRejected:
		return model.ToolStatusRejected
	default:
		return model.ToolStatusError
	}
}
