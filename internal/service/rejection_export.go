package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/model"
)

// P1 L5 驳回原因导出（§6.6 流程 6）：本农场 [from, to] 内的 rejection_reason，跨度上限可配（缺省 31 天）。

// RejectionLister 是驳回原因读取，repository.RejectionRepo 实现。
type RejectionLister interface {
	ListRejections(ctx context.Context, scope model.Scope, from, to time.Time) ([]model.RejectionReason, error)
}

// RejectionExportService 校验时间窗并读取驳回原因（权限 rejection.export 由路由 Authorize 判定）。
type RejectionExportService struct {
	store   RejectionLister
	maxSpan time.Duration
}

// NewRejectionExportService 校验依赖；maxSpan 须 > 0（config.Approval.RejectionExportMaxSpan）。
func NewRejectionExportService(store RejectionLister, maxSpan time.Duration) (*RejectionExportService, error) {
	if store == nil {
		return nil, errors.New("service: rejection export needs Store")
	}
	if maxSpan <= 0 {
		return nil, errors.New("service: rejection export max span must be > 0")
	}
	return &RejectionExportService{store: store, maxSpan: maxSpan}, nil
}

// Export 返回 created_at ∈ [from, to] 的驳回原因（升序）。from / to 缺失、from 晚于 to、跨度超过上限 → ErrBadInput。
func (s *RejectionExportService) Export(ctx context.Context, scope model.Scope, from, to time.Time) ([]model.RejectionReason, error) {
	switch {
	case from.IsZero() || to.IsZero():
		return nil, fmt.Errorf("%w: from and to required", ErrBadInput)
	case to.Before(from):
		return nil, fmt.Errorf("%w: from after to", ErrBadInput)
	case to.Sub(from) > s.maxSpan:
		return nil, fmt.Errorf("%w: span %s over max %s", ErrBadInput, to.Sub(from), s.maxSpan)
	}
	rows, err := s.store.ListRejections(ctx, scope, from, to)
	if err != nil {
		return nil, fmt.Errorf("service: export rejections: %w", err)
	}
	return rows, nil
}
