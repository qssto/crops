package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P2 L8 归因问题处置（PATCH /api/v1/admin/attribution-issues/:id）：平台级运营改 disposition / owner_id / resolution_ref /
// status，带 row_ver 乐观锁。路由层 RequireOps(attribution.write)；本层再防御一次（只认 (0,0) 上的 platform_admin 绑定）。

// IssuePatcher 是平台问题行的处置更新（repository.AttributionRepo 实现）。
type IssuePatcher interface {
	PatchIssue(ctx context.Context, sys repository.SystemScope, id uint64, p model.AttributionPatch,
		at time.Time) (model.AttributionIssue, error)
}

// AttributionIssueDeps 是处置服务依赖；System 由 app 构造（archtest SystemScope 白名单）。
type AttributionIssueDeps struct {
	Store  IssuePatcher
	Clock  clock.Clock
	System repository.SystemScope
}

// AttributionIssueService 是归因问题处置入口。
type AttributionIssueService struct{ d AttributionIssueDeps }

// 处置更新的错误码（controller 原样回给客户端）。
var (
	// ErrIssueConflict 是 row_ver 不符（他人已改）→ 409。
	ErrIssueConflict = errors.New("ISSUE_ROW_VER_CONFLICT")
)

// NewAttributionIssueService 校验依赖。
func NewAttributionIssueService(d AttributionIssueDeps) (*AttributionIssueService, error) {
	if d.Store == nil || d.Clock == nil {
		return nil, errors.New("service: attribution issue service needs Store and Clock")
	}
	return &AttributionIssueService{d: d}, nil
}

// Patch 校验入参后按 row_ver 更新：非平台运营 → ErrRoleInsufficient，入参非法 → ErrBadInput（含 model.ErrAttributionPatch），
// 行不存在 → model.ErrNotFound，row_ver 不符 → ErrIssueConflict。
func (s *AttributionIssueService) Patch(ctx context.Context, actor model.Scope, id uint64,
	p model.AttributionPatch) (model.AttributionIssue, error) {
	if actor.TenantID != 0 || actor.FarmID != 0 || !actor.HasRole(model.RolePlatformAdmin) {
		return model.AttributionIssue{}, fmt.Errorf("service: attribution patch needs platform_admin: %w", ErrRoleInsufficient)
	}
	if id == 0 {
		return model.AttributionIssue{}, fmt.Errorf("service: attribution issue id: %w", ErrBadInput)
	}
	if err := p.Validate(); err != nil {
		return model.AttributionIssue{}, fmt.Errorf("service: %w: %w", ErrBadInput, err)
	}
	got, err := s.d.Store.PatchIssue(ctx, s.d.System, id, p, s.d.Clock.Now())
	if errors.Is(err, model.ErrConcurrentUpdate) {
		return model.AttributionIssue{}, fmt.Errorf("service: attribution issue %d: %w", id, ErrIssueConflict)
	}
	if err != nil {
		return model.AttributionIssue{}, fmt.Errorf("service: attribution issue %d: %w", id, err)
	}
	return got, nil
}
