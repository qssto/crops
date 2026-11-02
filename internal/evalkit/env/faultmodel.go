package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"agri-steward/internal/agent/llm"
	"agri-steward/internal/evalkit/sample"
)

// ErrModelDown 是 FaultModel 对声明不可用的任务返回的错误（模拟超时 / 5xx，重试后由 llm.Client 包装为 ErrUnavailable）。
var ErrModelDown = errors.New("fault model: model unavailable")

// FaultModel 包装模型桩（P1 L6）：样本 fixtures.model.unavailable 列出的任务每次调用都返回 ErrModelDown，
// 其余任务透传给 inner。任务名取最后一条 user 消息 {"task": ...}（与 llm.Client 的 JSON 任务约定一致）。
type FaultModel struct {
	inner einomodel.BaseChatModel
	down  map[llm.Task]bool
	all   bool
}

var _ einomodel.BaseChatModel = (*FaultModel)(nil)

// NewFaultModel 按 fixtures.model 包装 inner；fx 为 nil 时原样返回 inner。未知任务名返回错误。
func NewFaultModel(inner einomodel.BaseChatModel, fx *sample.ModelFixture) (einomodel.BaseChatModel, error) {
	if fx == nil {
		return inner, nil
	}
	if inner == nil {
		return nil, fmt.Errorf("fault model: inner model is required")
	}
	m := &FaultModel{inner: inner, down: map[llm.Task]bool{}}
	for _, t := range fx.Unavailable {
		switch task := llm.Task(t); task {
		case llm.TaskClassify, llm.TaskChoose, llm.TaskExplain:
			m.down[task] = true
		default:
			if t != sample.ModelTaskAll {
				return nil, fmt.Errorf("fault model: unknown task %q", t)
			}
			m.all = true
		}
	}
	return m, nil
}

// Generate 对不可用任务返回 ErrModelDown，否则透传。
func (m *FaultModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	if task := lastTask(msgs); m.all || m.down[task] {
		return nil, fmt.Errorf("%w: task %s", ErrModelDown, task)
	}
	return m.inner.Generate(ctx, msgs, opts...)
}

// Stream 以单帧流返回 Generate 的结果。
func (m *FaultModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	out, err := m.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{out}), nil
}

// lastTask 取最后一条 user 消息的 task；无法解析时返回空串（不按任务注入，all 仍生效）。
func lastTask(msgs []*schema.Message) llm.Task {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i] == nil || msgs[i].Role != schema.User {
			continue
		}
		var env struct {
			Task llm.Task `json:"task"`
		}
		if err := json.Unmarshal([]byte(msgs[i].Content), &env); err != nil {
			return ""
		}
		return env.Task
	}
	return ""
}
