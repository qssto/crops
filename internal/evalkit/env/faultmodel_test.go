package env

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"

	"agri-steward/internal/agent/llm"
	"agri-steward/internal/evalkit/sample"
)

func taskMsgs(task llm.Task) []*schema.Message {
	return []*schema.Message{schema.SystemMessage("sys"),
		schema.UserMessage(`{"task":"` + string(task) + `","input":{"question":"稻飞虱要不要打药"}}`)}
}

// 模型桩故障：声明的任务每次调用都返回 ErrModelDown（应失败：声明不可用仍回复），未声明的任务透传桩回复；all 覆盖全部任务。
func TestFaultModel_Generate(t *testing.T) {
	tests := []struct {
		name     string
		fx       *sample.ModelFixture
		task     llm.Task
		wantDown bool
	}{
		{name: "no fixture passes through", task: llm.TaskClassify},
		{name: "declared task down", fx: &sample.ModelFixture{Unavailable: []string{"classify"}}, task: llm.TaskClassify,
			wantDown: true},
		{name: "other task passes", fx: &sample.ModelFixture{Unavailable: []string{"explain"}}, task: llm.TaskClassify},
		{name: "all down", fx: &sample.ModelFixture{Unavailable: []string{sample.ModelTaskAll}}, task: llm.TaskClassify,
			wantDown: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewFaultModel(llm.Stub{}, tt.fx)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 { // 每次调用都失败，不是只失败一次
				out, err := m.Generate(context.Background(), taskMsgs(tt.task))
				if tt.wantDown {
					if !errors.Is(err, ErrModelDown) || out != nil {
						t.Fatalf("out=%v err=%v, want ErrModelDown", out, err)
					}
					continue
				}
				if err != nil || out == nil || out.Content == "" {
					t.Fatalf("out=%v err=%v, want stub reply", out, err)
				}
			}
		})
	}
	if _, err := NewFaultModel(llm.Stub{}, &sample.ModelFixture{Unavailable: []string{"summarize"}}); err == nil {
		t.Fatal("want error for unknown task")
	}
}
