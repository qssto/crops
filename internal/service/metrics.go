package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// MaxMetricDays 是一次查询的最大天数（含首尾）。
const MaxMetricDays = 31

// DateLayout 是指标日期格式（UTC 自然日）。
const DateLayout = "2006-01-02"

const (
	day         = 24 * time.Hour
	percentile  = 0.95
	totalBucket = "total"
)

// MetricFactsReader 读时间窗内的指标事实行，repository.MetricsRepo 实现。
type MetricFactsReader interface {
	Facts(ctx context.Context, scope model.Scope, from, to time.Time) (repository.MetricFacts, error)
}

// Ratio 是一个比率：分母为 0（空数据）时 Value 为 nil。
type Ratio struct {
	Num   int      `json:"num"`
	Den   int      `json:"den"`
	Value *float64 `json:"value"`
}

func ratio(num, den int) Ratio {
	r := Ratio{Num: num, Den: den}
	if den > 0 {
		v := float64(num) / float64(den)
		r.Value = &v
	}
	return r
}

// DayMetrics 是一天（或整个窗口，Date="total"）的指标；口径见 docs/metrics.md。
type DayMetrics struct {
	Date              string     `json:"date"`
	Sessions          int        `json:"sessions"`
	Completion        Ratio      `json:"completion_rate"`
	ToolSuccess       Ratio      `json:"tool_success_rate"`
	ToolP95MS         *int       `json:"tool_p95_latency_ms"`
	Refusal           Ratio      `json:"refusal_rate"`
	ValidationFailure Ratio      `json:"validation_failure_rate"`
	ApprovalWait      WaitStats  `json:"approval_wait"`
	Resume            Ratio      `json:"resume_success_rate"`
	Tokens            TokenStats `json:"tokens"`
	Jobs              JobMetrics `json:"jobs"`
}

// WaitStats 是审批等待时长（OPEN → 决策，秒）；MissingOpen 是缺 APPROVAL_PENDING 事件、无法计时的票数。
type WaitStats struct {
	Decided     int      `json:"decided"`
	AvgSec      *float64 `json:"avg_sec"`
	P95Sec      *float64 `json:"p95_sec"`
	MissingOpen int      `json:"missing_open"`
}

// TokenStats 是 LLM_CALL_END 的 token 求和；PerSession 为总量 / 会话数；BadPayload 是无法解析、未计入的事件数。
type TokenStats struct {
	Prompt     int      `json:"prompt"`
	Completion int      `json:"completion"`
	Total      int      `json:"total"`
	PerSession *float64 `json:"per_session"`
	BadPayload int      `json:"bad_payload"`
}

// JobMetrics 是作业类指标：只统计 dispatch_mode=REAL；DRY_RUN 只计数，不进任何比率。
type JobMetrics struct {
	Orders         int   `json:"orders"`
	DispatchRate   Ratio `json:"dispatch_rate"`
	DoneRate       Ratio `json:"done_rate"`
	DryRunExcluded int   `json:"dry_run_excluded"`
}

// MetricsReport 是 GET /metrics 的结果：按 UTC 自然日的稠密序列（无数据的日子也有一行）+ 窗口合计。
type MetricsReport struct {
	From  string       `json:"from"`
	To    string       `json:"to"`
	Days  []DayMetrics `json:"days"`
	Total DayMetrics   `json:"total"`
}

// MetricsService 计算看板指标（只读生产业务库）。
type MetricsService struct {
	facts MetricFactsReader
	clock clock.Clock
}

// NewMetricsService 构造 MetricsService。
func NewMetricsService(facts MetricFactsReader, c clock.Clock) *MetricsService {
	return &MetricsService{facts: facts, clock: c}
}

