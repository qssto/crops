//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1-L7-A 审计哈希链集成测试：封链往返、多链加锁顺序（配 -race）、缺 head 回滚、SENSITIVE 二次留痕同事务回滚。

var errBusiness = errors.New("business write failed after audit queued")

// alertRec 记录封链失败告警。
type alertRec struct {
	mu  sync.Mutex
	got []model.Alert
}

func (r *alertRec) Fire(_ context.Context, a model.Alert) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, a)
}

func (r *alertRec) count(name model.AlertName) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, a := range r.got {
		if a.Name == name {
			n++
		}
	}
	return n
}

func chainRec(farm uint64, action string, i int) model.AuditRecord {
	summary := json.RawMessage(fmt.Sprintf(`{"问题": "稻飞虱", "剂量": 1.50, "i": %d, "嵌套": {"b": [1, 2e0], "a": null}}`, i))
	return model.AuditRecord{TenantID: itest.TenantID, FarmID: farm, Actor: model.Actor{Type: model.ActorUser, ID: 1001},
		Action: action, Target: fmt.Sprintf("field:%d", i), PayloadDigest: repository.Digest(summary), Summary: summary,
		Result: model.AuditResultOK, CreatedAt: t0.Add(123456789)}
}

// checkChain 读回整条链并逐行重算：序号连续、prev_hash 衔接、row_hash 一致、head 指向最后一行。
func checkChain(t *testing.T, db *repository.DB, k repository.ChainKey) []repository.ChainRow {
	t.Helper()
	r := repository.AuditChainRepo{DB: db}
	h, err := r.Head(t.Context(), k)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := r.Rows(t.Context(), k, 1, h.ChainSeq)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(rows)) != h.ChainSeq {
		t.Fatalf("chain %+v: %d rows, head seq %d", k, len(rows), h.ChainSeq)
	}
	prev := repository.GenesisHash
	for i, row := range rows {
		got, err := repository.AuditRowHash(row.PrevHash, row)
		if err != nil {
			t.Fatal(err)
		}
		if row.ChainSeq != uint64(i+1) || row.PrevHash != prev || got != row.RowHash {
			t.Fatalf("chain %+v row %d (id %d): seq %d prev ok %v hash ok %v", k, i, row.ID, row.ChainSeq,
				row.PrevHash == prev, got == row.RowHash)
		}
		prev = row.RowHash
	}
	if h.HeadHash != prev {
		t.Fatalf("chain %+v head hash %s != last row %s", k, h.HeadHash, prev)
	}
	return rows
}

func headSeq(t *testing.T, db *repository.DB, k repository.ChainKey) uint64 {
	t.Helper()
	h, err := repository.AuditChainRepo{DB: db}.Head(t.Context(), k)
	if err != nil {
		t.Fatal(err)
	}
	return h.ChainSeq
}

