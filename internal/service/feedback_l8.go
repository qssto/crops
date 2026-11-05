package service

import (
	"context"
	"errors"
	"fmt"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// P2 L8 农户采纳 / 差评（§6.9 技术实现 ① 1）：POST /api/v1/farms/{farm_id}/advices/{advice_id}/feedback。
// run_id 取自建议卡（不信请求）；request_id 幂等（uk_request，冲突返回首次结果）；comment 落库前 pii_mask。

// FeedbackStore 是反馈仓储，repository.FeedbackRepo 实现。
type FeedbackStore interface {
	AdviceRun(ctx context.Context, scope model.Scope, adviceID uint64) (uint64, error)
	InsertFeedback(ctx context.Context, scope model.Scope, fb model.AdviceFeedback) (model.AdviceFeedback, bool, error)
}

// FeedbackInput 是一次反馈请求（JSON 字段见 contracts/advice_feedback_request.schema.json）。
type FeedbackInput struct {
	AdviceID  uint64                 `json:"-"`
	Verdict   model.FeedbackVerdict  `json:"verdict"`
	Category  model.RejectReasonCode `json:"category,omitempty"`
	Comment   string                 `json:"comment,omitempty"`
	RequestID string                 `json:"request_id"`
}

// FeedbackResult 是反馈结果；Replayed 表示 request_id 已写过、返回的是首次结果。
type FeedbackResult struct {
	Feedback model.AdviceFeedback `json:"feedback"`
	Replayed bool                 `json:"replayed"`
}

// ErrFeedbackInvalid 是反馈入参非法的错误码（同时是 ErrBadInput）。
var ErrFeedbackInvalid = errors.New("FEEDBACK_INVALID")

// FeedbackDeps 是 FeedbackService 的依赖；Mask 由装配注入 guardrail/pii.Mask。
type FeedbackDeps struct {
	Store FeedbackStore
	Clock clock.Clock
	Mask  func(string) string
}

// FeedbackService 写采纳 / 差评（权限 advice.feedback 由路由 Authorize 判定）。
type FeedbackService struct{ d FeedbackDeps }

// NewFeedbackService 校验依赖。
func NewFeedbackService(d FeedbackDeps) (*FeedbackService, error) {
	if d.Store == nil || d.Clock == nil || d.Mask == nil {
		return nil, errors.New("service: feedback needs Store, Clock and Mask")
	}
	return &FeedbackService{d: d}, nil
}

// Submit 校验并写一条反馈。入参非法 → ErrBadInput + ErrFeedbackInvalid；建议卡不在本 Scope → model.ErrNotFound（404）；
// request_id 已被他场、或同场他人 / 另一建议卡用过 → model.ErrFeedbackRequestConflict（统一 409，不回首次的行）。
func (s *FeedbackService) Submit(ctx context.Context, scope model.Scope, in FeedbackInput) (FeedbackResult, error) {
	if in.AdviceID == 0 {
		return FeedbackResult{}, fmt.Errorf("%w: %w: advice_id required", ErrBadInput, ErrFeedbackInvalid)
	}
	if err := model.ValidateFeedback(in.Verdict, in.Category, in.Comment, in.RequestID); err != nil {
		return FeedbackResult{}, fmt.Errorf("%w: %w: %w", ErrBadInput, ErrFeedbackInvalid, err)
	}
	runID, err := s.d.Store.AdviceRun(ctx, scope, in.AdviceID)
	if err != nil {
		return FeedbackResult{}, fmt.Errorf("service: feedback advice %d: %w", in.AdviceID, err)
	}
	fb := model.AdviceFeedback{AdviceID: in.AdviceID, RunID: runID, UserID: scope.UserID, Verdict: in.Verdict,
		RequestID: in.RequestID, CreatedAt: s.d.Clock.Now()}
	if in.Category != "" {
		c := in.Category
		fb.Category = &c
	}
	if in.Comment != "" {
		masked := s.d.Mask(in.Comment)
		fb.Comment = &masked
	}
	got, created, err := s.d.Store.InsertFeedback(ctx, scope, fb)
	if err != nil {
		return FeedbackResult{}, fmt.Errorf("service: feedback advice %d: %w", in.AdviceID, err)
	}
	return FeedbackResult{Feedback: got, Replayed: !created}, nil
}
