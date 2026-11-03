package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L7-B 告警列表（规则 annotation 的下钻入口）：按农场、告警名、时间倒序列出告警审计行，每行带 run 详情或作业单链接。
// 只读 ALERT.{name} 审计（经 AlertSink 写入）；cross_farm_denied / flag_changed 另含业务审计 CROSS_FARM_DENIED / FLAG_CHANGE。
// 业务审计的 summary 不外露（可能含操作者信息），只给 action / target / result。

// AlertListStore 读告警审计行，repository.AlertRepo 实现。
type AlertListStore interface {
	ListAlerts(ctx context.Context, scope model.Scope, actions []string, from, to time.Time, before repository.AlertCursor,
		limit int) ([]repository.AlertRow, error)
}

const (
	// DefaultAlertListSpan 是缺省回看时长（from 缺省时 = to - 24h）。
	DefaultAlertListSpan = 24 * time.Hour
	// MaxAlertListSpan 是 [from, to) 最大跨度（与驳回导出缺省上限一致：31 天）。
	MaxAlertListSpan = 744 * time.Hour
	// DefaultAlertListLimit / MaxAlertListLimit 是返回行数缺省值与上限。
	DefaultAlertListLimit = 50
	MaxAlertListLimit     = 200
)

// AlertQuery 是告警列表查询；Name 为空表示全部告警项，From / To 零值取缺省，Limit 0 取缺省；
// Cursor 是上一页返回的 next_cursor（空为第一页；翻页时 from / to / alert 须与第一页一致）。
type AlertQuery struct {
	Name     model.AlertName
	From, To time.Time
	Limit    int
	Cursor   string
}

// AlertItem 是告警列表的一行。RunLink / OrderLink 是下钻地址（run 详情 API / 作业单所在 run 的详情）。
type AlertItem struct {
	ID        uint64           `json:"id"`
	CreatedAt time.Time        `json:"created_at"`
	Alert     model.AlertName  `json:"alert"`
	Level     model.AlertLevel `json:"level,omitempty"`
	Action    string           `json:"action"`
	Target    string           `json:"target,omitempty"`
	Result    string           `json:"result"`
	RunID     uint64           `json:"run_id,omitempty"`
	OrderID   uint64           `json:"order_id,omitempty"`
	Detail    string           `json:"detail,omitempty"`
	RunLink   string           `json:"run_link,omitempty"`
}

