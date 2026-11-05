//go:build integration

package repository_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/minio/minio-go/v7"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// reviewEvalDBName 是 P2-L2b 集成测试专用的隔离评测库（设了 AS_TEST_NS 时加命名空间前缀），每次开测清空两张表。
const reviewEvalDBName = "agri_eval_it_reviewl2b"

// reviewEvalDB 建隔离评测库、迁到 migrations/eval 最新版、清空 review_task / trial，返回连接。
func reviewEvalDB(t *testing.T) *repository.DB {
	t.Helper()
	root, err := itest.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "configs/app.yaml"), filepath.Join(root, "deploy/.env"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.RequireSecrets("mysql.password"); err != nil {
		t.Fatal(err)
	}
	name := reviewEvalDBName
	if ns := os.Getenv(env.NamespaceEnv); ns != "" {
		name = ns + "_" + name
	}
	ctx := context.Background()
	if err := repository.CreateDatabase(ctx, cfg.MySQL.DSN("", false), name); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Migrate(cfg.MySQL.DSN(name, true), filepath.Join(root, "migrations", "eval")); err != nil {
		t.Fatal(err)
	}
	logger, err := app.NewLogger(os.Stderr, "warn")
	if err != nil {
		t.Fatal(err)
	}
	db, err := app.OpenMySQL(ctx, cfg, name, clock.System{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(db); err != nil {
			t.Errorf("close eval db: %v", err)
		}
	})
	for _, q := range []string{"DELETE FROM review_task", "DELETE FROM trial"} {
		l6Exec(t, db, q)
	}
	return db
}

func reviewInsertTrial(t *testing.T, db *repository.DB, expID uint64, sample string, idx int, suite string) {
	t.Helper()
	l6Exec(t, db, `INSERT INTO trial (experiment_id, sample_id, trial_index, suite, status, created_at)
  VALUES (?, ?, ?, ?, 'PASS', ?)`, expID, sample, idx, suite, t0)
}

func reviewTaskID(t *testing.T, db *repository.DB, key string) uint64 {
	t.Helper()
	var id uint64
	if err := db.WithContext(t.Context()).Raw("SELECT id FROM review_task WHERE ref_key = ?", key).Scan(&id).Error; err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatalf("review task %s not inserted", key)
	}
	return id
}

