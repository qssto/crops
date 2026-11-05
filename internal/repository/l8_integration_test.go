//go:build integration

package repository_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P2 L8 功能任务 1：驳回写 rejection_reason 时同一事务按 advice_citation 取 kb_id 写 kb_ids（去重、升序 JSON 数组）；
// 无引用写 []。应失败：kb_ids 为 NULL、含 measured 引用的空值、重复或乱序、别的农场同 advice_id 的引用混入。
func TestRejectionKbIDs_Integration(t *testing.T) {
	e := itest.Setup(t, "repol8")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.NewDecisionRepo(slot.DB, slot.Clock)
	in := repository.RejectionInput{ReasonCode: model.RejectEvidence, OperatorID: 1002, OperatorRole: model.RoleAgronomist}
	reject := func(row repository.TicketRow) {
		t.Helper()
		if err := repo.InTx(ctx, func(tx *repository.DB) error { return repo.InsertRejectionTx(ctx, tx, row, in, t0) }); err != nil {
			t.Fatal(err)
		}
	}
	cited := itest.InsertRun(t, slot.DB, t0)
	citedRow := l5Ticket(t, repo, slot.DB, cited, itest.InsertOrder(t, slot.DB, cited, "PENDING_APPROVAL", t0), 0, t0)
	for i, c := range []struct{ typ, kb any }{{"kb", "kb:rice/pest/planthopper"}, {"measured", nil},
		{"kb", "kb:common/operation_window/spray"}, {"kb", "kb:rice/pest/planthopper"}} {
		l6Exec(t, slot.DB, `INSERT INTO advice_citation (tenant_id, farm_id, advice_id, seq, claim_path, type, kb_id, kb_version)
  VALUES (?, ?, ?, ?, 'conclusion', ?, ?, 'v1')`, itest.TenantID, itest.FarmID, cited, i, c.typ, c.kb)
	}
	// 别的农场同 advice_id 的引用不得混入
	l6Exec(t, slot.DB, `INSERT INTO advice_citation (tenant_id, farm_id, advice_id, seq, claim_path, type, kb_id, kb_version)
  VALUES (?, ?, ?, 0, 'conclusion', 'kb', 'kb:other/farm', 'v1')`, itest.TenantID, itest.FarmID+1, cited)
	reject(citedRow)
	bare := itest.InsertRun(t, slot.DB, t0)
	bareRow := l5Ticket(t, repo, slot.DB, bare, itest.InsertOrder(t, slot.DB, bare, "PENDING_APPROVAL", t0), 0, t0)
	reject(bareRow)

	kbIDs := func(ticketID uint64) []byte {
		t.Helper()
		var rows []struct{ KbIDs []byte }
		if err := slot.DB.WithContext(ctx).Raw(`SELECT kb_ids FROM rejection_reason WHERE tenant_id=? AND farm_id=? AND ticket_id=?`,
			itest.TenantID, itest.FarmID, ticketID).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].KbIDs == nil {
			t.Fatalf("ticket %d kb_ids rows = %+v, want one non-NULL", ticketID, rows)
		}
		return rows[0].KbIDs
	}
	if got := kbIDs(citedRow.ID); !jsonEqual(t, got, `["kb:common/operation_window/spray", "kb:rice/pest/planthopper"]`) {
		t.Errorf("cited kb_ids = %s", got)
	}
	if got := kbIDs(bareRow.ID); !jsonEqual(t, got, `[]`) {
		t.Errorf("uncited kb_ids = %s, want []", got)
	}
}

