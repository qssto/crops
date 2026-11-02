package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

var errBadLocalOrders = errors.New("env: bad fixtures.local_orders")

// fixtureOrderIDBase 是 fixture 作业单 id 起点（避开 trial 库自增 id，不会被当作本单排除）。
const fixtureOrderIDBase = 900001

// FixtureLocalOrders 是样本 fixtures.local_orders 注入的本地作业单源（P1 L3），实现 agent.LocalOrderSource：
// 按与 repository.LocalOrderRepo 相同的条件过滤（在途，或 since 之后完成），样本不区分地块。
//
//	local_orders:
//	  - {work_type: SPRAY, status: EXECUTING, updated_at: 2026-07-08T08:00:00+08:00, product: 氯虫苯甲酰胺}
type FixtureLocalOrders struct {
	orders []agent.LocalOrder
}

var _ agent.LocalOrderSource = (*FixtureLocalOrders)(nil)

// NewFixtureLocalOrders 解析 fixtures.local_orders（结构校验同 sample.LocalOrderFixture.Problem）；
// order_id 按声明顺序从 fixtureOrderIDBase 起编。
func NewFixtureLocalOrders(fx []sample.LocalOrderFixture) (*FixtureLocalOrders, error) {
	out := &FixtureLocalOrders{orders: make([]agent.LocalOrder, 0, len(fx))}
	for i, f := range fx {
		if p := f.Problem(); p != "" {
			return nil, fmt.Errorf("%w: [%d] %s", errBadLocalOrders, i, p)
		}
		at, err := time.Parse(time.RFC3339, f.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("%w: [%d].updated_at: %w", errBadLocalOrders, i, err)
		}
		payload, err := json.Marshal(map[string]string{"product": f.Product})
		if err != nil {
			return nil, fmt.Errorf("%w: [%d] payload: %w", errBadLocalOrders, i, err)
		}
		out.orders = append(out.orders, agent.LocalOrder{OrderID: fixtureOrderIDBase + uint64(i), WorkType: model.WorkType(f.WorkType),
			Payload: payload, Status: model.WorkOrderStatus(f.Status), UpdatedAt: at.UTC()})
	}
	return out, nil
}

// RecentOrders 返回在途或 since 之后完成的单（排除 excludeOrderID）。
func (f *FixtureLocalOrders) RecentOrders(_ context.Context, _ model.Scope, _, excludeOrderID uint64,
	since time.Time) ([]agent.LocalOrder, error) {
	out := make([]agent.LocalOrder, 0, len(f.orders))
	for _, o := range f.orders {
		if o.OrderID == excludeOrderID {
			continue
		}
		if slices.Contains(sample.LocalOrderInFlight, o.Status) || !o.UpdatedAt.Before(since) {
			out = append(out, o)
		}
	}
	return out, nil
}