// P2-L2b 红线：holdout 失败关闭（找不到 trial 也按 holdout）；入队按 ref_key 去重；Claim / Complete 可重放，
// 他人持有 / 不同分数重放 / 主标双标同一专家一律 ErrReviewConflict。
func TestEvalReviewRepo_Integration(t *testing.T) {
	db := reviewEvalDB(t)
	ctx := t.Context()
	sys := repository.NewSystemScope("review-l2b-it")
	repo := repository.EvalReviewRepo{DB: db}
	exp, idx := uint64(7), 0
	reviewInsertTrial(t, db, exp, "s-open", idx, "regression")
	reviewInsertTrial(t, db, exp, "s-hold", idx, "holdout")
	task := func(sample string, rater int) model.ReviewTask {
		return model.ReviewTask{TaskType: model.ReviewHumanQueue, QueueReason: "UNKNOWN", ExperimentID: &exp, SampleID: sample,
			TrialIndex: &idx, Dimension: "fact", JudgeVersion: "j1", RaterNo: rater}
	}
	tasks := []model.ReviewTask{task("s-open", model.RaterPrimary), task("s-open", model.RaterSecond),
		task("s-hold", model.RaterPrimary), task("s-missing", model.RaterPrimary),
		{TaskType: model.ReviewOnlineJudge, DraftRef: model.OnlineJudgeRef(1, 1, 9), Dimension: "fact", JudgeVersion: "j1"}}
	n, err := repo.InsertReviewTasks(ctx, sys, tasks, t0)
	if err != nil || n != len(tasks) {
		t.Fatalf("insert = %d, %v", n, err)
	}
	if n, err := repo.InsertReviewTasks(ctx, sys, tasks, t0); err != nil || n != 0 { // 重跑不重复派单
		t.Fatalf("re-insert = %d, %v", n, err)
	}

	wantHoldout := map[string]bool{"s-open": false, "s-hold": true, "s-missing": true}
	for sample, want := range wantHoldout {
		got, err := repo.GetReviewTask(ctx, sys, reviewTaskID(t, db, task(sample, model.RaterPrimary).Key()))
		if err != nil || got.Holdout != want {
			t.Fatalf("%s holdout = %v (%v), want %v", sample, got.Holdout, err, want)
		}
		if _, found, err := repo.TrialSummary(ctx, sys, got); err != nil || found == want {
			t.Fatalf("%s trial summary found = %v (%v)", sample, found, err)
		}
	}
	if got, err := repo.GetReviewTask(ctx, sys, reviewTaskID(t, db, tasks[4].Key())); err != nil || got.Holdout {
		t.Fatalf("online judge task holdout = %v (%v)", got.Holdout, err)
	}
	list, total, err := repo.ListReviewTasks(ctx, sys, model.ReviewTaskFilter{TaskType: model.ReviewHumanQueue, Limit: 2})
	if err != nil || total != 4 || len(list) != 2 {
		t.Fatalf("list = %d/%d, %v", len(list), total, err)
	}
	if _, err := repo.GetReviewTask(ctx, sys, 1<<40); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing task err = %v", err)
	}

	const me, other = 9002, 9003
	primary := reviewTaskID(t, db, tasks[0].Key())
	second := reviewTaskID(t, db, tasks[1].Key())
	if _, err := repo.ClaimReviewTask(ctx, sys, primary, me); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClaimReviewTask(ctx, sys, primary, other); !errors.Is(err, repository.ErrReviewConflict) {
		t.Fatalf("claim held task err = %v", err)
	}
	if _, err := repo.ClaimReviewTask(ctx, sys, second, me); !errors.Is(err, repository.ErrReviewConflict) {
		t.Fatalf("same expert on both raters err = %v", err)
	}
	two, three := 2, 3
	if err := repo.CompleteReviewTask(ctx, sys, primary, me, &two, t0); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteReviewTask(ctx, sys, primary, me, &two, t0); err != nil { // 同分重放
		t.Fatalf("replay err = %v", err)
	}
	if err := repo.CompleteReviewTask(ctx, sys, primary, me, &three, t0); !errors.Is(err, repository.ErrReviewConflict) {
		t.Fatalf("different-score replay err = %v", err)
	}
	c, err := repo.ClaimReviewTask(ctx, sys, primary, me)
	if err != nil || !c.Done || c.Task.ExpertScore == nil || *c.Task.ExpertScore != two {
		t.Fatalf("claim done task = %+v, %v", c, err)
	}
	if _, err := repo.ClaimReviewTask(ctx, sys, second, other); err != nil { // 双标换人可以
		t.Fatal(err)
	}
	// 释放：只认 ASSIGNED 且 assignee 是本人；SKIPPED 后不可再认领、不可再释放
	if err := repo.SkipReviewTask(ctx, sys, second, me, t0); !errors.Is(err, repository.ErrReviewConflict) {
		t.Fatalf("skip task held by other err = %v", err)
	}
	if err := repo.SkipReviewTask(ctx, sys, primary, me, t0); !errors.Is(err, repository.ErrReviewConflict) {
		t.Fatalf("skip done task err = %v", err)
	}
	if err := repo.SkipReviewTask(ctx, sys, second, other, t0); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.GetReviewTask(ctx, sys, second); err != nil || got.Status != model.ReviewSkipped || got.DoneAt == nil {
		t.Fatalf("skipped task = %+v, %v", got, err)
	}
	if _, err := repo.ClaimReviewTask(ctx, sys, second, other); !errors.Is(err, repository.ErrReviewConflict) {
		t.Fatalf("claim skipped task err = %v", err)
	}
	if err := repo.SkipReviewTask(ctx, sys, second, other, t0); !errors.Is(err, repository.ErrReviewConflict) {
		t.Fatalf("skip twice err = %v", err)
	}
}