// P2 L8 功能任务 2（隔离、幂等红线）：run_id 只取建议卡；他场建议卡 → ErrNotFound；同 request_id 重放回首次结果且不改写；
// 他场已用、或同场他人 / 另一建议卡已用的 request_id → 同一个 ErrFeedbackRequestConflict，且不回首次的行。
func TestFeedbackRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repol8fb")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.FeedbackRepo{DB: slot.DB}
	own := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	other := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID + 1, UserID: 2001}
	runID := itest.InsertRun(t, slot.DB, t0)
	adviceID := p2l6Card(t, slot.DB, runID, "OK", "注意稻飞虱", "", "", t0)

	if got, err := repo.AdviceRun(ctx, own, adviceID); err != nil || got != runID {
		t.Fatalf("AdviceRun own = %d, %v; want %d", got, err, runID)
	}
	if _, err := repo.AdviceRun(ctx, other, adviceID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("AdviceRun other farm err = %v, want ErrNotFound", err)
	}

	dosage := model.RejectDosage
	first := model.AdviceFeedback{AdviceID: adviceID, RunID: runID, UserID: 1001, Verdict: model.FeedbackDislike,
		Category: &dosage, RequestID: "fb-l8-1", CreatedAt: t0}
	got, created, err := repo.InsertFeedback(ctx, own, first)
	if err != nil || !created || got.ID == 0 || got.RunID != runID || got.Category == nil || *got.Category != dosage {
		t.Fatalf("first insert = %+v created=%v err=%v", got, created, err)
	}
	replay := first
	replay.Verdict, replay.Category, replay.CreatedAt = model.FeedbackAdopt, nil, t0.Add(time.Hour)
	again, created, err := repo.InsertFeedback(ctx, own, replay)
	if err != nil || created || again.ID != got.ID || again.Verdict != model.FeedbackDislike || !again.CreatedAt.Equal(t0) {
		t.Fatalf("replay = %+v created=%v err=%v, want first row unchanged", again, created, err)
	}
	crossFarm, _, crossErr := repo.InsertFeedback(ctx, other, replay)
	if !errors.Is(crossErr, model.ErrFeedbackRequestConflict) || crossFarm.ID != 0 {
		t.Fatalf("other farm same request_id = %+v, %v; want ErrFeedbackRequestConflict, no row", crossFarm, crossErr)
	}
	// 同农场他人 / 另一建议卡重放同一 request_id：一律冲突、不回首次行，错误与跨农场一致（不借此区分两种情况）。
	otherUser := replay
	otherUser.UserID = 1002
	otherAdvice := replay
	otherAdvice.AdviceID = adviceID + 1
	for name, fb := range map[string]model.AdviceFeedback{"other user": otherUser, "other advice": otherAdvice} {
		row, _, err := repo.InsertFeedback(ctx, own, fb)
		if !errors.Is(err, model.ErrFeedbackRequestConflict) || row.ID != 0 || err.Error() != crossErr.Error() {
			t.Fatalf("%s replay = %+v, %v; want same conflict as cross farm (%v), no row", name, row, err, crossErr)
		}
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM advice_feedback WHERE request_id=?`, "fb-l8-1"); n != 1 {
		t.Fatalf("advice_feedback rows = %d, want 1", n)
	}
}

// P2 L8 功能任务 3（隔离红线）：WeekHits 只取本农场 [from, to) 的五类信号；采纳、非判错专家分、窗口外、他场行不得混入；
// 主 kb 取 seq 最小的 kb 引用；专家判错按 advice_id 去重（多维度 × 多 judge_version 只算一次）。
func TestAttributionWeekHits_Integration(t *testing.T) {
	e := itest.Setup(t, "repol8hit")
	slot := e.Fresh(t, t0)
	db, ctx := slot.DB, t.Context()
	from, to := t0, t0.Add(7*24*time.Hour)
	in := t0.Add(time.Hour)

	run := itest.InsertRun(t, db, in)
	card := p2l6Card(t, db, run, "OK", "注意稻飞虱", "", "", in)
	for i, kb := range []string{"kb:rice/pest/planthopper", "kb:common/operation_window/spray"} {
		l6Exec(t, db, `INSERT INTO advice_citation (tenant_id, farm_id, advice_id, seq, claim_path, type, kb_id, kb_version)
  VALUES (?, ?, ?, ?, 'conclusion', 'kb', ?, 'v1')`, itest.TenantID, itest.FarmID, card, i+1, kb)
	}
	reject := func(farm, ticket uint64, at time.Time) {
		l6Exec(t, db, `INSERT INTO rejection_reason (tenant_id, farm_id, ticket_id, order_id, advice_id, run_id, reason_code,
  operator_id, operator_role, created_at) VALUES (?, ?, ?, 1, ?, ?, 'DOSAGE', 1002, 'agronomist', ?)`,
			itest.TenantID, farm, ticket, card, run, at)
	}
	reject(itest.FarmID, 1, in)
	reject(itest.FarmID, 2, to)   // 窗口外（右开）
	reject(itest.FarmID+1, 3, in) // 他场
	feedback := func(farm uint64, verdict model.FeedbackVerdict, category any, req string) {
		l6Exec(t, db, `INSERT INTO advice_feedback (tenant_id, farm_id, advice_id, run_id, user_id, verdict, category, request_id,
  created_at) VALUES (?, ?, ?, ?, 1001, ?, ?, ?, ?)`, itest.TenantID, farm, card, run, verdict, category, req, in)
	}
	feedback(itest.FarmID, model.FeedbackDislike, model.RejectTiming, "hit-1")
	feedback(itest.FarmID, model.FeedbackAdopt, nil, "hit-2")
	feedback(itest.FarmID+1, model.FeedbackDislike, model.RejectTiming, "hit-3")
	for _, reason := range []string{"VALIDATION_FAILED", "KB_MISS", "NO_DATA"} {
		r := itest.InsertRun(t, db, in)
		c := p2l6Card(t, db, r, "REFUSED", "无法给出", "", "", in)
		l6Exec(t, db, `UPDATE advice_card SET refuse_reason = ? WHERE id = ?`, reason, c)
	}
	judge := func(score int, status, version string) {
		l6Exec(t, db, `INSERT INTO online_judge_result (tenant_id, farm_id, run_id, advice_id, stat_date, category, dimension,
  judge_version, judge_score, expert_score, status, created_at, updated_at)
  VALUES (?, ?, ?, ?, '2026-07-15', 'pest', ?, ?, 2, ?, ?, ?, ?)`, itest.TenantID, itest.FarmID, run, card,
			fmt.Sprintf("dim-%d-%s", score, status), version, score, status, in, in)
	}
	judge(1, "REVIEWED", "j1")
	judge(1, "REVIEWED", "j2") // 同一建议卡另一 judge_version 判错：按 advice_id 去重，仍只算一次
	l6Exec(t, db, `INSERT INTO online_judge_result (tenant_id, farm_id, run_id, advice_id, stat_date, category, dimension,
  judge_version, judge_score, expert_score, status, created_at, updated_at)
  VALUES (?, ?, ?, ?, '2026-07-15', 'pest', 'other-dim', 'j1', 2, 1, 'REVIEWED', ?, ?)`, itest.TenantID, itest.FarmID, run, card,
		in, in.Add(time.Minute)) // 同一建议卡另一维度判错：同上
	judge(3, "REVIEWED", "j1")
	judge(1, "NEED_EXPERT", "j1")

	hits, err := repository.AttributionRepo{DB: db}.WeekHits(ctx, model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]model.AttributionHit{}
	for _, h := range hits {
		if h.FarmID != itest.FarmID || got[h.Source].Source != "" {
			t.Fatalf("unexpected hit %+v", h)
		}
		got[h.Source] = h
	}
	want := map[string]string{model.EvidenceRejection: string(model.RejectDosage), model.EvidenceFeedback: string(model.RejectTiming),
		model.EvidenceValidation: model.AttrReasonGuardrail, model.EvidenceKBMiss: model.AttrReasonKBMiss,
		model.EvidenceExpert: model.AttrReasonExpertWrong}
	if len(got) != len(want) {
		t.Fatalf("hits = %+v, want one per source %v", hits, want)
	}
	for src, reason := range want {
		if got[src].Reason != reason || got[src].QuestionType != "pest" {
			t.Errorf("%s hit = %+v, want reason %s question_type pest", src, got[src], reason)
		}
	}
	if kb := got[model.EvidenceRejection].PrimaryKB; kb != "kb:rice/pest/planthopper" {
		t.Errorf("rejection primary kb = %q", kb)
	}
	if kb := got[model.EvidenceKBMiss].PrimaryKB; kb != "" {
		t.Errorf("kb_miss primary kb = %q, want empty", kb)
	}
}

// P2 L8 功能任务 3 / 4（乐观锁红线）：平台行 0/0、跨周一签名一行；RESOLVED 后再命中 → OPEN 且 reopen_count+1
// （未关闭再命中不加）；PATCH row_ver 过期 → ErrConcurrentUpdate，不存在 → ErrNotFound。
func TestAttributionIssues_Integration(t *testing.T) {
	e := itest.Setup(t, "repol8issue")
	slot := e.Fresh(t, t0)
	db, ctx := slot.DB, t.Context()
	repo := repository.AttributionRepo{DB: db}
	sys := repository.NewSystemScope("attribution-weekly")
	dosage := string(model.RejectDosage)
	sig := model.Signature(dosage, "pest", "kb:a")
	upsert := func(week string, hits int, at time.Time) model.AttributionIssue {
		t.Helper()
		ev := []model.EvidenceRef{{RunID: uint64(hits), Source: model.EvidenceRejection, SourceID: uint64(hits), Week: week}}
		if err := repo.UpsertIssues(ctx, sys, []model.IssueUpsert{{Signature: sig, Week: week, Reason: dosage,
			QuestionType: "pest", PrimaryKB: "kb:a", HitCount: hits, Evidence: ev}}, at); err != nil {
			t.Fatal(err)
		}
		got, err := repo.IssuesBySignature(ctx, sys, []string{sig})
		if err != nil || len(got) != 1 {
			t.Fatalf("IssuesBySignature = %+v, %v", got, err)
		}
		return got[0]
	}
	i := upsert("2026W29", 2, t0)
	if i.Status != model.IssueOpen || i.FirstWeek != "2026W29" || i.HitCount != 2 || i.ReopenCount != 0 || len(i.Evidence) != 1 {
		t.Fatalf("first upsert = %+v", i)
	}
	if n := itest.Count(t, db, `SELECT COUNT(*) FROM attribution_issue WHERE tenant_id = 0 AND farm_id = 0`); n != 1 {
		t.Fatalf("platform rows = %d, want 1", n)
	}

	s := func(v string) *string { return &v }
	n := func(v int) *int { return &v }
	resolved, err := repo.PatchIssue(ctx, sys, i.ID, model.AttributionPatch{Disposition: s(model.DispositionAddKB),
		ResolutionRef: s("kb PR #1"), Status: s(model.IssueResolved), RowVer: n(i.RowVer)}, t0.Add(time.Hour))
	if err != nil || resolved.Status != model.IssueResolved || resolved.RowVer != i.RowVer+1 || *resolved.Disposition != model.DispositionAddKB {
		t.Fatalf("patch = %+v, %v", resolved, err)
	}
	if _, err := repo.PatchIssue(ctx, sys, i.ID, model.AttributionPatch{Status: s(model.IssueOpen), RowVer: n(i.RowVer)},
		t0); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("stale row_ver err = %v, want ErrConcurrentUpdate", err)
	}
	if _, err := repo.PatchIssue(ctx, sys, i.ID+999, model.AttributionPatch{Status: s(model.IssueOpen), RowVer: n(0)},
		t0); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing id err = %v, want ErrNotFound", err)
	}

	again := upsert("2026W30", 5, t0.Add(7*24*time.Hour))
	if again.Status != model.IssueOpen || again.ReopenCount != 1 || again.FirstWeek != "2026W29" || again.LastWeek != "2026W30" ||
		again.HitCount != 5 || *again.Disposition != model.DispositionAddKB {
		t.Fatalf("reopen = %+v, want OPEN reopen_count=1 first_week kept", again)
	}
	still := upsert("2026W31", 1, t0.Add(14*24*time.Hour))
	if still.ReopenCount != 1 || still.Status != model.IssueOpen {
		t.Fatalf("open re-hit = %+v, want reopen_count unchanged", still)
	}
	if n := itest.Count(t, db, `SELECT COUNT(*) FROM attribution_issue WHERE signature = ?`, sig); n != 1 {
		t.Fatalf("rows for signature = %d, want 1", n)
	}
	top, err := repo.WeekIssues(ctx, sys, "2026W31", 10)
	if err != nil || len(top) != 1 || top[0].ID != i.ID {
		t.Fatalf("WeekIssues = %+v, %v", top, err)
	}
	if old, err := repo.WeekIssues(ctx, sys, "2026W29", 10); err != nil || len(old) != 0 {
		t.Fatalf("WeekIssues old week = %+v, %v", old, err)
	}
}
