package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/model"
)

// CodeConstraintBlocked 是约束拦截的统一错误码（P1 L3）。
const CodeConstraintBlocked = "CONSTRAINT_BLOCKED"

// ErrConstraintBlocked 表示开票前法规前置条件或约束校验未通过：作业单留 DRAFT、不建票、不迁移。
// ApprovalGate.Open 返回 *ConstraintBlockedError（errors.Is 本哨兵为 true）。
var ErrConstraintBlocked = errors.New(CodeConstraintBlocked)

// ConstraintBlockedError 带未通过的 rule_id（按校验顺序：法规前置条件 → 未映射法规条目 → 约束）。
type ConstraintBlockedError struct {
	OrderID uint64
	RuleIDs []string
}

func (e *ConstraintBlockedError) Error() string {
	return CodeConstraintBlocked + ": order " + strconv.FormatUint(e.OrderID, 10) + " rules [" + strings.Join(e.RuleIDs, " ") + "]"
}

// Is 使 errors.Is(err, ErrConstraintBlocked) 成立。
func (e *ConstraintBlockedError) Is(target error) bool { return target == ErrConstraintBlocked }

// LocalOrder 是本地库中同一地块的一张 REAL 作业单（重复作业与安全间隔期判定用）。
type LocalOrder struct {
	OrderID   uint64
	WorkType  model.WorkType
	Payload   json.RawMessage
	Status    model.WorkOrderStatus
	UpdatedAt time.Time
}

// LocalOrderSource 是本地作业单读取接缝（P1 L3）：在途（APPROVED / DISPATCHED / EXECUTING）或 since 之后完成
// （DONE / PARTIALLY_DONE）的 REAL 单，排除 excludeOrderID 本身。实现：repository.LocalOrderRepo，app 注入 graph。
type LocalOrderSource interface {
	RecentOrders(ctx context.Context, scope model.Scope, fieldID, excludeOrderID uint64, since time.Time) ([]LocalOrder, error)
}
