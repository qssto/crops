package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// RawReadTTL 是原文预签名 URL 的有效期（P0 §6.8 功能任务 7）。
const RawReadTTL = 5 * time.Minute

// RAW_READ 拒绝原因（审计 summary.code）。
const (
	RawDenyRole        = "ROLE_INSUFFICIENT"
	RawDenyNotFound    = "RAW_NOT_FOUND"
	RawDenyRunMismatch = "RUN_MISMATCH"
)

// rawReadRoles 是可读原文的角色；operator 不可读。
var rawReadRoles = []model.Role{model.RoleFarmManager, model.RoleAgronomist, model.RolePlatformAdmin}

// RawAccessStore 是读原文需要的仓储能力，repository.RawAccessRepo 实现。
type RawAccessStore interface {
	InTx(ctx context.Context, fn func(tx *repository.DB) error) error
	RawTx(ctx context.Context, tx *repository.DB, scope model.Scope, rawID uint64) (repository.RawObjectRow, error)
	AuditTx(ctx context.Context, tx *repository.DB, rec model.AuditRecord) error
	Audit(ctx context.Context, rec model.AuditRecord) error
}

// AuditDeps 是 AuditService 的依赖，均必填（Logger 可空）。
type AuditDeps struct {
	Store   RawAccessStore
	Presign repository.Presigner
	Clock   clock.Clock
	Logger  *slog.Logger
}

// AuditService 提供带授权审计的原文读取。
type AuditService struct{ d AuditDeps }

// NewAuditService 校验依赖。
func NewAuditService(d AuditDeps) (*AuditService, error) {
	if d.Store == nil || d.Presign == nil || d.Clock == nil {
		return nil, errors.New("service: audit service needs Store, Presign and Clock")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &AuditService{d: d}, nil
}

// RawLink 是签发结果；URL 本身是凭据，不进审计 summary。
type RawLink struct {
	RawID     uint64    `json:"raw_id"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// rawDenied 携带拒绝原因与（若已查到）raw_object 行，交给独立审计。
type rawDenied struct {
	code string
	row  *repository.RawObjectRow
}

func (e *rawDenied) Error() string { return "raw read denied: " + e.code }

// rawAudit 是 RAW_READ 审计的 payload（只含控制字段）。
type rawAudit struct {
	RawID     uint64     `json:"raw_id"`
	RunID     uint64     `json:"run_id"`
	Kind      string     `json:"kind,omitempty"`
	Code      string     `json:"code,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// ReadRaw 校验角色与归属（raw_object 属于 scope 且 run_id 一致）后签发 5 分钟预签名 URL：
// 成功时 RAW_READ/OK 与签发同事务写入；拒绝时独立写 RAW_READ/DENIED 并返回包装 ErrForbidden 的错误（→ 403）。
// 不存在与跨 run 同样按 403 处理，不暴露对象是否存在。
func (s *AuditService) ReadRaw(ctx context.Context, scope model.Scope, runID, rawID uint64) (RawLink, error) {
	if runID == 0 || rawID == 0 {
		return RawLink{}, fmt.Errorf("%w: run_id / raw_id required", ErrBadInput)
	}
	if !hasAnyRole(scope, rawReadRoles) {
		return RawLink{}, s.deny(ctx, scope, runID, rawID, &rawDenied{code: RawDenyRole})
	}
	now := s.d.Clock.Now()
	var link RawLink
	err := s.d.Store.InTx(ctx, func(tx *repository.DB) error {
		row, err := s.d.Store.RawTx(ctx, tx, scope, rawID)
		if errors.Is(err, model.ErrNotFound) {
			return &rawDenied{code: RawDenyNotFound}
		}
		if err != nil {
			return err
		}
		if row.RunID != runID {
			return &rawDenied{code: RawDenyRunMismatch, row: &row}
		}
		url, err := s.d.Presign.PresignGet(ctx, row.ObjectKey, RawReadTTL)
		if err != nil {
			return err
		}
		exp := now.Add(RawReadTTL)
		rec, err := s.rawRecord(scope, runID, rawID, &row, model.AuditResultOK, rawAudit{RawID: rawID, RunID: runID,
			Kind: string(row.Kind), ExpiresAt: &exp}, now)
		if err != nil {
			return err
		}
		if err := s.d.Store.AuditTx(ctx, tx, rec); err != nil {
			return err
		}
		link = RawLink{RawID: rawID, Kind: string(row.Kind), URL: url, ExpiresAt: exp}
		return nil
	})
	var denied *rawDenied
	if errors.As(err, &denied) {
		return RawLink{}, s.deny(ctx, scope, runID, rawID, denied)
	}
	if err != nil {
		return RawLink{}, fmt.Errorf("service: read raw %d of run %d: %w", rawID, runID, err)
	}
	return link, nil
}

// deny 独立写 RAW_READ/DENIED（不随业务事务回滚）；审计写失败时一并返回，仍映射为 403。
func (s *AuditService) deny(ctx context.Context, scope model.Scope, runID, rawID uint64, d *rawDenied) error {
	forbidden := fmt.Errorf("%w: raw %d of run %d: %s", ErrForbidden, rawID, runID, d.code)
	p := rawAudit{RawID: rawID, RunID: runID, Code: d.code}
	if d.row != nil {
		p.Kind = string(d.row.Kind)
	}
	rec, err := s.rawRecord(scope, runID, rawID, d.row, model.AuditResultDenied, p, s.d.Clock.Now())
	if err == nil {
		err = s.d.Store.Audit(context.WithoutCancel(ctx), rec)
	}
	if err != nil {
		s.d.Logger.ErrorContext(ctx, "audit RAW_READ denied", "run_id", runID, "raw_id", rawID, "err", err)
		return errors.Join(forbidden, fmt.Errorf("service: audit raw read: %w", err))
	}
	return forbidden
}

func (s *AuditService) rawRecord(scope model.Scope, runID, rawID uint64, row *repository.RawObjectRow, result string,
	p rawAudit, now time.Time) (model.AuditRecord, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return model.AuditRecord{}, fmt.Errorf("service: encode RAW_READ audit: %w", err)
	}
	rec := repository.NewAuditRecord(scope, &runID, model.Actor{Type: model.ActorUser, ID: scope.UserID},
		model.AuditRawRead, "raw:"+strconv.FormatUint(rawID, 10), body, result, now)
	if row != nil {
		id := row.ID
		rec.RawObjectID = &id
	}
	return rec, nil
}

func hasAnyRole(scope model.Scope, roles []model.Role) bool {
	for _, r := range roles {
		if scope.HasRole(r) {
			return true
		}
	}
	return false
}