func TestAuditChainSeal_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	tx := repository.Transactor{DB: slot.DB}
	farm := repository.ChainKey{TenantID: itest.TenantID, FarmID: itest.FarmID}
	for i := range 3 {
		err := tx.InTx(t.Context(), func(db *repository.DB) error {
			if err := (repository.AuditRepo{}).AppendTx(t.Context(), db, chainRec(itest.FarmID, "ASK", i)); err != nil {
				return err
			}
			return repository.AuditRepo{}.AppendTx(t.Context(), db, model.AuditRecord{Actor: model.Actor{Type: model.ActorUser,
				ID: 9}, Action: "AUTH", Target: "denied", PayloadDigest: "d", Result: model.AuditResultDenied, CreatedAt: t0})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rows := checkChain(t, slot.DB, farm)
	if len(rows) != 3 || rows[0].Rec.CreatedAt.Nanosecond() != 123000000 {
		t.Fatalf("rows %d created_at %v (want ms truncation)", len(rows), rows[0].Rec.CreatedAt)
	}
	if n := len(checkChain(t, slot.DB, repository.ChainKey{})); n != 3 {
		t.Fatalf("platform chain rows = %d, want 3", n)
	}
	// 未经封链事务写审计：失败关闭。
	err := slot.DB.WithContext(t.Context()).Transaction(func(db *repository.DB) error {
		return repository.AuditRepo{}.AppendTx(t.Context(), db, chainRec(itest.FarmID, "ASK", 9))
	})
	if !errors.Is(err, repository.ErrAuditNoSeal) {
		t.Fatalf("unsealed append err = %v, want ErrAuditNoSeal", err)
	}
}

// writingSink 模拟 L7-B 的审计告警出口：Fire 经 AuditRepo.Append 另开短事务写一行 ALERT.* 到 farm 链。
// 若封链失败告警仍在原事务回调内同步发出，原事务持有的 head 行锁会让这次写等锁超时。
type writingSink struct {
	alertRec
	db   *repository.DB
	farm uint64
	errs []error
}

func (s *writingSink) Fire(ctx context.Context, a model.Alert) {
	s.alertRec.Fire(ctx, a)
	err := repository.AuditRepo{DB: s.db}.Append(ctx, model.AuditRecord{TenantID: itest.TenantID, FarmID: s.farm,
		Actor: model.Actor{Type: model.ActorSystem}, Action: "ALERT." + string(a.Name), Target: "alert", PayloadDigest: "d",
		Result: model.AuditResultOK, CreatedAt: t0})
	s.mu.Lock()
	s.errs = append(s.errs, err)
	s.mu.Unlock()
}

func useSink(t *testing.T, s model.AlertSink) {
	t.Helper()
	repository.SetAuditAlertSink(s)
	t.Cleanup(func() { repository.SetAuditAlertSink(nil) })
}

// 缺 head：整笔回滚（含已锁住的 (1,1)），告警在回滚之后发出，告警出口写 (1,1) 链不等锁。
func TestAuditChainHeadMissing_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	sink := &writingSink{db: slot.DB, farm: itest.FarmID}
	useSink(t, sink)
	farm := repository.ChainKey{TenantID: itest.TenantID, FarmID: itest.FarmID}
	err := repository.Transactor{DB: slot.DB}.InTx(t.Context(), func(db *repository.DB) error {
		if err := (repository.AuditRepo{}).AppendTx(t.Context(), db, chainRec(itest.FarmID, "ASK", 1)); err != nil {
			return err
		}
		return repository.AuditRepo{}.AppendTx(t.Context(), db, chainRec(99, "ASK", 2))
	})
	if !errors.Is(err, repository.ErrAuditChainHeadMissing) {
		t.Fatalf("err = %v, want ErrAuditChainHeadMissing", err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM audit_log WHERE action = 'ASK'"); n != 0 {
		t.Fatalf("ASK rows = %d after rollback, want 0", n)
	}
	if sink.count(model.AlertAuditWriteFailed) != 1 || len(sink.errs) != 1 || sink.errs[0] != nil {
		t.Fatalf("alerts %d sink write errs %v", sink.count(model.AlertAuditWriteFailed), sink.errs)
	}
	if !strings.Contains(sink.got[0].Detail, "(1,99)") || sink.got[0].Level != model.AlertP0 {
		t.Fatalf("alert = %+v", sink.got[0])
	}
	rows := checkChain(t, slot.DB, farm) // 只剩告警行
	if len(rows) != 1 || rows[0].Rec.Action != "ALERT."+string(model.AlertAuditWriteFailed) {
		t.Fatalf("farm chain rows = %+v", rows)
	}
}

// 防递归：告警出口自己写审计也失败（目标链缺 head）时不再告警。
func TestAuditChainAlertNoRecursion_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	sink := &writingSink{db: slot.DB, farm: 98}
	useSink(t, sink)
	err := repository.AuditRepo{DB: slot.DB}.Append(t.Context(), chainRec(97, "ASK", 1))
	if !errors.Is(err, repository.ErrAuditChainHeadMissing) {
		t.Fatalf("err = %v", err)
	}
	if n := sink.count(model.AlertAuditWriteFailed); n != 1 || len(sink.errs) != 1 ||
		!errors.Is(sink.errs[0], repository.ErrAuditChainHeadMissing) {
		t.Fatalf("alerts = %d, sink errs %v (want exactly one alert, its own write failing)", n, sink.errs)
	}
}

// 多链交叉写：农场链 (1,1)、(1,2) 与平台链 (0,0) 在同一事务里写，一半按 (1,1)→(1,2)→(0,0)、一半相反；
// head 按链键升序加锁，不死锁；三条链序号连续、哈希可重算。须 -race -p 1 跑。
func TestAuditChainConcurrent_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	const workers, rounds = 8, 5
	tx := repository.Transactor{DB: slot.DB}
	farm1 := repository.ChainKey{TenantID: itest.TenantID, FarmID: 1}
	farm2 := repository.ChainKey{TenantID: itest.TenantID, FarmID: 2}
	keys := []repository.ChainKey{farm1, farm2, {}}
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order := []repository.ChainKey{farm1, farm2, {}}
			if w%2 == 1 {
				order = []repository.ChainKey{{}, farm2, farm1}
			}
			for i := range rounds {
				errs <- tx.InTx(t.Context(), func(db *repository.DB) error {
					for _, k := range order {
						rec := chainRec(k.FarmID, "ASK", w*100+i)
						rec.TenantID = k.TenantID
						if err := (repository.AuditRepo{}).AppendTx(t.Context(), db, rec); err != nil {
							return err
						}
					}
					return nil
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range keys {
		if n := len(checkChain(t, slot.DB, k)); n != workers*rounds {
			t.Fatalf("chain %+v rows = %d, want %d", k, n, workers*rounds)
		}
	}
}

// SENSITIVE.{op} 二次留痕与主审计 / 业务写同事务：提交则两行都在且链连续，业务失败则一并回滚、head 不动。
func TestSensitiveAuditRollback_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	farm := repository.ChainKey{TenantID: itest.TenantID, FarmID: itest.FarmID}
	decisions := repository.NewDecisionRepo(slot.DB, slot.Clock)
	n := 0
	appendMain := func(action model.AuditAction) func(context.Context, *repository.DB) error {
		return func(ctx context.Context, db *repository.DB) error {
			return repository.AuditRepo{}.AppendTx(ctx, db, chainRec(itest.FarmID, string(action), n))
		}
	}
	tests := []struct {
		name      string
		write     func(context.Context, *repository.DB) error
		sensitive model.AuditAction
	}{
		{"flag change", appendMain(model.AuditFlagChange), model.AuditSensitiveFlagChange},
		{"order cancel", appendMain(model.AuditOrderCancel), model.AuditSensitiveOrderCancel},
		{"raw read", appendMain(model.AuditRawRead), model.AuditSensitiveRawRead},
		{"audit export", appendMain(model.AuditExport), model.AuditSensitiveExport},
		{"approval decision", func(ctx context.Context, db *repository.DB) error {
			_, err := decisions.InsertDecisionTx(ctx, db, repository.DecisionRow{TenantID: itest.TenantID, FarmID: itest.FarmID,
				TicketID: 7, RequestID: fmt.Sprintf("l7a-sensitive-%d", n), Decision: "APPROVE", OperatorID: 1002,
				OperatorRole: "agronomist", SnapshotDigest: strings.Repeat("a", 64), CreatedAt: t0})
			return err
		}, model.AuditSensitiveApprovalDecide},
	}
	for _, tt := range tests {
		for _, commit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s commit=%v", tt.name, commit), func(t *testing.T) {
				n++
				before := headSeq(t, slot.DB, farm)
				err := decisions.InTx(t.Context(), func(db *repository.DB) error {
					if err := tt.write(t.Context(), db); err != nil {
						return err
					}
					if !commit {
						return errBusiness
					}
					return nil
				})
				if commit != (err == nil) || (!commit && !errors.Is(err, errBusiness)) {
					t.Fatalf("commit=%v err=%v", commit, err)
				}
				sens := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM audit_log WHERE tenant_id = 1 AND farm_id = 1 AND action = ?",
					string(tt.sensitive))
				dec := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM approval_decision WHERE request_id = ?",
					fmt.Sprintf("l7a-sensitive-%d", n))
				after := headSeq(t, slot.DB, farm)
				if !commit {
					if sens != 0 || dec != 0 || after != before {
						t.Fatalf("rollback left sensitive=%d decision=%d head %d->%d", sens, dec, before, after)
					}
					return
				}
				// 审批决策只追加留痕行（主 APPROVAL 审计来自事件投影）；其余为主行 + 留痕行。
				want := uint64(2)
				if tt.sensitive == model.AuditSensitiveApprovalDecide {
					want = 1
				}
				if sens != 1 || after != before+want {
					t.Fatalf("commit: sensitive=%d head %d->%d (want +%d)", sens, before, after, want)
				}
				rows := checkChain(t, slot.DB, farm)
				last := rows[len(rows)-1]
				if last.Rec.Action != string(tt.sensitive) {
					t.Fatalf("last row action %s, want %s", last.Rec.Action, tt.sensitive)
				}
				if want == 2 && last.Rec.Target != fmt.Sprintf("audit:%d", rows[len(rows)-2].ID) {
					t.Fatalf("sensitive target %s, main id %d", last.Rec.Target, rows[len(rows)-2].ID)
				}
			})
		}
	}
}

// InitChainHeads 幂等：补建缺失的农场 / 租户链 head，重复执行不新增。
func TestInitChainHeads_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	if err := slot.DB.WithContext(t.Context()).Exec(
		"DELETE FROM audit_chain_head WHERE tenant_id = 1 AND farm_id IN (0, 2)").Error; err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{2, 0} {
		got, err := repository.InitChainHeads(t.Context(), slot.DB, t0)
		if err != nil || got != want {
			t.Fatalf("call %d: created %d err %v, want %d", i+1, got, err, want)
		}
	}
	if seq := headSeq(t, slot.DB, repository.ChainKey{TenantID: 1, FarmID: 2}); seq != 0 {
		t.Fatalf("new head seq = %d", seq)
	}
}
