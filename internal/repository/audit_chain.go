package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// 审计哈希链（P1 §6.8 技术实现第 1 条）：AppendTx 只把条目挂到事务上，inTx 在提交前统一封链——
// 按 (tenant_id, farm_id) 升序 SELECT … FOR UPDATE 涉及的 audit_chain_head → 逐条 chain_seq = head+1、
// row_hash = sha256(prev_hash || canonical_json(行)) → INSERT audit_log → 推进 head。

var (
	// ErrAuditNoSeal 表示审计写在了不经 inTx 开启的事务里（没有封链钩子，不写无链行）。
	ErrAuditNoSeal = errors.New("repository: audit append outside a sealing transaction")
	// ErrAuditChainHeadMissing 表示 audit_chain_head 未预建（农场开通 / cmd/migrate 负责）。
	ErrAuditChainHeadMissing = errors.New("repository: audit_chain_head missing")
)

// GenesisHash 是每条链首行的 prev_hash。
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// auditTimeLayout 是参与哈希的 created_at 文本（毫秒 UTC，与 datetime(3) 同精度）。
const auditTimeLayout = "2006-01-02T15:04:05.000Z"

// ChainKey 标识一条链；(0,0) 是平台 / 未解析农场的拒绝类审计链。
type ChainKey struct{ TenantID, FarmID uint64 }

func (k ChainKey) compare(o ChainKey) int {
	if k.TenantID != o.TenantID {
		return cmpU64(k.TenantID, o.TenantID)
	}
	return cmpU64(k.FarmID, o.FarmID)
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ChainHead 是 audit_chain_head 一行。
type ChainHead struct {
	Key      ChainKey
	ChainSeq uint64
	HeadHash string
}

// ChainRow 是带链字段的 audit_log 行（校验 / 导出读取）。
type ChainRow struct {
	ID       uint64
	ChainSeq uint64
	PrevHash string
	RowHash  string
	Rec      model.AuditRecord
}

// auditBatch 是一次事务内待封链的审计条目。
type auditBatch struct {
	mu   sync.Mutex
	recs []model.AuditRecord
}

func (b *auditBatch) add(r model.AuditRecord) {
	b.mu.Lock()
	b.recs = append(b.recs, r)
	b.mu.Unlock()
}

func (b *auditBatch) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.recs)
}

func (b *auditBatch) truncate(n int) {
	b.mu.Lock()
	b.recs = b.recs[:n]
	b.mu.Unlock()
}

func (b *auditBatch) take() []model.AuditRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	recs := b.recs
	b.recs = nil
	return recs
}

// auditBatches：事务连接（*sql.Tx）→ *auditBatch。不放 context：调用方常 tx.WithContext(ctx) 换掉 Statement.Context，
// 而事务内派生的句柄共享同一个 ConnPool。
var auditBatches sync.Map

// sealFailure 记录封链失败的条目与原因。告警由 inTx 在事务回滚之后发出：真实告警出口会经 Append 另开短事务写
// ALERT.* 审计并锁同一 head 行，在本事务回调内同步告警会等本事务持有的行锁直到超时。
type sealFailure struct {
	recs []model.AuditRecord
	err  error
}

// withAuditSeal 在事务 tx 内执行 fn，fn 成功后封链；同一连接上嵌套（SAVEPOINT）时由最外层封链，内层失败撤回其条目。
// 封链失败时写入 failed（不在此处告警），返回错误使事务回滚。
func withAuditSeal(ctx context.Context, tx *gorm.DB, fn func(tx *gorm.DB) error, failed *sealFailure) error {
	key := tx.Statement.ConnPool
	b := &auditBatch{}
	if v, loaded := auditBatches.LoadOrStore(key, b); loaded {
		outer := v.(*auditBatch)
		n := outer.len()
		if err := fn(tx); err != nil {
			outer.truncate(n)
			return err
		}
		return nil
	}
	defer auditBatches.Delete(key)
	if err := fn(tx); err != nil {
		return err
	}
	recs := b.take()
	if err := sealAudit(ctx, tx, recs); err != nil {
		*failed = sealFailure{recs: recs, err: err}
		return err
	}
	return nil
}

// queueAudit 把 r 挂到 tx 的封链批次；tx 不是经 inTx 开启的返回 ErrAuditNoSeal。
// 此分支同步告警：该连接未经 sealAudit（head 行锁只在 sealAudit 里取），不持有告警写审计要等的锁。
func queueAudit(ctx context.Context, tx *gorm.DB, r model.AuditRecord) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	v, ok := auditBatches.Load(tx.Statement.ConnPool)
	if !ok {
		err := fmt.Errorf("%w: %s", ErrAuditNoSeal, r.Action)
		fireAuditWriteFailed(ctx, []model.AuditRecord{r}, err)
		return err
	}
	v.(*auditBatch).add(r)
	return nil
}