// AlertList 是 GET /alerts 的结果；NextCursor 非空表示还有下一页（作为 cursor 参数原样传回）。
type AlertList struct {
	From       time.Time   `json:"from"`
	To         time.Time   `json:"to"`
	Items      []AlertItem `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// AlertService 读告警列表。
type AlertService struct {
	store AlertListStore
	clock clock.Clock
}

// NewAlertService 构造告警列表服务。
func NewAlertService(store AlertListStore, c clock.Clock) (*AlertService, error) {
	if store == nil || c == nil {
		return nil, errors.New("service: alert list needs store and clock")
	}
	return &AlertService{store: store, clock: c}, nil
}

// List 校验查询并读本农场告警（时间倒序，键集分页）。告警名未登记、跨度非法或超限、游标非法 → ErrBadInput。
// 多取一行判断是否有下一页；有则 NextCursor = 本页最后一行的 (created_at, id)。
func (s *AlertService) List(ctx context.Context, scope model.Scope, q AlertQuery) (AlertList, error) {
	if q.Name != "" && !q.Name.Known() {
		return AlertList{}, fmt.Errorf("%w: unknown alert %q", ErrBadInput, q.Name)
	}
	cur, err := ParseAlertCursor(q.Cursor)
	if err != nil {
		return AlertList{}, err
	}
	to := q.To
	if to.IsZero() {
		to = s.clock.Now()
	}
	from := q.From
	if from.IsZero() {
		from = to.Add(-DefaultAlertListSpan)
	}
	if !from.Before(to) {
		return AlertList{}, fmt.Errorf("%w: from must be before to", ErrBadInput)
	}
	if to.Sub(from) > MaxAlertListSpan {
		return AlertList{}, fmt.Errorf("%w: span exceeds %s", ErrBadInput, MaxAlertListSpan)
	}
	limit := q.Limit
	switch {
	case limit == 0:
		limit = DefaultAlertListLimit
	case limit < 0 || limit > MaxAlertListLimit:
		return AlertList{}, fmt.Errorf("%w: limit must be 1..%d", ErrBadInput, MaxAlertListLimit)
	}
	rows, err := s.store.ListAlerts(ctx, scope, alertActions(q.Name), from, to, cur, limit+1)
	if err != nil {
		return AlertList{}, fmt.Errorf("service: list alerts: %w", err)
	}
	out := AlertList{From: from, To: to}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		out.NextCursor = FormatAlertCursor(repository.AlertCursor{At: last.CreatedAt, ID: last.ID})
	}
	out.Items = make([]AlertItem, 0, len(rows))
	for _, r := range rows {
		out.Items = append(out.Items, alertItem(scope.FarmID, r))
	}
	return out, nil
}

// FormatAlertCursor 把 (created_at, id) 编成游标「{unix 毫秒}_{id}」（audit_log.created_at 是 datetime(3)，毫秒无损）。
func FormatAlertCursor(c repository.AlertCursor) string {
	return strconv.FormatInt(c.At.UnixMilli(), 10) + "_" + strconv.FormatUint(c.ID, 10)
}

// ParseAlertCursor 解析游标；空串为第一页（零值）。格式不对、毫秒非正或 id 为 0 → ErrBadInput。
func ParseAlertCursor(s string) (repository.AlertCursor, error) {
	if s == "" {
		return repository.AlertCursor{}, nil
	}
	ms, id, ok := strings.Cut(s, "_")
	if !ok {
		return repository.AlertCursor{}, fmt.Errorf("%w: bad cursor", ErrBadInput)
	}
	at, err := strconv.ParseInt(ms, 10, 64)
	if err != nil || at <= 0 {
		return repository.AlertCursor{}, fmt.Errorf("%w: bad cursor time", ErrBadInput)
	}
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n == 0 {
		return repository.AlertCursor{}, fmt.Errorf("%w: bad cursor id", ErrBadInput)
	}
	return repository.AlertCursor{At: time.UnixMilli(at).UTC(), ID: n}, nil
}

// alertActions 返回要检索的审计 action（name 为空时取全部告警项）。
func alertActions(name model.AlertName) []string {
	names := model.AlertNames
	if name != "" {
		names = []model.AlertName{name}
	}
	var out []string
	for _, n := range names {
		for _, a := range n.ListActions() {
			out = append(out, string(a))
		}
	}
	return out
}

// businessAlert 把业务审计 action 映射回告警项。
var businessAlert = map[string]model.AlertName{
	string(model.AuditCrossFarmDenied): model.AlertCrossFarmDenied,
	string(model.AuditFlagChange):      model.AlertFlagChanged,
}

func alertItem(farmID uint64, r repository.AlertRow) AlertItem {
	it := AlertItem{ID: r.ID, CreatedAt: r.CreatedAt, Action: r.Action, Target: r.Target, Result: r.Result}
	if r.RunID != nil {
		it.RunID = *r.RunID
	}
	if name, ok := strings.CutPrefix(r.Action, model.AlertActionPrefix); ok {
		it.Alert = model.AlertName(name)
		var sum alertSummary
		if json.Unmarshal(r.Summary, &sum) == nil {
			it.Level, it.OrderID, it.Detail = sum.Level, sum.OrderID, sum.Detail
			if it.RunID == 0 {
				it.RunID = sum.RunID
			}
		}
	} else {
		it.Alert = businessAlert[r.Action]
	}
	if it.RunID != 0 {
		it.RunLink = "/api/v1/farms/" + strconv.FormatUint(farmID, 10) + "/runs/" + strconv.FormatUint(it.RunID, 10)
	}
	return it
}