// Daily 返回 [from, to] 每个 UTC 自然日的指标（from / to 取所在日，含首尾）；零值取 Clock 的当天。
func (s *MetricsService) Daily(ctx context.Context, scope model.Scope, from, to time.Time) (MetricsReport, error) {
	today := s.clock.Now().UTC().Truncate(day)
	if from.IsZero() {
		from = today
	}
	if to.IsZero() {
		to = today
	}
	from, to = from.UTC().Truncate(day), to.UTC().Truncate(day)
	if to.Before(from) {
		return MetricsReport{}, fmt.Errorf("%w: to %s before from %s", ErrBadInput, to.Format(DateLayout), from.Format(DateLayout))
	}
	if n := int(to.Sub(from)/day) + 1; n > MaxMetricDays {
		return MetricsReport{}, fmt.Errorf("%w: %d days exceeds %d", ErrBadInput, n, MaxMetricDays)
	}
	f, err := s.facts.Facts(ctx, scope, from, to.Add(day))
	if err != nil {
		return MetricsReport{}, fmt.Errorf("service: metrics: %w", err)
	}
	return computeMetrics(f, from, to), nil
}

// bucket 累积一天的原始量。
type bucket struct {
	sessions, completed        int
	validationFailed           int
	resumed, resumedOK         int
	toolOK, toolAll            int
	toolLatency                []int
	advice, refused            int
	waits                      []float64
	decided, missingOpen       int
	prompt, completion         int
	realOrders, realDispatched int
	realDone, dryRun           int
	badPayload                 int
}

// computeMetrics 是纯函数：事实行按日期归桶（run 类按 run 创建日，票按决策日，其余按行创建日）后出指标。
func computeMetrics(f repository.MetricFacts, from, to time.Time) MetricsReport {
	days := make(map[string]*bucket)
	total := &bucket{}
	add := func(t time.Time, fn func(*bucket)) {
		key := t.UTC().Format(DateLayout)
		b, ok := days[key]
		if !ok {
			b = &bucket{}
			days[key] = b
		}
		fn(b)
		fn(total)
	}
	runDay := make(map[uint64]time.Time, len(f.Runs))
	for _, r := range f.Runs {
		runDay[r.ID] = r.CreatedAt
		completed := r.Status == model.RunCompleted
		add(r.CreatedAt, func(b *bucket) {
			b.sessions++
			if completed {
				b.completed++
			}
		})
	}
	for runID, ev := range runEventFacts(f.Events) {
		at, ok := runDay[runID]
		if !ok {
			continue
		}
		add(at, func(b *bucket) {
			if ev.validationFailed {
				b.validationFailed++
			}
			if ev.resumed {
				b.resumed++
				if ev.completedAfterResume {
					b.resumedOK++
				}
			}
			b.prompt += ev.prompt
			b.completion += ev.completion
			b.badPayload += ev.badPayload
		})
	}
	for _, c := range f.ToolCalls {
		ok, lat := c.Status == model.ToolStatusOK, c.LatencyMS
		add(c.CreatedAt, func(b *bucket) {
			b.toolAll++
			if ok {
				b.toolOK++
			}
			b.toolLatency = append(b.toolLatency, lat)
		})
	}
	for _, a := range f.Advice {
		refused := a.Outcome == model.OutcomeRefused
		add(a.CreatedAt, func(b *bucket) {
			b.advice++
			if refused {
				b.refused++
			}
		})
	}
	for _, t := range f.Tickets {
		opened := t.OpenedAt
		wait := 0.0
		if opened != nil {
			wait = t.DecidedAt.Sub(*opened).Seconds()
		}
		add(t.DecidedAt, func(b *bucket) {
			b.decided++
			if opened == nil {
				b.missingOpen++
				return
			}
			b.waits = append(b.waits, wait)
		})
	}
	for _, o := range f.Orders {
		add(o.CreatedAt, func(b *bucket) {
			if o.DispatchMode != string(model.DispatchReal) {
				b.dryRun++
				return
			}
			b.realOrders++
			if o.Dispatched {
				b.realDispatched++
			}
			if o.Status == model.OrderDone || o.Status == model.OrderPartiallyDone {
				b.realDone++
			}
		})
	}
	rep := MetricsReport{From: from.Format(DateLayout), To: to.Format(DateLayout), Total: total.metrics(totalBucket)}
	for d := from; !d.After(to); d = d.Add(day) {
		key := d.Format(DateLayout)
		b, ok := days[key]
		if !ok {
			b = &bucket{}
		}
		rep.Days = append(rep.Days, b.metrics(key))
	}
	return rep
}

