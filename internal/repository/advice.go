package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// 情景记忆种类与摘要上限（episodic_memory.kind / summary varchar(512)）。
const (
	memoryKindAdvice = "ADVICE"
	memorySummaryMax = 512
)

var (
	// ErrMeasuredWithoutToolCall 表示 measured 引用缺 tool_call_id（P0 L4 红线）。
	ErrMeasuredWithoutToolCall = errors.New("repository: measured citation requires tool_call_id")
	// ErrScopeMismatch 表示写入对象的 tenant_id / farm_id 与 Scope 不一致。
	ErrScopeMismatch = errors.New("repository: tenant/farm does not match scope")
)

// AdviceRepo 实现 agent.AdviceStore：同一事务写 advice_card + advice_citation + episodic_memory(ADVICE)。
type AdviceRepo struct {
	db    *gorm.DB
	clock clock.Clock
}

var _ agent.AdviceStore = (*AdviceRepo)(nil)

// NewAdviceRepo 构造 AdviceRepo。
func NewAdviceRepo(db *gorm.DB, c clock.Clock) *AdviceRepo { return &AdviceRepo{db: db, clock: c} }

// checkScope 校验对象自带的 tenant/farm（0 视为未填）与 Scope 一致。
func checkScope(scope model.Scope, tenantID, farmID uint64) error {
	if (tenantID != 0 && tenantID != scope.TenantID) || (farmID != 0 && farmID != scope.FarmID) {
		return fmt.Errorf("%w: (%d,%d) vs scope (%d,%d)", ErrScopeMismatch, tenantID, farmID, scope.TenantID, scope.FarmID)
	}
	return nil
}

func validateCitations(scope model.Scope, cs []model.Citation) error {
	for i, c := range cs {
		if err := checkScope(scope, c.TenantID, c.FarmID); err != nil {
			return fmt.Errorf("citations[%d]: %w", i, err)
		}
		if c.Type == model.CitationMeasured && (c.ToolCallID == nil || *c.ToolCallID == 0) {
			return fmt.Errorf("citations[%d]: %w", i, ErrMeasuredWithoutToolCall)
		}
	}
	return nil
}