const (
	lockChainHeadSQL    = `SELECT chain_seq, head_hash FROM audit_chain_head WHERE tenant_id=? AND farm_id=? FOR UPDATE`
	advanceChainHeadSQL = `UPDATE audit_chain_head SET chain_seq=?, head_hash=?, updated_at=? WHERE tenant_id=? AND farm_id=?`
	insertChainAuditSQL = `INSERT INTO audit_log (created_at, tenant_id, farm_id, run_id, actor_type, actor_id, action, target, payload_digest, summary, result, raw_object_id, chain_seq, prev_hash, row_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	// InitChainHeadSQL 预建一条链的 head（农场开通 / cmd/migrate；幂等）。
	InitChainHeadSQL = `INSERT IGNORE INTO audit_chain_head (tenant_id, farm_id, chain_seq, head_hash, updated_at) VALUES (?, ?, 0, REPEAT('0',64), ?)`
)

// sealAudit 给 recs 编链并写入：先按链键升序锁 head（多链事务加锁顺序固定，交叉写不死锁），再按追加顺序逐条写。
func sealAudit(ctx context.Context, tx *gorm.DB, recs []model.AuditRecord) error {
	if len(recs) == 0 {
		return nil
	}
	heads := map[ChainKey]*ChainHead{}
	for _, r := range recs {
		heads[ChainKey{r.TenantID, r.FarmID}] = nil
	}
	keys := make([]ChainKey, 0, len(heads))
	for k := range heads {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, ChainKey.compare)
	for _, k := range keys {
		h := &ChainHead{Key: k}
		err := tx.Statement.ConnPool.QueryRowContext(ctx, lockChainHeadSQL, k.TenantID, k.FarmID).Scan(&h.ChainSeq, &h.HeadHash)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: (%d,%d)", ErrAuditChainHeadMissing, k.TenantID, k.FarmID)
		}
		if err != nil {
			return fmt.Errorf("repository: lock audit_chain_head (%d,%d): %w", k.TenantID, k.FarmID, err)
		}
		heads[k] = h
	}
	for _, r := range recs {
		h := heads[ChainKey{r.TenantID, r.FarmID}]
		id, err := insertChained(ctx, tx, h, r)
		if err != nil {
			return err
		}
		if action, ok := model.SensitiveAudit(r.Action, r.Result); ok {
			sr, err := sensitiveRecord(r, action, id)
			if err != nil {
				return err
			}
			if _, err := insertChained(ctx, tx, h, sr); err != nil {
				return err
			}
		}
	}
	now := tx.Config.NowFunc().UTC().Truncate(time.Millisecond)
	for _, k := range keys {
		h := heads[k]
		if _, err := tx.Statement.ConnPool.ExecContext(ctx, advanceChainHeadSQL, h.ChainSeq, h.HeadHash, now,
			k.TenantID, k.FarmID); err != nil {
			return fmt.Errorf("repository: advance audit_chain_head (%d,%d): %w", k.TenantID, k.FarmID, err)
		}
	}
	return nil
}

// insertChained 以 h 的下一个序号写一行并推进内存中的 h，返回 audit_log.id。
func insertChained(ctx context.Context, tx *gorm.DB, h *ChainHead, r model.AuditRecord) (uint64, error) {
	r, err := normalizeAudit(r)
	if err != nil {
		return 0, err
	}
	seq := h.ChainSeq + 1
	hash, err := rowHash(h.HeadHash, seq, r)
	if err != nil {
		return 0, err
	}
	var summary any
	if len(r.Summary) > 0 {
		summary = string(r.Summary)
	}
	res, err := tx.Statement.ConnPool.ExecContext(ctx, insertChainAuditSQL, r.CreatedAt, r.TenantID, r.FarmID, r.RunID,
		string(r.Actor.Type), r.Actor.ID, r.Action, nullIfEmpty(r.Target), r.PayloadDigest, summary, r.Result,
		r.RawObjectID, seq, h.HeadHash, hash)
	if err != nil {
		return 0, fmt.Errorf("repository: insert audit_log %s %s: %w", r.Action, r.Result, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("repository: audit_log id: %w", err)
	}
	h.ChainSeq, h.HeadHash = seq, hash
	return uint64(id), nil
}

// normalizeAudit 把行改成「写入值 = 读回值」的形式：时间截到毫秒 UTC，summary 存规范化 JSON。
func normalizeAudit(r model.AuditRecord) (model.AuditRecord, error) {
	r.CreatedAt = r.CreatedAt.UTC().Truncate(time.Millisecond)
	if len(r.Summary) > 0 {
		c, err := CanonicalJSON(r.Summary)
		if err != nil {
			return r, fmt.Errorf("repository: audit %s summary: %w", r.Action, err)
		}
		r.Summary = c
	}
	return r, nil
}

// sensitiveRecord 生成主审计 main（audit_log.id = mainID）的二次留痕行：target audit:{id}，summary 指回主记录。
func sensitiveRecord(main model.AuditRecord, action model.AuditAction, mainID uint64) (model.AuditRecord, error) {
	summary, err := json.Marshal(map[string]string{"action": main.Action, "target": main.Target,
		"payload_digest": main.PayloadDigest})
	if err != nil {
		return model.AuditRecord{}, fmt.Errorf("repository: sensitive audit summary: %w", err)
	}
	return model.AuditRecord{TenantID: main.TenantID, FarmID: main.FarmID, RunID: main.RunID, Actor: main.Actor,
		Action: string(action), Target: ref("audit", mainID), PayloadDigest: Digest(summary), Summary: summary,
		Result: model.AuditResultOK, CreatedAt: main.CreatedAt}, nil
}

// AuditRowHash 重算一行的 row_hash（校验用；row 为读回值）。
func AuditRowHash(prevHash string, row ChainRow) (string, error) {
	r, err := normalizeAudit(row.Rec)
	if err != nil {
		return "", err
	}
	return rowHash(prevHash, row.ChainSeq, r)
}

// rowHash = hex(sha256(prev_hash 十六进制文本 || canonical_json(行)))；r 须已 normalizeAudit。
func rowHash(prevHash string, seq uint64, r model.AuditRecord) (string, error) {
	c, err := canonicalAuditRow(seq, r)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write(c)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalAuditRow 是参与哈希的行：不含自增 id；整数列写十进制字符串；NULL / 空 target / 空 summary 写 null。
func canonicalAuditRow(seq uint64, r model.AuditRecord) ([]byte, error) {
	var summary any
	if len(r.Summary) > 0 {
		summary = rawCanonical(r.Summary)
	}
	var target any
	if r.Target != "" {
		target = r.Target
	}
	obj := jsonObject{
		"action": r.Action, "actor_id": u64Text(r.Actor.ID), "actor_type": string(r.Actor.Type),
		"chain_seq": u64Text(seq), "created_at": r.CreatedAt.UTC().Format(auditTimeLayout),
		"farm_id": u64Text(r.FarmID), "payload_digest": r.PayloadDigest, "raw_object_id": optU64Text(r.RawObjectID),
		"result": r.Result, "run_id": optU64Text(r.RunID), "summary": summary, "target": target,
		"tenant_id": u64Text(r.TenantID),
	}
	var out bytes.Buffer
	if err := encodeCanonical(&out, obj); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func u64Text(v uint64) string { return strconv.FormatUint(v, 10) }

func optU64Text(p *uint64) any {
	if p == nil {
		return nil
	}
	return u64Text(*p)
}

// ---- 封链失败告警 ----

type alertSinkBox struct{ sink model.AlertSink }

var auditAlerts atomic.Pointer[alertSinkBox]

// SetAuditAlertSink 装配封链失败的告警出口（app 启动时调用；未装配时只记日志）。
func SetAuditAlertSink(s model.AlertSink) { auditAlerts.Store(&alertSinkBox{sink: s}) }

// auditAlertingKey 标记「正在发告警」：告警实现自己写 ALERT.* 审计也走封链，失败时不再递归告警。
type auditAlertingKey struct{}

func fireAuditWriteFailed(ctx context.Context, recs []model.AuditRecord, cause error) {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || ctx.Value(auditAlertingKey{}) != nil {
		return
	}
	var scope model.Scope
	if len(recs) > 0 {
		scope = model.Scope{TenantID: recs[0].TenantID, FarmID: recs[0].FarmID}
	}
	a := model.Alert{Name: model.AlertAuditWriteFailed, Level: model.AlertP0, Scope: scope, Detail: cause.Error()}
	if len(recs) > 0 && recs[0].RunID != nil {
		a.RunID = *recs[0].RunID
	}
	box := auditAlerts.Load()
	if box == nil || box.sink == nil {
		slog.Default().ErrorContext(ctx, "audit: seal failed (no alert sink)", "alert", string(a.Name),
			"tenant_id", scope.TenantID, "farm_id", scope.FarmID, "err", cause)
		return
	}
	box.sink.Fire(context.WithValue(ctx, auditAlertingKey{}, true), a)
}