func (b *bucket) metrics(date string) DayMetrics {
	m := DayMetrics{Date: date, Sessions: b.sessions,
		Completion:        ratio(b.completed, b.sessions),
		ToolSuccess:       ratio(b.toolOK, b.toolAll),
		Refusal:           ratio(b.refused, b.advice),
		ValidationFailure: ratio(b.validationFailed, b.sessions),
		Resume:            ratio(b.resumedOK, b.resumed),
		Tokens: TokenStats{Prompt: b.prompt, Completion: b.completion, Total: b.prompt + b.completion,
			BadPayload: b.badPayload},
		ApprovalWait: WaitStats{Decided: b.decided, MissingOpen: b.missingOpen},
		Jobs: JobMetrics{Orders: b.realOrders, DispatchRate: ratio(b.realDispatched, b.realOrders),
			DoneRate: ratio(b.realDone, b.realOrders), DryRunExcluded: b.dryRun},
	}
	if len(b.toolLatency) > 0 {
		lat := make([]float64, len(b.toolLatency))
		for i, v := range b.toolLatency {
			lat[i] = float64(v)
		}
		p := int(nearestRank(lat, percentile))
		m.ToolP95MS = &p
	}
	if b.sessions > 0 {
		v := float64(m.Tokens.Total) / float64(b.sessions)
		m.Tokens.PerSession = &v
	}
	if len(b.waits) > 0 {
		sum := 0.0
		for _, w := range b.waits {
			sum += w
		}
		avg, p95 := sum/float64(len(b.waits)), nearestRank(b.waits, percentile)
		m.ApprovalWait.AvgSec, m.ApprovalWait.P95Sec = &avg, &p95
	}
	return m
}

// nearestRank 是最近秩百分位：排序后取第 ceil(p·n) 个（1 起），与 docs/metrics.md 的 SQL 一致。v 非空。
func nearestRank(v []float64, p float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	idx := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(idx, 0)]
}

// runFacts 是一个 run 的事件归纳。
type runFacts struct {
	validationFailed     bool
	resumed              bool
	completedAfterResume bool // 存在 seq 大于某条 RUN_RESUMED 的 RUN_COMPLETED
	prompt, completion   int
	badPayload           int
}

// runEventFacts 按 run 归纳事件（events 已按 run_id, seq 排序；乱序也成立：只看首个 RUN_RESUMED 与末个 RUN_COMPLETED）。
func runEventFacts(events []repository.MetricEvent) map[uint64]*runFacts {
	out := make(map[uint64]*runFacts)
	firstResume := make(map[uint64]int)
	lastComplete := make(map[uint64]int)
	for _, e := range events {
		rf, ok := out[e.RunID]
		if !ok {
			rf = &runFacts{}
			out[e.RunID] = rf
		}
		switch e.Type {
		case model.EventValidationFailed:
			rf.validationFailed = true
		case model.EventRunResumed:
			if s, ok := firstResume[e.RunID]; !ok || e.Seq < s {
				firstResume[e.RunID] = e.Seq
			}
			rf.resumed = true
		case model.EventRunCompleted:
			if e.Seq > lastComplete[e.RunID] {
				lastComplete[e.RunID] = e.Seq
			}
		case model.EventLLMCallEnd:
			var p model.LLMCallPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				rf.badPayload++
				continue
			}
			rf.prompt += p.PromptTokens
			rf.completion += p.CompletionTokens
		}
	}
	for runID, s := range firstResume {
		out[runID].completedAfterResume = lastComplete[runID] > s
	}
	return out
}
