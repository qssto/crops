package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// BackflowRepo 是 P2 L8 线上回流 backflow-collect 的业务库读写（农场 Scope，走 agent_run.idx_backflow /
// audit_log.idx_run / tool_call.idx_run）。真实 ID 与草稿合成码的对应只写 agent_run.backflow_draft_ref。
type BackflowRepo struct{ DB *gorm.DB }

// ErrBackflowStatus 表示回流状态推进的目标状态非法。
var ErrBackflowStatus = errors.New("repository: backflow status invalid")

// MarkSelected 把一批 run 置 SELECTED（只改尚未进回流的行，重跑幂等），返回新选中的行数。
// 同一事务为每个新选中的 run 写一条 BACKFLOW 审计事件（status=SELECTED，周报回流计数按事件时刻）。
func (r BackflowRepo) MarkSelected(ctx context.Context, scope model.Scope, runIDs []uint64, at time.Time) (int64, error) {
	if err := requireScope(scope); err != nil {
		return 0, err
	}
	if len(runIDs) == 0 {
		return 0, nil
	}
	var n int64
	err := inTx(ctx, r.DB, func(tx *gorm.DB) error {
		var ids []uint64
		if err := tx.Raw(`SELECT id FROM agent_run WHERE tenant_id = ? AND farm_id = ? AND id IN (?) AND backflow_status IS NULL
ORDER BY id FOR UPDATE`, scope.TenantID, scope.FarmID, runIDs).Scan(&ids).Error; err != nil {
			return fmt.Errorf("lock runs: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		res := tx.Exec(`UPDATE agent_run SET backflow_status = ?, updated_at = ?
WHERE tenant_id = ? AND farm_id = ? AND id IN (?) AND backflow_status IS NULL`,
			model.BackflowSelected, at.UTC(), scope.TenantID, scope.FarmID, ids)
		if res.Error != nil {
			return fmt.Errorf("update runs: %w", res.Error)
		}
		n = res.RowsAffected
		for _, id := range ids {
			if err := backflowEvent(ctx, tx, scope, id, model.BackflowSelected, 0, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("repository: mark backflow selected farm %d: %w", scope.FarmID, err)
	}
	return n, nil
}

// SelectedRuns 返回本农场 SELECTED 的 run（按 id 升序，最多 limit 个；含上轮中断留下的）。
func (r BackflowRepo) SelectedRuns(ctx context.Context, scope model.Scope, limit int) ([]uint64, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var ids []uint64
	if err := r.DB.WithContext(ctx).Raw(`SELECT id FROM agent_run
WHERE tenant_id = ? AND farm_id = ? AND backflow_status = ? ORDER BY id LIMIT ?`,
		scope.TenantID, scope.FarmID, model.BackflowSelected, limit).Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("repository: selected backflow runs farm %d: %w", scope.FarmID, err)
	}
	return ids, nil
}

// Material 读一个 run 的草稿材料：agent_run + field_profile.crop、audit_log 的 ASK / KB_LOAD、有原文的工具调用。
func (r BackflowRepo) Material(ctx context.Context, scope model.Scope, runID uint64) (model.BackflowMaterial, error) {
	if err := requireScope(scope); err != nil {
		return model.BackflowMaterial{}, err
	}
	db := r.DB.WithContext(ctx)
	t, f := scope.TenantID, scope.FarmID
	var run struct {
		ID, FieldID, UserID uint64
		KnowledgeRef, Crop  string
	}
	res := db.Raw(`SELECT r.id, r.field_id, r.user_id, r.knowledge_ref, COALESCE(fp.crop, '') AS crop FROM agent_run r
LEFT JOIN field_profile fp ON fp.tenant_id = r.tenant_id AND fp.farm_id = r.farm_id AND fp.id = r.field_id
WHERE r.tenant_id = ? AND r.farm_id = ? AND r.id = ?`, t, f, runID).Scan(&run)
	if res.Error != nil {
		return model.BackflowMaterial{}, fmt.Errorf("repository: backflow run %d: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
		return model.BackflowMaterial{}, fmt.Errorf("repository: backflow run %d: %w", runID, model.ErrNotFound)
	}
	m := model.BackflowMaterial{RunID: run.ID, FieldID: run.FieldID, UserID: run.UserID, KnowledgeRef: run.KnowledgeRef,
		Crop: run.Crop}
	var audits []struct {
		Action, Target string
		Question       *string
		CreatedAt      time.Time
	}
	if err := db.Raw(`SELECT action, target, JSON_UNQUOTE(JSON_EXTRACT(summary, '$.question')) AS question, created_at
FROM audit_log WHERE tenant_id = ? AND farm_id = ? AND run_id = ? AND action IN (?, ?) ORDER BY created_at, id`,
		t, f, runID, string(model.AuditAsk), string(model.AuditKBLoad)).Scan(&audits).Error; err != nil {
		return model.BackflowMaterial{}, fmt.Errorf("repository: backflow audit run %d: %w", runID, err)
	}
	kbSeen := false
	for _, a := range audits {
		switch {
		case a.Action == string(model.AuditAsk) && m.AskedAt.IsZero():
			m.AskedAt = a.CreatedAt
			if a.Question != nil {
				m.Question = *a.Question
			}
		case a.Action == string(model.AuditKBLoad) && !kbSeen:
			if ref := strings.TrimPrefix(a.Target, "kb:"); ref != "" {
				m.KnowledgeRef, kbSeen = ref, true
			}
		}
	}
	if err := db.Raw(`SELECT tool_name AS name, raw_object_id AS raw_id FROM tool_call
WHERE tenant_id = ? AND farm_id = ? AND run_id = ? AND superseded_at IS NULL AND status = ? AND raw_object_id IS NOT NULL
ORDER BY id`, t, f, runID, model.ToolStatusOK).Scan(&m.Tools).Error; err != nil {
		return model.BackflowMaterial{}, fmt.Errorf("repository: backflow tool calls run %d: %w", runID, err)
	}
	return m, nil
}

// FinishDraft 把 SELECTED 的 run 推进到 DRAFTED（写 draft_ref）、PII_FAILED 或 FAILED（draft_ref 留空），
// 同一事务写 BACKFLOW 审计事件（summary 不含 draft_ref：映射只留 agent_run）。
// 行已不是 SELECTED（他人已处理）→ model.ErrConcurrentUpdate。
func (r BackflowRepo) FinishDraft(ctx context.Context, scope model.Scope, runID uint64, status, draftRef string,
	at time.Time) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	var ref any
	switch status {
	case model.BackflowDrafted:
		if draftRef == "" {
			return fmt.Errorf("%w: DRAFTED needs draft_ref", ErrBackflowStatus)
		}
		ref = draftRef
	case model.BackflowPIIFailed, model.BackflowFailed:
	default:
		return fmt.Errorf("%w: %q", ErrBackflowStatus, status)
	}
	return inTx(ctx, r.DB, func(tx *gorm.DB) error { return finishTx(ctx, tx, scope, runID, status, ref, 0, at) })
}

// finishTx 在事务内把 SELECTED 的 run 推进到 status 并写事件；attempt 非 0 时记入事件。
func finishTx(ctx context.Context, tx *gorm.DB, scope model.Scope, runID uint64, status string, ref any, attempt int,
	at time.Time) error {
	res, err := execInTx(ctx, tx, `UPDATE agent_run SET backflow_status = ?, backflow_draft_ref = ?, updated_at = ?
WHERE tenant_id = ? AND farm_id = ? AND id = ? AND backflow_status = ?`,
		status, ref, at.UTC(), scope.TenantID, scope.FarmID, runID, model.BackflowSelected)
	if err != nil {
		return fmt.Errorf("repository: finish backflow run %d: %w", runID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("repository: finish backflow run %d: %w", runID, err)
	} else if n == 0 {
		return fmt.Errorf("repository: finish backflow run %d: %w", runID, model.ErrConcurrentUpdate)
	}
	return backflowEvent(ctx, tx, scope, runID, status, attempt, at)
}

// FailAttempt 记一次失败的出草稿尝试：同一事务锁住 SELECTED 的 run、数此前的 RETRY 事件、写本次 RETRY 事件；
// 累计达 maxAttempts 次把 run 置 FAILED（不再进 SelectedRuns）。尝试次数存在 audit_log（不改迁移、重启不丢）。
// run 已不是 SELECTED → model.ErrConcurrentUpdate。
func (r BackflowRepo) FailAttempt(ctx context.Context, scope model.Scope, runID uint64, maxAttempts int,
	at time.Time) (bool, error) {
	if err := requireScope(scope); err != nil {
		return false, err
	}
	if maxAttempts <= 0 {
		return false, fmt.Errorf("%w: maxAttempts %d", ErrBackflowStatus, maxAttempts)
	}
	failed := false
	err := inTx(ctx, r.DB, func(tx *gorm.DB) error {
		var ids []uint64
		if err := tx.Raw(`SELECT id FROM agent_run WHERE tenant_id = ? AND farm_id = ? AND id = ? AND backflow_status = ? FOR UPDATE`,
			scope.TenantID, scope.FarmID, runID, model.BackflowSelected).Scan(&ids).Error; err != nil {
			return fmt.Errorf("lock run: %w", err)
		}
		if len(ids) == 0 {
			return model.ErrConcurrentUpdate
		}
		var prior int
		if err := tx.Raw(`SELECT COUNT(*) FROM audit_log WHERE tenant_id = ? AND farm_id = ? AND run_id = ? AND action = ?
  AND JSON_UNQUOTE(JSON_EXTRACT(summary, '$.status')) = ?`, scope.TenantID, scope.FarmID, runID,
			string(model.AuditBackflow), model.BackflowRetry).Scan(&prior).Error; err != nil {
			return fmt.Errorf("count attempts: %w", err)
		}
		attempt := prior + 1
		if err := backflowEvent(ctx, tx, scope, runID, model.BackflowRetry, attempt, at); err != nil {
			return err
		}
		if attempt < maxAttempts {
			return nil
		}
		failed = true
		return finishTx(ctx, tx, scope, runID, model.BackflowFailed, nil, attempt, at)
	})
	if err != nil {
		return false, fmt.Errorf("repository: backflow attempt run %d: %w", runID, err)
	}
	return failed, nil
}

// backflowEvent 在事务内追加一条 BACKFLOW 审计事件（target run:{id}，summary {status, attempt?}）。
func backflowEvent(ctx context.Context, tx *gorm.DB, scope model.Scope, runID uint64, status string, attempt int,
	at time.Time) error {
	payload := map[string]any{"status": status}
	if attempt > 0 {
		payload["attempt"] = attempt
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("repository: encode backflow event: %w", err)
	}
	rec := NewAuditRecord(scope, &runID, model.Actor{Type: model.ActorSystem}, model.AuditBackflow,
		"run:"+strconv.FormatUint(runID, 10), b, model.AuditResultOK, at.UTC())
	if err := (AuditRepo{}).AppendTx(ctx, tx, rec); err != nil {
		return fmt.Errorf("repository: backflow event run %d: %w", runID, err)
	}
	return nil
}

// RawFetchMaxBytes 是回流读一个工具原文对象的上限（mcp Result JSON，远小于此）。
const RawFetchMaxBytes = 4 << 20

// ErrRawTooLarge 表示原文对象超过 RawFetchMaxBytes。
var ErrRawTooLarge = errors.New("repository: raw object too large")

// PresignedFetcher 按 ReadRaw 签发的预签名 URL 取原文（回流只经 ReadRaw 取原文，保证每次读都有 RAW_READ 审计）。
type PresignedFetcher struct{ Client *http.Client }

// Fetch GET 预签名 URL，非 200 或超过 RawFetchMaxBytes 报错。URL 本身是凭据，不写进错误信息。
func (p PresignedFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("repository: build raw fetch request")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("repository: raw fetch failed")
	}
	defer resp.Body.Close() //nolint:errcheck // 只读响应体，关闭失败不影响结果
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("repository: raw fetch status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, RawFetchMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("repository: read raw body: %w", err)
	}
	if len(body) > RawFetchMaxBytes {
		return nil, ErrRawTooLarge
	}
	return body, nil
}
