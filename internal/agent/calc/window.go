package calc

import (
	"slices"
	"time"
)

// 作业窗口筛选（P2 L4 下沉项 operation_window）：common/operation_window 的硬约束（风速、降雨、气温）由规则层逐条判定，
// 这里把判定结果汇成窗口结论；output_validator 据此拒收模型结论里窗口外的作业时间（合入即常开）。

// WindowRuleVer 是作业窗口规则版本（rule_shadow_diff.rule_ver）。
const WindowRuleVer = "op-window-v1"

// WindowCheck 是一条已判定的窗口硬约束（阈值 id 与是否满足）。
type WindowCheck struct {
	ID  string `json:"id"`
	Met bool   `json:"met"`
}

// Window 是作业窗口结论：Evaluated 表示至少判定过一条硬约束；Open 表示全部满足，此时 [From, To] 为可作业时段；
// Blocked 是未满足的约束 id（去重排序）。
type Window struct {
	Evaluated bool      `json:"evaluated"`
	Open      bool      `json:"open"`
	Blocked   []string  `json:"blocked,omitempty"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
}

// OperationWindow 汇总已判定的硬约束；from / to 是作业单建议时段（assemble 的 suggested_window）。
func OperationWindow(checks []WindowCheck, from, to time.Time) Window {
	w := Window{Evaluated: len(checks) > 0, From: from, To: to}
	for _, c := range checks {
		if !c.Met && !slices.Contains(w.Blocked, c.ID) {
			w.Blocked = append(w.Blocked, c.ID)
		}
	}
	slices.Sort(w.Blocked)
	w.Open = w.Evaluated && len(w.Blocked) == 0
	return w
}

// AdmitsDay 判断相对 From 所在本地日历日偏移 offset 天的那一天能否作业：窗口打开且该日与 [From, To] 相交。
func (w Window) AdmitsDay(offset int, loc *time.Location) bool {
	return w.Open && w.Covers(offset, loc)
}

// Covers 判断相对 From 所在本地日历日偏移 offset 天的那一天是否与 [From, To] 相交（不看窗口是否打开）：
// 窗口关闭时，落在判定时段内的作业时间即与硬约束冲突。
func (w Window) Covers(offset int, loc *time.Location) bool {
	if offset < 0 {
		return false
	}
	y, m, d := w.From.In(loc).Date()
	start := time.Date(y, m, d+offset, 0, 0, 0, 0, loc)
	return !start.After(w.To)
}
