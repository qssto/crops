package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// transcript 三层结构（P0 L7 功能任务 6）：每行一个 JSON 对象，按 root → agent_step → atomic_step 先序输出。
//
//	root        一个 trial 的 run（sample / trial / run_id / outcome / status）
//	agent_step  run 内一个图节点（step_id = 节点名；step_id 为空的 run 级事件归到 step_id=""）
//	atomic_step 节点内的一次工具调用（tool_call）、模型调用（llm_call）或其余事件（event，引用 timeline 的 seq）
const (
	LayerRoot   = "root"
	LayerStep   = "agent_step"
	LayerAtomic = "atomic_step"

	KindToolCall = "tool_call"
	KindLLMCall  = "llm_call"
	KindEvent    = "event"

	// TranscriptContentType 是上传 MinIO 的内容类型。
	TranscriptContentType = "application/x-ndjson"
)

// TranscriptLine 是 JSONL 的一行；ID / ParentID 串起三层（root 的 ParentID 为空）。
type TranscriptLine struct {
	Layer    string          `json:"layer"`
	ID       string          `json:"id"`
	ParentID string          `json:"parent_id,omitempty"`
	Kind     string          `json:"kind,omitempty"`
	Data     json.RawMessage `json:"data"`
}

// TranscriptKey 是 trial transcript 在 eval bucket 内的对象键：transcript/{exp_id}/{sample_id}/{trial}.jsonl。
func TranscriptKey(expID string, t evalkit.Trial) string {
	return path.Join("transcript", expID, t.SampleID, strconv.Itoa(t.TrialIndex)+".jsonl")
}

// rootData 是 root 行的数据。
type rootData struct {
	ExpID      string                   `json:"exp_id"`
	SampleID   string                   `json:"sample_id"`
	TrialIndex int                      `json:"trial"`
	RunID      uint64                   `json:"run_id"`
	TenantID   uint64                   `json:"tenant_id"`
	FarmID     uint64                   `json:"farm_id"`
	Expected   string                   `json:"expected"`
	Outcome    string                   `json:"outcome"`
	Status     string                   `json:"status"`
	Error      string                   `json:"error,omitempty"`
	Run        model.RunSummary         `json:"run"`
	Question   *model.RunStartedPayload `json:"question,omitempty"`
	Warnings   []string                 `json:"warnings,omitempty"`
	Approvals  []service.TicketDetail   `json:"approvals"`
	Execution  service.Execution        `json:"execution"`
}

// stepData 是 agent_step 行的数据（不含子调用，子调用各占一行 atomic_step）。
type stepData struct {
	StepID    string `json:"step_id"`
	Node      string `json:"node"`
	Status    string `json:"status"`
	ToolCalls int    `json:"tool_calls"`
	LLMCalls  int    `json:"llm_calls"`
	Events    int    `json:"events"`
}

// BuildTranscript 把一个 trial 的 run 详情展开为三层 JSONL；trial 没有 Detail（无 run 或无 trial 库）时返回 nil。
func BuildTranscript(expID string, t evalkit.Trial) ([]byte, error) {
	d := t.Detail
	if d == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	emit := func(layer, id, parent, kind string, data any) error {
		raw, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("report: transcript %s %s: %w", layer, id, err)
		}
		return enc.Encode(TranscriptLine{Layer: layer, ID: id, ParentID: parent, Kind: kind, Data: raw})
	}
	rootID := "run:" + strconv.FormatUint(t.RunID, 10)
	root := rootData{ExpID: expID, SampleID: t.SampleID, TrialIndex: t.TrialIndex, RunID: t.RunID,
		TenantID: t.Scope.TenantID, FarmID: t.Scope.FarmID, Expected: string(t.Expected), Outcome: string(t.Outcome),
		Status: t.Status(), Error: t.Error, Run: d.Run, Question: d.Question, Warnings: d.Warnings,
		Approvals: d.Approvals, Execution: d.Execution}
	if err := emit(LayerRoot, rootID, "", "", root); err != nil {
		return nil, err
	}
	events := make(map[int]repository.EventRow, len(d.Timeline))
	for _, e := range d.Timeline {
		events[e.Seq] = e
	}
	for _, s := range d.Steps {
		stepID := rootID + "/step:" + s.StepID
		if err := emit(LayerStep, stepID, rootID, "", stepData{StepID: s.StepID, Node: s.Node, Status: s.Status,
			ToolCalls: len(s.ToolCalls), LLMCalls: len(s.LLMCalls), Events: len(s.EventSeqs)}); err != nil {
			return nil, err
		}
		for _, c := range s.ToolCalls {
			if err := emit(LayerAtomic, fmt.Sprintf("%s/tool:%d", stepID, c.ID), stepID, KindToolCall, c); err != nil {
				return nil, err
			}
		}
		for _, l := range s.LLMCalls {
			if err := emit(LayerAtomic, fmt.Sprintf("%s/llm:%d", stepID, l.EventSeq), stepID, KindLLMCall, l); err != nil {
				return nil, err
			}
		}
		for _, seq := range s.EventSeqs {
			if err := emit(LayerAtomic, fmt.Sprintf("%s/event:%d", stepID, seq), stepID, KindEvent, events[seq]); err != nil {
				return nil, err
			}
		}
	}
	return buf.Bytes(), nil
}

// ObjectPutter 是对象存储写接缝（repository.MinIOPutter 实现）。
type ObjectPutter interface {
	PutObject(ctx context.Context, bucket, key string, body []byte, contentType string) error
}

// UploadTranscripts 把每个有 Detail 的 trial 的 transcript 上传到 bucket，并把对象键写回 Trial.Transcript，
// 返回上传条数。没有 Detail 的 trial 跳过；任一上传失败即返回错误（已上传的 trial 仍带键）。
func UploadTranscripts(ctx context.Context, obj ObjectPutter, bucket string, exp *evalkit.Experiment) (int, error) {
	n := 0
	for i := range exp.Trials {
		t := &exp.Trials[i]
		body, err := BuildTranscript(exp.ID, *t)
		if err != nil {
			return n, err
		}
		if body == nil {
			continue
		}
		key := TranscriptKey(exp.ID, *t)
		if err := obj.PutObject(ctx, bucket, key, body, TranscriptContentType); err != nil {
			return n, fmt.Errorf("report: upload transcript %s: %w", key, err)
		}
		t.Transcript = key
		n++
	}
	return n, nil
}