// jsonOrNull 编码可空 json 列：空切片写 NULL。
func jsonOrNull[T any](v []T) (any, error) {
	if len(v) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// SaveAdvice 幂等落卡：uk_run 冲突时 LAST_INSERT_ID(id) 返回已有 advice_id，citation 按 uk_advice_seq 不重复，
// 记忆按 uk_kind_ref INSERT IGNORE。拒答卡不写记忆。
func (r *AdviceRepo) SaveAdvice(ctx context.Context, scope model.Scope, card model.AdviceCard) (uint64, error) {
	if err := checkScope(scope, card.TenantID, card.FarmID); err != nil {
		return 0, err
	}
	if err := validateCitations(scope, card.Citations); err != nil {
		return 0, fmt.Errorf("repository: advice run %d: %w", card.RunID, err)
	}
	reasons, err := json.Marshal(nonNil(card.Reasons))
	if err != nil {
		return 0, fmt.Errorf("repository: encode reasons: %w", err)
	}
	numbers, err := jsonOrNull(card.Numbers)
	if err != nil {
		return 0, fmt.Errorf("repository: encode numbers: %w", err)
	}
	candidates, err := jsonOrNull(card.ProductCandidates)
	if err != nil {
		return 0, fmt.Errorf("repository: encode product_candidates: %w", err)
	}
	now := r.clock.Now()
	var adviceID uint64
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res, err := execInTx(ctx, tx,
			`INSERT INTO advice_card (tenant_id, farm_id, field_id, run_id, crop, phenology_stage, category, advice_type, outcome, conclusion, reasons, numbers, product_candidates, selected_product, next_action, refuse_reason, degraded, schema_ver, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,
			scope.TenantID, scope.FarmID, card.FieldID, card.RunID, card.Crop, card.PhenologyStage,
			string(card.Category), string(card.AdviceType), string(card.Outcome), card.Conclusion, string(reasons),
			numbers, candidates, nullIfEmpty(card.SelectedProduct), string(card.NextAction),
			nullIfEmpty(card.RefuseReason), card.Degraded, model.ContractVersion, now)
		if err != nil {
			return fmt.Errorf("repository: insert advice_card run %d: %w", card.RunID, err)
		}
		last, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("repository: advice_card id: %w", err)
		}
		adviceID = uint64(last)
		for i, c := range card.Citations {
			if _, err := execInTx(ctx, tx,
				`INSERT INTO advice_citation (tenant_id, farm_id, advice_id, seq, claim_path, type, kb_id, kb_version, tool_call_id, collected_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE id = id`,
				scope.TenantID, scope.FarmID, adviceID, i, c.ClaimPath, string(c.Type),
				nullIfEmpty(c.KBID), nullIfEmpty(c.KBVersion), c.ToolCallID, c.CollectedAt); err != nil {
				return fmt.Errorf("repository: insert advice_citation %d#%d: %w", adviceID, i, err)
			}
		}
		if card.Outcome != model.OutcomeAdvice {
			return nil
		}
		if _, err := execInTx(ctx, tx,
			`INSERT IGNORE INTO episodic_memory (tenant_id, farm_id, field_id, kind, ref_id, summary, occurred_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			scope.TenantID, scope.FarmID, card.FieldID, memoryKindAdvice, adviceID,
			truncateRunes(card.Conclusion, memorySummaryMax), now); err != nil {
			return fmt.Errorf("repository: insert episodic_memory advice %d: %w", adviceID, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return adviceID, nil
}

// adviceRow 是 advice_card 的读取列。
type adviceRow struct {
	ID                uint64
	FieldID           uint64
	RunID             uint64
	Crop              string
	PhenologyStage    string
	Category          string
	AdviceType        string
	Outcome           string
	Conclusion        string
	Reasons           []byte
	Numbers           []byte
	ProductCandidates []byte
	SelectedProduct   *string
	NextAction        string
	RefuseReason      *string
	Degraded          bool
}

// citationRow 是 advice_citation LEFT JOIN tool_call 的读取列（measured 的 data_source / value 取自 tool_call）。
type citationRow struct {
	ClaimPath   string
	Type        string
	KBID        *string
	KBVersion   *string
	ToolCallID  *uint64
	CollectedAt *time.Time
	Source      *string
	Summary     []byte
}

// GetByRun 按 run_id 读回建议卡；无卡返回 model.ErrNotFound。
// confidence / suggested_window 不落库（P0 DDL 无此列），读回为零值。
func (r *AdviceRepo) GetByRun(ctx context.Context, scope model.Scope, runID uint64) (model.AdviceCard, error) {
	var row adviceRow
	res := r.db.WithContext(ctx).Raw(
		`SELECT id, field_id, run_id, crop, phenology_stage, category, advice_type, outcome, conclusion, reasons, numbers, product_candidates, selected_product, next_action, refuse_reason, degraded FROM advice_card WHERE tenant_id=? AND farm_id=? AND run_id=?`,
		scope.TenantID, scope.FarmID, runID).Scan(&row)
	if res.Error != nil {
		return model.AdviceCard{}, fmt.Errorf("repository: get advice_card run %d: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
		return model.AdviceCard{}, fmt.Errorf("repository: advice_card run %d: %w", runID, model.ErrNotFound)
	}
	card := model.AdviceCard{AdviceID: row.ID, RunID: row.RunID, TenantID: scope.TenantID, FarmID: scope.FarmID,
		FieldID: row.FieldID, Crop: row.Crop, PhenologyStage: row.PhenologyStage, Category: model.Category(row.Category),
		AdviceType: model.AdviceType(row.AdviceType), Outcome: model.Outcome(row.Outcome), Degraded: row.Degraded,
		Conclusion: row.Conclusion, NextAction: model.NextAction(row.NextAction), Citations: []model.Citation{}}
	if row.SelectedProduct != nil {
		card.SelectedProduct = *row.SelectedProduct
	}
	if row.RefuseReason != nil {
		card.RefuseReason = *row.RefuseReason
	}
	for _, col := range []struct {
		raw []byte
		dst any
	}{{row.Reasons, &card.Reasons}, {row.Numbers, &card.Numbers}, {row.ProductCandidates, &card.ProductCandidates}} {
		if len(col.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(col.raw, col.dst); err != nil {
			return model.AdviceCard{}, fmt.Errorf("repository: decode advice_card %d: %w", row.ID, err)
		}
	}
	var cites []citationRow
	if err := r.db.WithContext(ctx).Raw(
		`SELECT c.claim_path, c.type, c.kb_id, c.kb_version, c.tool_call_id, c.collected_at, t.source, t.result_summary AS summary
FROM advice_citation c LEFT JOIN tool_call t ON t.id = c.tool_call_id AND t.tenant_id = c.tenant_id AND t.farm_id = c.farm_id
WHERE c.tenant_id=? AND c.farm_id=? AND c.advice_id=? ORDER BY c.seq`,
		scope.TenantID, scope.FarmID, row.ID).Scan(&cites).Error; err != nil {
		return model.AdviceCard{}, fmt.Errorf("repository: list advice_citation %d: %w", row.ID, err)
	}
	for _, c := range cites {
		card.Citations = append(card.Citations, c.toModel(scope))
	}
	return card, nil
}

func (c citationRow) toModel(scope model.Scope) model.Citation {
	out := model.Citation{TenantID: scope.TenantID, FarmID: scope.FarmID, Type: model.CitationType(c.Type),
		ClaimPath: c.ClaimPath, ToolCallID: c.ToolCallID, CollectedAt: c.CollectedAt}
	if c.KBID != nil {
		out.KBID = *c.KBID
	}
	if c.KBVersion != nil {
		out.KBVersion = *c.KBVersion
	}
	if c.Source != nil {
		out.DataSource = *c.Source
	}
	if len(c.Summary) > 0 {
		out.Value = json.RawMessage(c.Summary)
	}
	return out
}

// nonNil 把 nil 切片换成空切片（json 编码为 [] 而非 null）。
func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// truncateRunes 按字符截断（varchar 按字符计长）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