// P2-L2b 红线：业务库回写带 tenant_id / farm_id 条件——别的农场同 id 的行不受影响（ErrJudgeRowConflict），
// 审计与回写同事务：冲突时不留审计；成功时 REVIEW_SUBMIT 记在本农场链上。
func TestReviewJudgeResultAudited_Integration(t *testing.T) {
	e := itest.Setup(t, "reviewl2b")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.QualityRepo{DB: slot.DB}
	own := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	otherFarm := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID + 1}
	run := itest.InsertRun(t, slot.DB, t0)
	one := 1
	row := model.OnlineJudgeResult{TenantID: own.TenantID, FarmID: own.FarmID, RunID: run, AdviceID: 501, StatDate: "2026-07-15",
		Category: "pest", Dimension: "fact", JudgeVersion: "j1", JudgeScore: &one, Status: model.JudgeStatusNeedExpert}
	if _, err := repo.InsertJudgeResult(ctx, own, row, t0); err != nil {
		t.Fatal(err)
	}
	theirs := row
	theirs.FarmID, theirs.AdviceID = otherFarm.FarmID, 502
	if _, err := repo.InsertJudgeResult(ctx, otherFarm, theirs, t0); err != nil {
		t.Fatal(err)
	}
	need, err := repo.NeedExpertResults(ctx, own, 0, 10)
	if err != nil || len(need) != 1 || need[0].AdviceID != 501 {
		t.Fatalf("need expert = %+v, %v", need, err)
	}
	target, err := repo.JudgeResult(ctx, otherFarm, need[0].ID)
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("cross-farm read = %+v, %v", target, err)
	}
	actor := model.Actor{Type: model.ActorUser, ID: 9002}
	audit := func(s model.Scope) model.AuditRecord {
		return repository.NewAuditRecord(s, nil, actor, model.AuditReviewSubmit, "review:1", []byte(`{"score":3}`),
			model.AuditResultOK, t0)
	}
	// 用本农场的 id 冒充别的农场回写：UPDATE 条件带 farm_id，影响 0 行
	if err := repo.ReviewJudgeResultAudited(ctx, otherFarm, need[0].ID, 0, 3, 9002, audit(otherFarm), t0); !errors.Is(err,
		repository.ErrJudgeRowConflict) {
		t.Fatalf("cross-farm writeback err = %v", err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM audit_log WHERE action = ?", model.AuditReviewSubmit); n != 0 {
		t.Fatalf("audit rows after rolled-back writeback = %d", n)
	}
	if err := repo.ReviewJudgeResultAudited(ctx, own, need[0].ID, 0, 3, 9002, audit(own), t0); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReviewJudgeResultAudited(ctx, own, need[0].ID, 0, 3, 9002, audit(own), t0); !errors.Is(err,
		repository.ErrJudgeRowConflict) { // 旧 row_ver 再写：乐观锁冲突
		t.Fatalf("stale row_ver err = %v", err)
	}
	got, err := repo.JudgeResult(ctx, own, need[0].ID)
	if err != nil || got.Status != model.JudgeStatusReviewed || got.ExpertScore == nil || *got.ExpertScore != 3 ||
		got.ExpertID != 9002 || got.RowVer != 1 {
		t.Fatalf("reviewed row = %+v, %v", got, err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM online_judge_result WHERE tenant_id = ? AND farm_id = ?
  AND status = 'NEED_EXPERT' AND expert_score IS NULL`, otherFarm.TenantID, otherFarm.FarmID); n != 1 {
		t.Fatalf("other farm rows touched: untouched = %d", n)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM audit_log WHERE tenant_id = ? AND farm_id = ? AND action = ?",
		own.TenantID, own.FarmID, model.AuditReviewSubmit); n != 1 {
		t.Fatalf("farm-chain REVIEW_SUBMIT audits = %d", n)
	}
}

// P2-L2b 红线（P1：draft_ref 可被评测库写权限伪造）：评审回写只认 NEED_EXPERT 行——SCORED 行即便 row_ver 对得上也
// 影响 0 行（ErrJudgeRowConflict），行不变、不留审计；跨 scope 指向（别的农场的 scope + 本行 id）同样拒绝。
func TestReviewJudgeResultAudited_OnlyNeedExpert_Integration(t *testing.T) {
	e := itest.Setup(t, "reviewl2b")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.QualityRepo{DB: slot.DB}
	own := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	otherFarm := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID + 1}
	run := itest.InsertRun(t, slot.DB, t0)
	two := 2
	scored := model.OnlineJudgeResult{TenantID: own.TenantID, FarmID: own.FarmID, RunID: run, AdviceID: 601, StatDate: "2026-07-15",
		Category: "pest", Dimension: "fact", JudgeVersion: "j1", JudgeScore: &two, Status: model.JudgeStatusScored}
	if _, err := repo.InsertJudgeResult(ctx, own, scored, t0); err != nil {
		t.Fatal(err)
	}
	var id uint64
	if err := slot.DB.WithContext(ctx).Raw("SELECT id FROM online_judge_result WHERE advice_id = ?", 601).Scan(&id).Error; err != nil {
		t.Fatal(err)
	}
	actor := model.Actor{Type: model.ActorUser, ID: 9002}
	audit := func(s model.Scope) model.AuditRecord {
		return repository.NewAuditRecord(s, nil, actor, model.AuditReviewSubmit, "review:1", []byte(`{"score":3}`),
			model.AuditResultOK, t0)
	}
	for name, s := range map[string]model.Scope{"scored": own, "cross scope": otherFarm} {
		if err := repo.ReviewJudgeResultAudited(ctx, s, id, 0, 3, 9002, audit(s), t0); !errors.Is(err,
			repository.ErrJudgeRowConflict) {
			t.Fatalf("%s writeback err = %v, want ErrJudgeRowConflict", name, err)
		}
	}
	got, err := repo.JudgeResult(ctx, own, id)
	if err != nil || got.Status != model.JudgeStatusScored || got.ExpertScore != nil || got.ExpertID != 0 || got.RowVer != 0 {
		t.Fatalf("scored row changed: %+v, %v", got, err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM audit_log WHERE action = ?", model.AuditReviewSubmit); n != 0 {
		t.Fatalf("audit rows after rejected writeback = %d", n)
	}
}

// P2-4：UPDATE 命中 1 行但同事务封审计失败（删掉本农场 audit_chain_head，sealAudit 报 ErrAuditChainHeadMissing）——
// 整个事务回滚：业务行保持 NEED_EXPERT、row_ver 0、无专家分，不留审计。防止「只断言 UPDATE 0 行」的用例假通过。
func TestReviewJudgeResultAudited_SealFailRollsBack_Integration(t *testing.T) {
	e := itest.Setup(t, "reviewl2b")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.QualityRepo{DB: slot.DB}
	own := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	run := itest.InsertRun(t, slot.DB, t0)
	one := 1
	row := model.OnlineJudgeResult{TenantID: own.TenantID, FarmID: own.FarmID, RunID: run, AdviceID: 701, StatDate: "2026-07-15",
		Category: "pest", Dimension: "fact", JudgeVersion: "j1", JudgeScore: &one, Status: model.JudgeStatusNeedExpert}
	if _, err := repo.InsertJudgeResult(ctx, own, row, t0); err != nil {
		t.Fatal(err)
	}
	need, err := repo.NeedExpertResults(ctx, own, 0, 10)
	if err != nil || len(need) != 1 {
		t.Fatalf("need expert = %+v, %v", need, err)
	}
	const headQ = "SELECT COUNT(*) FROM audit_chain_head WHERE tenant_id = ? AND farm_id = ?"
	if n := itest.Count(t, slot.DB, headQ, own.TenantID, own.FarmID); n != 1 { // 确认删除前链头在，失败确由删除造成
		t.Fatalf("farm chain head rows before delete = %d", n)
	}
	l6Exec(t, slot.DB, "DELETE FROM audit_chain_head WHERE tenant_id = ? AND farm_id = ?", own.TenantID, own.FarmID)
	rec := repository.NewAuditRecord(own, nil, model.Actor{Type: model.ActorUser, ID: 9002}, model.AuditReviewSubmit,
		"review:1", []byte(`{"score":3}`), model.AuditResultOK, t0)
	if err := repo.ReviewJudgeResultAudited(ctx, own, need[0].ID, 0, 3, 9002, rec, t0); !errors.Is(err,
		repository.ErrAuditChainHeadMissing) {
		t.Fatalf("seal-fail writeback err = %v, want ErrAuditChainHeadMissing", err)
	}
	got, err := repo.JudgeResult(ctx, own, need[0].ID)
	if err != nil || got.Status != model.JudgeStatusNeedExpert || got.ExpertScore != nil || got.ExpertID != 0 || got.RowVer != 0 {
		t.Fatalf("row changed although audit seal failed: %+v, %v", got, err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM audit_log WHERE action = ?", model.AuditReviewSubmit); n != 0 {
		t.Fatalf("audit rows after rolled-back writeback = %d", n)
	}
}

// P2-3：入队续扫游标存在 Redis cursor:review_enqueue:{tenant}:{farm}，带 TTL；不存在读 0，删除后读 0；按农场隔离。
func TestReviewEnqueueCursor_Integration(t *testing.T) {
	e := itest.Setup(t, "reviewl2b")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	cur := repository.NewReviewEnqueueCursor(e.Redis, slot.Keys)
	own := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	other := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID + 1}
	if after, err := cur.EnqueueCursor(ctx, own); err != nil || after != 0 {
		t.Fatalf("missing cursor = %d, %v", after, err)
	}
	if err := cur.SetEnqueueCursor(ctx, own, 4242); err != nil {
		t.Fatal(err)
	}
	if after, err := cur.EnqueueCursor(ctx, own); err != nil || after != 4242 {
		t.Fatalf("cursor = %d, %v", after, err)
	}
	if after, err := cur.EnqueueCursor(ctx, other); err != nil || after != 0 {
		t.Fatalf("other farm cursor = %d, %v", after, err)
	}
	ttl, err := e.Redis.TTL(ctx, slot.Keys.ReviewEnqueueCursor(own.TenantID, own.FarmID)).Result()
	if err != nil || ttl <= 0 || ttl > rediskey.ReviewEnqueueCursorTTL {
		t.Fatalf("cursor ttl = %v, %v", ttl, err)
	}
	if err := cur.ResetEnqueueCursor(ctx, own); err != nil {
		t.Fatal(err)
	}
	if after, err := cur.EnqueueCursor(ctx, own); err != nil || after != 0 {
		t.Fatalf("reset cursor = %d, %v", after, err)
	}
}

// P2-2：BACKFLOW_EXPECT 结论走条件写（If-None-Match: *）——对象已存在返回 ErrObjectExists 且不覆盖。
// 用本地 MinIO 的 eval 桶，键带 AS_TEST_NS 前缀，测前测后删除；本地 MinIO 版本不执行该头时此用例失败（见 dev-log 风险）。
func TestQualityObjects_PutObjectIfAbsent_Integration(t *testing.T) {
	root, err := itest.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "configs/app.yaml"), filepath.Join(root, "deploy/.env"))
	if err != nil {
		t.Fatal(err)
	}
	mc, err := app.NewMinIO(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	bucket, key := cfg.MinIO.BucketEval, "it/review-l2b/put-if-absent.json"
	if ns := os.Getenv(env.NamespaceEnv); ns != "" {
		key = "it/" + ns + "/review-l2b/put-if-absent.json"
	}
	remove := func() {
		if err := mc.RemoveObject(context.Background(), bucket, key, minio.RemoveObjectOptions{}); err != nil {
			t.Errorf("remove %s/%s: %v", bucket, key, err)
		}
	}
	remove()
	t.Cleanup(remove)
	objs := repository.QualityObjects{Client: mc}
	if err := objs.PutObjectIfAbsent(ctx, bucket, key, []byte(`{"decision":"MERGE"}`), "application/json"); err != nil {
		t.Fatal(err)
	}
	if err := objs.PutObjectIfAbsent(ctx, bucket, key, []byte(`{"decision":"DROP"}`), "application/json"); !errors.Is(err,
		repository.ErrObjectExists) {
		t.Fatalf("second put-if-absent err = %v, want ErrObjectExists", err)
	}
	got, err := objs.GetObject(ctx, bucket, key)
	if err != nil || string(got) != `{"decision":"MERGE"}` {
		t.Fatalf("stored = %s, %v (first writer must win)", got, err)
	}
}
