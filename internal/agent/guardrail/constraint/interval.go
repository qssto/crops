package constraint

import (
	"fmt"
	"math"
	"strings"
	"time"

	"agri-steward/internal/model"
)

// 农事记录字段（field.history 与评测 fixture）。
const (
	keyOperations    = "operations"
	keyLastSpray     = "last_spray"
	keySprays        = "sprays"
	keyLastFertilize = "last_fertilize"
	keyDate          = "date"
	keyKind          = "kind"
	keyProduct       = "product"
	keyHarvest       = "expected_harvest"
	keyOrderID       = "order_id"
	unitDay          = "天"
)

// opKind 是作业类型在 field.history operations[].kind 中的取值。
var opKind = map[model.WorkType]string{model.WorkSpray: "spray", model.WorkFertilize: "fertilize", model.WorkIrrigate: "irrigate"}

// op 是一条已发生 / 在途的同地块作业。
type op struct {
	Kind    string
	Date    time.Time // 本地日历日零点
	Product string
	Origin  string // history / order:<id>
	Flight  bool   // 本地在途单（APPROVED / DISPATCHED / EXECUTING）
	OrderID uint64 // 农事记录里的 order_id（来自本系统作业单）或本地单 id；0 表示未知
}

// day 是 t 在 loc 下的日历日零点。
func day(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

// days 是 from → to 的日历日差：取各自（已在农场时区下的）年月日，按 UTC 零点相减，不受夏令时 / 时区偏移影响。
func days(from, to time.Time) int {
	f := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	t := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(t.Sub(f).Hours() / 24)
}

func parseDay(v any, loc *time.Location) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, loc); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return day(t, loc), true
	}
	return time.Time{}, false
}

// historyOps 展开农事记录：operations[]、last_spray、sprays[]、last_fertilize（日期无法解析的条目忽略）。
// 条目可带 order_id（本系统作业单回写的记录），供与本地单去重。
func historyOps(data map[string]any, loc *time.Location) []op {
	var out []op
	add := func(v any, kind string) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		d, ok := parseDay(m[keyDate], loc)
		if !ok {
			return
		}
		if k, ok := m[keyKind].(string); ok && k != "" {
			kind = k
		}
		p, _ := m[keyProduct].(string) // 非字符串即无药剂名（施药类由 PHI 判失败）
		x := op{Kind: kind, Date: d, Product: p, Origin: "history"}
		if id, ok := m[keyOrderID].(float64); ok && id > 0 && id == math.Trunc(id) {
			x.OrderID = uint64(id)
			x.Origin = fmt.Sprintf("history(order:%d)", x.OrderID)
		}
		out = append(out, x)
	}
	if arr, ok := data[keyOperations].([]any); ok {
		for _, x := range arr {
			add(x, "")
		}
	}
	add(data[keyLastSpray], opKind[model.WorkSpray])
	if arr, ok := data[keySprays].([]any); ok {
		for _, x := range arr {
			add(x, opKind[model.WorkSpray])
		}
	}
	add(data[keyLastFertilize], opKind[model.WorkFertilize])
	return out
}

// priorOps 合并农事记录与本地单，并按 order_id 去重：同一作业单既回写进农事记录又在本地库时只保留本地单
// （状态更准：在途 / 完成）。
func priorOps(history map[string]any, in *Input) []op {
	local := localOps(in)
	seen := make(map[uint64]bool, len(local))
	for _, o := range local {
		seen[o.OrderID] = true
	}
	out := make([]op, 0, len(local))
	for _, o := range historyOps(history, in.Loc) {
		if o.OrderID != 0 && seen[o.OrderID] {
			continue
		}
		out = append(out, o)
	}
	return append(out, local...)
}

// localOps 把本地 REAL 单转为作业：在途单按今日计，完成单按 updated_at 计。
func localOps(in *Input) []op {
	today := day(in.Now, in.Loc)
	out := make([]op, 0, len(in.LocalOrders))
	for _, o := range in.LocalOrders {
		x := op{Kind: opKind[o.WorkType], Date: day(o.UpdatedAt, in.Loc), Product: payloadProduct(o.Payload),
			Origin: fmt.Sprintf("order:%d", o.OrderID), OrderID: o.OrderID}
		switch o.Status {
		case model.OrderApproved, model.OrderDispatched, model.OrderExecuting:
			x.Date, x.Flight = today, true
		case model.OrderDone, model.OrderPartiallyDone:
		default:
			continue
		}
		out = append(out, x)
	}
	return out
}

// phiChecker：药剂安全间隔期。最早可收获日 = max(今日 + 本药 PHI, 历史 / 本地同地块施药日 + 该药 PHI)，
// 余量 = 预计收获日 − 最早可收获日（日历日），知识阈值 interval#phi-margin（≥ 0 天）。差 1 天即不通过。
// PHI 不适用（登记 phi_days 为空，如浸种、播后封闭）的药只是不贡献最早收获日：本药不适用时基准取今日，历史仍逐条计入，
// 历史中不适用的药在说明里列出。
// 失败关闭：农事记录不可用、本地单未读取、历史 / 本地施药药剂名为空或在登记表中查不到（含无本作物登记），均不通过（说明列出未识别药剂）。
type phiChecker struct{}

