package llm

import (
	"context"
	"time"

	"github.com/cloudwego/eino/schema"
)

// Observer 是模型调用钩子（实现：internal/agent/harness/callbacks.Recorder.ObserveLLM）。
type Observer func(ctx context.Context, c Call)

// Message 是发往模型的一条消息（user 消息已脱敏）。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Attempt 是一次尝试的原始回复与错误。
type Attempt struct {
	Reply string `json:"reply,omitempty"`
	Error string `json:"error,omitempty"`
	Usage Usage  `json:"usage"`
}

// Call 是一次 Do 的完整记录：Messages / Attempts 是模型输入输出原文，Usage / Latency / Err 为汇总。
type Call struct {
	Tier     Tier
	Task     Task
	Messages []Message
	Attempts []Attempt
	Usage    Usage
	Latency  time.Duration
	Err      error
}

// callRecord 是一次 Do 的进行中记录；未配置 Observer 时为 nil，各方法空操作。
type callRecord struct {
	call  Call
	start time.Time
}

func (c *Client) begin(tier Tier, task Task, msgs []*schema.Message) *callRecord {
	if c.observe == nil {
		return nil
	}
	r := &callRecord{call: Call{Tier: tier, Task: task, Messages: make([]Message, 0, len(msgs))}, start: c.clock.Now()}
	for _, m := range msgs {
		r.call.Messages = append(r.call.Messages, Message{Role: string(m.Role), Content: m.Content})
	}
	return r
}

func (r *callRecord) add(reply string, u Usage, err error) {
	if r == nil {
		return
	}
	a := Attempt{Reply: reply, Usage: u}
	if err != nil {
		a.Error = err.Error()
	}
	r.call.Attempts = append(r.call.Attempts, a)
}

// end 汇总并回调 Observer（同步执行；Observer 自行处理失败，不影响调用结果）。
func (c *Client) end(ctx context.Context, r *callRecord, total Usage, err error) {
	if r == nil {
		return
	}
	r.call.Usage, r.call.Err, r.call.Latency = total, err, c.clock.Now().Sub(r.start)
	c.observe(ctx, r.call)
}
