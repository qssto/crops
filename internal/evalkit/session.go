package evalkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P2 L6 多轮会话样本（fixtures.session）：提问前把各轮写成 trial 库里已结束的 run 链，再按生产路径
// （AskService.CreateRun，enable_compaction 开时续接、关时与 P1 相同不续接）建本次 run，交给被测 agent 同步执行；
// 归还槽位前按种子 run_id 独立查询业务表（env.ReadSessionTruth，不复用生产 LoadSession）并读本次 run 的
// LLM_IO 原始对象，供 compaction_retained 打分。

// userRole 是 LLM_IO 原始对象中 user 消息的 role（eino schema.User）。
const userRole = "user"

// rawObjects 是 LLM_IO 原始对象的读取来源（与 App.RawStore 同一 bucket）。
type rawObjects struct {
	getter repository.ObjectGetter
	bucket string
}

// WithRawObjects 设置 raw 对象读取来源（cmd/eval 传 MinIOGetter 与 minio.bucket_raw）。未配置时多轮会话样本
// 的 SessionObs.Prompts 为 nil，compaction_retained 记 grader_error。
func WithRawObjects(getter repository.ObjectGetter, bucket string) RunnerOption {
	return func(r *Runner) { r.raw = &rawObjects{getter: getter, bucket: bucket} }
}

// prepareSession 为声明 fixtures.session 的样本播种会话链并建本次 run，把 run_id 写入 q.RunID；
// 返回种子链各轮 run_id（由早到晚，末项为本次 prev_run_id；未声明 fixtures.session 时返回 nil，q 不动）。
func prepareSession(ctx context.Context, a *app.App, s *sample.Sample, q *agent.Question) ([]uint64, error) {
	if s.Fixtures.Session == nil {
		return nil, nil
	}
	if a.DB == nil || a.Ask == nil || a.Clock == nil {
		return nil, errors.New("session fixture requires App.DB, App.Ask and App.Clock")
	}
	seeded, err := env.SeedSession(ctx, a.DB, env.SessionSeed{Scope: q.Scope, FieldID: q.FieldID, Crop: s.Crop,
		KnowledgeRef: s.KnowledgeRef, Now: a.Clock.Now(), Fixture: s.Fixtures.Session})
	if err != nil {
		return nil, err
	}
	runID, err := a.Ask.CreateRun(ctx, q.Scope, service.AskInput{FieldID: q.FieldID, Text: q.Text,
		PrevRunID: seeded[len(seeded)-1]})
	if err != nil {
		return nil, fmt.Errorf("session run: %w", err)
	}
	q.RunID = runID
	return seeded, nil
}

// recordSession 读多轮会话观测（seeded 为空即样本无 fixtures.session 时不动）：真值按种子 run_id 独立查询
// （env.ReadSessionTruth）；Linked 看本次 run 的 prev_run_id 是否为种子链末轮。须在 recordDetail 之后调用。
func (r *Runner) recordSession(ctx context.Context, t *Trial, a *app.App, scope model.Scope, seeded []uint64, runID uint64) error {
	if len(seeded) == 0 || runID == 0 {
		return nil
	}
	truth, err := env.ReadSessionTruth(ctx, a.DB, scope, seeded, runID)
	if err != nil {
		return fmt.Errorf("record session: %w", err)
	}
	obs := &grader.SessionObs{Linked: truth.Linked, Source: truth.Source, Superseded: truth.Superseded}
	if a.Flags != nil {
		if obs.Enabled, err = a.Flags.Bool(ctx, model.FlagEnableCompaction, scope); err != nil {
			return fmt.Errorf("record session flag: %w", err)
		}
	}
	if obs.Prompts, err = r.userPrompts(ctx, a, scope, t.Detail); err != nil {
		return fmt.Errorf("record session prompts: %w", err)
	}
	t.Session = obs
	return nil
}

// userPrompts 读本次 run 每次模型调用的 user 消息（按 RunDetail 步骤与调用顺序）；未配置 WithRawObjects 或无详情时为 nil。
func (r *Runner) userPrompts(ctx context.Context, a *app.App, scope model.Scope, d *service.RunDetail) ([]string, error) {
	if r.raw == nil || d == nil {
		return nil, nil
	}
	reader := repository.RawReader{DB: a.DB, Obj: r.raw.getter, Bucket: r.raw.bucket}
	prompts := []string{}
	for _, st := range d.Steps {
		for _, c := range st.LLMCalls {
			if c.RawObjectID == nil {
				continue
			}
			body, err := reader.Get(ctx, scope, *c.RawObjectID)
			if err != nil {
				return nil, err
			}
			var io struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(body, &io); err != nil {
				return nil, fmt.Errorf("decode raw_object %d: %w", *c.RawObjectID, err)
			}
			for _, m := range io.Messages {
				if m.Role == userRole {
					prompts = append(prompts, m.Content)
				}
			}
		}
	}
	return prompts, nil
}