func (phiChecker) RuleID() string { return RulePHI }

func (phiChecker) Check(in *Input) Result {
	th, ok := threshold(in, KBInterval, thPHIMargin)
	if !ok {
		return fail(th, Observed{Unit: unitDay}, "threshold missing in knowledge")
	}
	p := in.Product
	if p == nil {
		return fail(th, Observed{Unit: unitDay}, "no product selected")
	}
	harvest, src, note := harvestDay(in)
	obs := observed(src, nil, unitDay)
	if note != "" {
		return fail(th, obs, note)
	}
	hist, problem := source(in, ToolHistory)
	switch {
	case problem != "":
		return fail(th, obs, "prior sprays unknown: "+problem)
	case !in.LocalLoaded:
		return fail(th, obs, "prior sprays unknown: local orders not loaded")
	}
	earliest, why := day(in.Now, in.Loc), p.CommonName+" phi not applicable"
	if p.PHIDays != nil {
		earliest, why = earliest.AddDate(0, 0, *p.PHIDays), p.CommonName
	}
	var unknown, notApplicable []string
	for _, o := range priorOps(hist.Data, in) {
		if o.Kind != opKind[model.WorkSpray] {
			continue
		}
		var d *int
		known := false
		if o.Product != "" && in.PHIOf != nil {
			d, known = in.PHIOf(o.Product)
		}
		switch {
		case !known:
			unknown = append(unknown, fmt.Sprintf("%q@%s", o.Product, o.Origin))
		case d == nil:
			notApplicable = append(notApplicable, o.Product+"@"+o.Origin)
		default:
			if e := o.Date.AddDate(0, 0, *d); e.After(earliest) {
				earliest, why = e, o.Product+"@"+o.Origin
			}
		}
	}
	if len(unknown) > 0 {
		return fail(th, obs, "prior spray product not recognized: "+strings.Join(unknown, ", "))
	}
	margin := days(earliest, harvest)
	obs.Value = margin
	obs.Note = fmt.Sprintf("harvest %s, earliest %s (%s)", harvest.Format(time.DateOnly), earliest.Format(time.DateOnly), why)
	if len(notApplicable) > 0 {
		obs.Note += "; phi not applicable: " + strings.Join(notApplicable, ", ")
	}
	return Result{Passed: compare(th.Op, float64(margin), th.Value), Observed: obs, Threshold: th}
}

// harvestDay 取预计收获日：先 field.profile，再 crop.phenology；须来自 OK 且带 tool_call_id 的结果。
func harvestDay(in *Input) (time.Time, Source, string) {
	var last Source
	for _, tool := range []string{ToolProfile, ToolPhenology} {
		src, problem := source(in, tool)
		last = src
		if problem != "" {
			continue
		}
		if d, ok := parseDay(src.Data[keyHarvest], in.Loc); ok {
			return d, src, ""
		}
	}
	return time.Time{}, last, "expected_harvest missing"
}

// repeatChecker：同地块同类作业重复。本地在途单直接不通过；否则距最近一次同类作业（农事记录 + 本地完成单）的天数
// 须满足 interval#repeat-*（施药 ≥ 3 天、追肥 ≥ 7 天、灌溉 ≥ 1 天）。农事记录或本地单读取失败即不通过。
type repeatChecker struct{}

func (repeatChecker) RuleID() string { return RuleRepeat }

var repeatThreshold = map[model.WorkType]string{model.WorkSpray: thRepeatSpray, model.WorkFertilize: thRepeatFert,
	model.WorkIrrigate: thRepeatIrrigate}

func (repeatChecker) Check(in *Input) Result {
	thID, known := repeatThreshold[in.WorkType]
	th, ok := threshold(in, KBInterval, thID)
	src, problem := source(in, ToolHistory)
	obs := observed(src, nil, unitDay)
	switch {
	case !known:
		return fail(th, obs, "no repeat window for work type "+string(in.WorkType))
	case !ok:
		return fail(th, obs, "threshold missing in knowledge")
	case problem != "":
		return fail(th, obs, problem)
	case !in.LocalLoaded:
		return fail(th, obs, "local orders not loaded")
	}
	kind, today := opKind[in.WorkType], day(in.Now, in.Loc)
	var last *op
	for _, o := range priorOps(src.Data, in) {
		if o.Kind != kind {
			continue
		}
		if o.Flight {
			obs.Value, obs.Note = 0, "in-flight local "+o.Origin
			return Result{Passed: false, Observed: obs, Threshold: th}
		}
		if last == nil || o.Date.After(last.Date) {
			last = &o
		}
	}
	if last == nil {
		obs.Note = noteNoPrevious + kind
		return Result{Passed: true, Observed: obs, Threshold: th}
	}
	gap := days(last.Date, today)
	obs.Value, obs.Note = gap, fmt.Sprintf("last %s %s (%s)", kind, last.Date.Format(time.DateOnly), last.Origin)
	return Result{Passed: compare(th.Op, float64(gap), th.Value), Observed: obs, Threshold: th}
}
