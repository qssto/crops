//go:build integration

package app_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/app"
	"agri-steward/internal/config"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P2 L6 重启后续跑（代码 + 注入时钟）：真实 MySQL / Redis 上带 prev_run_id 提问 → 压缩并在审批点中断，
// 换一个新装配的 App（模拟进程重启）续跑，压缩状态从检查点 / agent_run 读回，不重读会话、不重调模型与工具、不重复压缩。

const (
	p2l6Sample    = "rice-pest-0015" // 作业类 → 开票后在 await_approval 中断
	p2l6RawBucket = "raw-it"
	p2l6Turns     = 8
)

var p2l6Scope = model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}

// memObjects 是内存对象存储：RawStore 写入与检查点读回同一份，不连 MinIO。
type memObjects struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (o *memObjects) PutObject(_ context.Context, bucket, key string, body []byte, _ string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.m[bucket+"/"+key] = append([]byte(nil), body...)
	return nil
}

func (o *memObjects) GetObject(_ context.Context, bucket, key string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.m[bucket+"/"+key]
	if !ok {
		return nil, fmt.Errorf("memObjects: %s/%s not found", bucket, key)
	}
	return append([]byte(nil), b...), nil
}

// p2l6App 按 cmd/eval 的单 trial 装配构造 App（桩模型、样本 FixtureMCP、压缩引擎装配且阈值调低）；返回 App 与其工具桩。
func p2l6App(t *testing.T, e *itest.Env, slot *env.Slot, s *sample.Sample, objs *memObjects) (*app.App, *env.FixtureMCP) {
	t.Helper()
	ctx := context.Background()
	root := e.Root
	mcfg, err := config.LoadModel(filepath.Join(root, "configs/model.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	mcfg.Provider, mcfg.PromptsDir = config.ModelProviderStub, filepath.Join(root, "prompts")
	chat, err := app.ChatModel(mcfg)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := knowledge.LoadTaxonomy(filepath.Join(root, "eval/taxonomy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(ctx context.Context, code string) (uint64, error) {
		return repository.ProfileRepo{}.FieldIDByCode(ctx, slot.DB, itest.TenantID, itest.FarmID, code)
	}
	tools, err := env.NewFixtureMCP(ctx, s.Fixtures.MCP, resolve, slot.Clock)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := env.NewRecordingAdapter(s.Fixtures.Device)
	if err != nil {
		t.Fatal(err)
	}
	risk, err := app.LoadRiskMatrix(filepath.Join(root, "policy/risk_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	constraints, err := app.LoadConstraintReviewer(filepath.Join(root, "policy/regulation_map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	perms, err := service.LoadPermissionMatrix(filepath.Join(root, "policy/permission_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	notify, err := service.LoadNotifyTexts(filepath.Join(root, "configs/texts/notify.zh-CN.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cc := e.Cfg.Compaction
	cc.TriggerTokens, cc.KeepRecentTurns, cc.SummaryMaxTokens = 80, 2, 26 // 8 轮种子会话即触发
	comp, err := app.LoadCompaction(cc, filepath.Join(root, app.DefaultLexiconPath))
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(app.Deps{Clock: slot.Clock, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: slot.DB,
		Redis: e.Redis, Keys: slot.Keys, RawStore: repository.NewRawStore(slot.DB, objs, p2l6RawBucket),
		NewAgent: app.GraphAgent(ctx, app.GraphConfig{Model: mcfg, Chat: chat, Taxonomy: tx,
			KB: knowledge.NewStore(knowledge.NewRepoReader(root))}),
		Tools: tools, Risk: risk, Constraints: constraints, Permissions: perms, Adapter: dev, Faults: mustFaults(t, root),
		NotifyTexts: notify, Approval: e.Cfg.Approval,
		HITL: app.HITLDeps{Objects: objs, RawBucket: p2l6RawBucket}, Compaction: comp})
	if err != nil {
		t.Fatal(err)
	}
	return a, tools
}

func p2l6Exec(t *testing.T, db *repository.DB, q string, args ...any) int64 {
	t.Helper()
	res := db.WithContext(t.Context()).Exec(q, args...)
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	return res.RowsAffected
}

// p2l6Session 插一条已结束的会话链（同用户 / 地块，updated_at 在 idle 窗口内）：每轮一条 RUN_STARTED 问题与一张建议卡，返回最后一轮 run_id。
func p2l6Session(t *testing.T, db *repository.DB, fieldID uint64, now time.Time) uint64 {
	t.Helper()
	var prev any
	var last uint64
	for i := 1; i <= p2l6Turns; i++ {
		at := now.Add(-time.Duration(p2l6Turns+1-i) * time.Minute)
		p2l6Exec(t, db, `INSERT INTO agent_run (tenant_id, farm_id, field_id, user_id, status, graph_version, prompt_version,
  model_tier, knowledge_ref, prev_run_id, created_at, updated_at)
  VALUES (?, ?, ?, ?, 'COMPLETED', ?, 'v1', 'standard', 'kb@test', ?, ?, ?)`,
			itest.TenantID, itest.FarmID, fieldID, p2l6Scope.UserID, graph.GraphVersion, prev, at, at)
		last = uint64(itest.Count(t, db, `SELECT MAX(id) FROM agent_run`))
		p2l6Exec(t, db, `INSERT INTO run_event (tenant_id, farm_id, run_id, seq, event_type, actor, payload, created_at)
  VALUES (?, ?, ?, 1, ?, 'user', ?, ?)`, itest.TenantID, itest.FarmID, last, string(model.EventRunStarted),
			fmt.Sprintf(`{"field_id":%d,"question":"第%d轮：稻飞虱发生情况怎么样，最近要不要打药，旁边是蜂场","channel":"web"}`, fieldID, i), at)
		p2l6Exec(t, db, `INSERT INTO advice_card (tenant_id, farm_id, field_id, run_id, crop, phenology_stage, category, advice_type,
  outcome, conclusion, reasons, next_action, schema_ver, created_at)
  VALUES (?, ?, ?, ?, 'rice', 'tillering', 'pest', 'spray', 'ADVICE', ?, '[]', 'NONE', 1, ?)`,
			itest.TenantID, itest.FarmID, fieldID, last, fmt.Sprintf("第%d轮回答：先查虫口密度再定是否用药", i), at)
		prev = last
	}
	return last
}

// p2l6RunState 读 run 的 status / compaction_state（NULL 为空串）/ compaction_count。
func p2l6RunState(t *testing.T, db *repository.DB, runID uint64) (string, string, int64) {
	t.Helper()
	var row struct {
		Status          string
		CompactionState *string
		CompactionCount int64
	}
	if err := db.WithContext(t.Context()).Raw(`SELECT status, compaction_state, compaction_count FROM agent_run WHERE id=?`,
		runID).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.CompactionState == nil {
		return row.Status, "", row.CompactionCount
	}
	return row.Status, *row.CompactionState, row.CompactionCount
}

// p2l6Counts 是一个 run 的副作用计数：压缩原文转存、压缩事件、模型调用、关键节点启动次数。
type p2l6Counts struct {
	compacted, applied, failed, llm int64
	nodes                           map[string]int64
}

func readP2L6Counts(t *testing.T, db *repository.DB, runID uint64) p2l6Counts {
	t.Helper()
	ev := func(typ model.EventType) int64 {
		return itest.Count(t, db, `SELECT COUNT(*) FROM run_event WHERE run_id=? AND event_type=?`, runID, string(typ))
	}
	c := p2l6Counts{
		compacted: itest.Count(t, db, `SELECT COUNT(*) FROM raw_object WHERE run_id=? AND kind=?`, runID,
			string(model.RawCompactedMsgs)),
		applied: ev(model.EventCompactionApplied), failed: ev(model.EventCompactionFailed), llm: ev(model.EventLLMCallEnd),
		nodes: map[string]int64{},
	}
	for _, n := range []string{graph.NodeClassify, graph.NodeChoose, graph.NodeExplain, graph.NodeToOrder} {
		c.nodes[n] = itest.Count(t, db, `SELECT COUNT(*) FROM run_step WHERE run_id=? AND node=?`, runID, n)
	}
	return c
}

// 验收 5 / 技术实现 6（集成 p2l6，代码 + 注入时钟）：
//  1. 进程 1：enable_compaction 开，带 prev_run_id 提问 → 新 run 续接会话链（prev_run_id 落库）、压缩一次（COMPACTED_MSGS 1 份、
//     COMPACTION_APPLIED 1 条），作业单开票后在审批点中断，compaction_state / compaction_count 随中断事务落库；
//  2. 进程 2：新装配的 App（新工具桩、新主链路）在票据通过后 Resume：不重调模型、不调工具、关键节点不重复启动、不再压缩，
//     run 终态 COMPLETED 且 compaction_state 与中断时一致、compaction_count 仍为 1。
func TestCompaction_RestartResume_Integration(t *testing.T) {
	e := itest.Setup(t, "appp2l6")
	all, problems, err := sample.LoadDir(filepath.Join(e.Root, "eval/suites"))
	if err != nil || len(problems) > 0 {
		t.Fatalf("load samples: %v %v", err, problems)
	}
	var s *sample.Sample
	for _, c := range all {
		if c.ID == p2l6Sample {
			s = c
		}
	}
	if s == nil {
		t.Fatalf("sample %s not found", p2l6Sample)
	}
	at, err := time.Parse(time.RFC3339, s.Fixtures.Clock)
	if err != nil {
		t.Fatal(err)
	}
	slot := e.Fresh(t, at)
	db, ctx, now := slot.DB, context.Background(), slot.Clock.Now()
	fieldID, err := repository.ProfileRepo{}.FieldIDByCode(ctx, db, itest.TenantID, itest.FarmID, s.Context.Field)
	if err != nil {
		t.Fatal(err)
	}
	prev := p2l6Session(t, db, fieldID, now)
	p2l6Exec(t, db, `INSERT INTO feature_flag (tenant_id, farm_id, flag_key, scope_type, value, comment, updated_by,
  created_at, updated_at) VALUES (?, ?, ?, ?, '{"on":true}', 'p2l6 it', 0, ?, ?)`,
		itest.TenantID, itest.FarmID, string(model.FlagEnableCompaction), string(model.FlagScopeFarm), now, now)
	objs := &memObjects{m: map[string][]byte{}}

	// 进程 1
	a1, _ := p2l6App(t, e, slot, s, objs)
	runID, err := a1.Ask.Ask(ctx, p2l6Scope, service.AskInput{FieldID: fieldID, Text: evalkit.QuestionOf(s).Text,
		PrevRunID: prev})
	if err != nil {
		t.Fatal(err)
	}
	a1.Ask.Wait()
	if n := itest.Count(t, db, `SELECT COUNT(*) FROM agent_run WHERE id=? AND prev_run_id=?`, runID, prev); n != 1 {
		t.Fatalf("run %d not linked to prev %d", runID, prev)
	}
	status, state, count := p2l6RunState(t, db, runID)
	if status != string(model.RunInterrupted) || state == "" || count != 1 {
		t.Fatalf("after ask status=%s state=%q count=%d, want INTERRUPTED with state, count 1", status, state, count)
	}
	if n := itest.Count(t, db, `SELECT COUNT(*) FROM run_checkpoint WHERE run_id=?`, runID); n == 0 {
		t.Fatal("no checkpoint persisted")
	}
	before := readP2L6Counts(t, db, runID)
	if before.compacted != 1 || before.applied != 1 || before.failed != 0 || before.llm == 0 {
		t.Fatalf("after ask counts = %+v, want 1 compacted / 1 applied / 0 failed / llm > 0", before)
	}
	for n, c := range before.nodes {
		if c != 1 {
			t.Fatalf("node %s started %d times before resume, want 1", n, c)
		}
	}

	// 进程 2（重启）：票据通过后续跑
	if n := p2l6Exec(t, db, `UPDATE approval_ticket SET status='APPROVED' WHERE tenant_id=? AND farm_id=? AND run_id=?
  AND ticket_kind='ORDER' AND status IN ('PREPARING', 'OPEN')`, itest.TenantID, itest.FarmID, runID); n != 1 {
		t.Fatalf("approved %d tickets, want 1", n)
	}
	a2, tools2 := p2l6App(t, e, slot, s, objs)
	r, ok := a2.Agent.(agent.Resumer)
	if !ok {
		t.Fatalf("agent %T is not a Resumer", a2.Agent)
	}
	if _, err := r.Resume(ctx, p2l6Scope, runID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	status, after, count := p2l6RunState(t, db, runID)
	if status != string(model.RunCompleted) || after != state || count != 1 {
		t.Fatalf("after resume status=%s count=%d state changed=%v, want COMPLETED, count 1, same state",
			status, count, after != state)
	}
	if got := readP2L6Counts(t, db, runID); got.compacted != before.compacted || got.applied != before.applied ||
		got.failed != 0 || got.llm != before.llm || fmt.Sprint(got.nodes) != fmt.Sprint(before.nodes) {
		t.Fatalf("resume repeated work: before %+v after %+v", before, got)
	}
	if calls := tools2.Calls(); len(calls) != 0 {
		t.Fatalf("resume called tools %d times, want 0", len(calls))
	}
}
