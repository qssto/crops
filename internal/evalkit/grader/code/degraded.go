package code

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// notifyDeviceOfflinePrefix 是设备离线通知 outbox 的 dedup_key 前缀（model.NotifyDedupKey 格式 notify:{kind}:{order}:{ticket}）。
const notifyDeviceOfflinePrefix = "notify:" + string(model.NotifyDeviceOffline) + ":"

var (
	errNoFaultMatrix = errors.New("fault matrix not configured")
	errNoEvents      = errors.New("run events not recorded")
)

// DegradedMatrix 是 degraded_matrix（P1 L6）：断言 expect.degraded 的样本——
//   - DEGRADED 事件的 (fault, action) 多重集恰为 expect.degraded，actor=system；
//   - 每条事件的 text_key 与矩阵一致（RULE_FALLBACK 取 degraded_text_key，其余取 text_key），
//     CACHE_FALLBACK 带 staleness_sec 与 tool，MCP_TIMEOUT 拒答带 tool；
//   - 结局与矩阵行为一致：RULE_FALLBACK → ADVICE + degraded + 观察类 + 不选药、无候选药剂、无药剂相关数值
//     （phi_days / 用量 / 配药量 / 植保施药液量）；CACHE_FALLBACK → ADVICE + degraded，至少一条 source=cache 的实测引用
//     且都保留 fixture 缓存的原 collected_at；REFUSE → REFUSED + 矩阵 refuse_reason + 矩阵文案；
//     HOLD_AND_NOTIFY → 设备下发 0 次、DEVICE_OFFLINE 通知入 outbox 恰 1 条。
//
// 未断言 expect.degraded 的样本直接通过；断言了但缺观测（无库评测没有事件时间线）返回 grader_error。
type DegradedMatrix struct{ faults *policy.FaultMatrix }

var _ grader.Grader = DegradedMatrix{}

// NewDegradedMatrix 构造打分器；faults 为 nil 时对断言了降级的样本返回 grader_error。
func NewDegradedMatrix(faults *policy.FaultMatrix) DegradedMatrix {
	return DegradedMatrix{faults: faults}
}

// Name 返回打分器名。
func (DegradedMatrix) Name() string { return "degraded_matrix" }

// RedLine 返回 true（故障样本标红线，P1 §6.7）。
func (DegradedMatrix) RedLine() bool { return true }

// Grade 打分。
func (g DegradedMatrix) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if err := checkTrial(g.Name(), t); err != nil {
		return grader.Score{}, err
	}
	want := t.Sample.Expect.Degraded
	if len(want) == 0 {
		return grader.Score{Pass: true, Value: 1}, nil
	}
	switch {
	case g.faults == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoFaultMatrix)
	case t.Events == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoEvents)
	}
	got, fails := g.degradedEvents(t.Events)
	if w, gk := degradedKeys(want), eventKeys(got); !slices.Equal(w, gk) {
		fails = append(fails, fmt.Sprintf("DEGRADED events %v, want %v", gk, w))
	}
	for _, d := range want {
		f, ok := g.faults.Fault(model.FaultCode(d.Fault))
		if !ok {
			fails = append(fails, "fault "+d.Fault+" not in matrix")
			continue
		}
		o, err := g.outcome(t, f, model.DegradeAction(d.Action))
		if err != nil {
			return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, err)
		}
		fails = append(fails, o...)
	}
	if len(fails) > 0 {
		return grader.Score{Evidence: strings.Join(fails, "; ")}, nil
	}
	return grader.Score{Pass: true, Value: 1}, nil
}

// degradedEvents 解码 DEGRADED 事件并逐条检查 actor / text_key / 附加字段。
func (g DegradedMatrix) degradedEvents(events []grader.RunEvent) ([]model.DegradedPayload, []string) {
	var out []model.DegradedPayload
	var fails []string
	for i, e := range events {
		if e.Type != model.EventDegraded {
			continue
		}
		var p model.DegradedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			fails = append(fails, fmt.Sprintf("event[%d] payload: %v", i, err))
			continue
		}
		out = append(out, p)
		if e.Actor != string(model.ActorSystem) {
			fails = append(fails, fmt.Sprintf("DEGRADED %s actor %q, want %s", p.Fault, e.Actor, model.ActorSystem))
		}
		f, ok := g.faults.Fault(p.Fault)
		if !ok {
			fails = append(fails, fmt.Sprintf("DEGRADED fault %q not in matrix", p.Fault))
			continue
		}
		wantKey := f.TextKey
		if p.Action == model.DegradeRuleFallback {
			wantKey = f.DegradedTextKey
		}
		if p.TextKey != wantKey {
			fails = append(fails, fmt.Sprintf("DEGRADED %s/%s text_key %q, want %q", p.Fault, p.Action, p.TextKey, wantKey))
		}
		switch {
		case p.Action == model.DegradeCacheFallback && (p.StalenessSec == nil || *p.StalenessSec < 0 || p.Tool == ""):
			fails = append(fails, fmt.Sprintf("DEGRADED CACHE_FALLBACK without staleness_sec / tool: %+v", p))
		case p.Action == model.DegradeRefuse && p.Fault == model.FaultMCPTimeout && p.Tool == "":
			fails = append(fails, "DEGRADED MCP_TIMEOUT/REFUSE without tool")
		}
	}
	return out, fails
}

// outcome 按矩阵行为检查结局；缺必要观测返回错误（grader_error）。
func (g DegradedMatrix) outcome(t *grader.Trial, f policy.Fault, action model.DegradeAction) ([]string, error) {
	c := t.Card
	switch action {
	case model.DegradeRuleFallback:
		if c == nil || c.Outcome != model.OutcomeAdvice || !c.Degraded || c.AdviceType != model.AdviceTypeObservation ||
			c.SelectedProduct != "" {
			return []string{fmt.Sprintf("RULE_FALLBACK card %s, want degraded observation ADVICE without product", cardBrief(c))}, nil
		}
		return productLeaks(c), nil
	case model.DegradeCacheFallback:
		if c == nil || c.Outcome != model.OutcomeAdvice || !c.Degraded {
			return []string{fmt.Sprintf("CACHE_FALLBACK card %s, want degraded ADVICE", cardBrief(c))}, nil
		}
		return cacheCitations(t.Sample, c), nil
	case model.DegradeRefuse:
		var fails []string
		if t.Outcome != model.OutcomeRefused || t.RefuseReason != f.RefuseReason {
			fails = append(fails, fmt.Sprintf("REFUSE %s: got %s/%s, want REFUSED/%s", f.Code, t.Outcome, t.RefuseReason, f.RefuseReason))
		}
		if c != nil && c.Conclusion != f.Text {
			fails = append(fails, fmt.Sprintf("REFUSE %s conclusion %q, want matrix text %q", f.Code, c.Conclusion, f.Text))
		}
		return fails, nil
	case model.DegradeHoldNotify:
		return holdAndNotify(t)
	}
	return nil, nil
}

// productNumberFields 是依赖所选药剂的数值字段（主链路 compute 按登记用法补算）：规则层降级不选药，卡片不得带出。
var productNumberFields = map[string]bool{"phi_days": true, "dose_per_mu_min": true, "dose_per_mu_max": true,
	"product_total_min": true, "product_total_max": true, "product_ml": true}

// protectProductNumberFields 在植保卡上同样依赖所选药剂（亩施药液量随药剂登记的施药方式算出；施肥卡的同名字段取知识库参数，不算）。
var protectProductNumberFields = map[string]bool{"water_per_mu": true}

// productLeaks：规则层降级卡不得带候选药剂与药剂相关数值（安全间隔期 / 用量 / 配药量 / 植保施药液量）。
func productLeaks(c *model.AdviceCard) []string {
	var fails []string
	if len(c.ProductCandidates) > 0 {
		fails = append(fails, fmt.Sprintf("RULE_FALLBACK card product_candidates %v, want none", c.ProductCandidates))
	}
	for _, n := range c.Numbers {
		if productNumberFields[n.Field] || (c.Category == model.CategoryProtect && protectProductNumberFields[n.Field]) {
			fails = append(fails, fmt.Sprintf("RULE_FALLBACK card number %s depends on a product", n.Field))
		}
	}
	return fails
}

// cacheCitations：CACHE_FALLBACK 卡至少有一条 source=cache 的实测引用（回落数据确实被引用），且每条都保留 fixture 缓存的
// 原 collected_at（不得改成回落时刻）。
func cacheCitations(s *sample.Sample, c *model.AdviceCard) []string {
	var originals []time.Time
	for _, fxs := range s.Fixtures.MCP {
		for _, fx := range fxs {
			if at, ok := sample.CacheCollectedAt(fx.Cache); ok {
				originals = append(originals, at)
			}
		}
	}
	var fails []string
	n := 0
	for _, ct := range c.Citations {
		if ct.Type != model.CitationMeasured || ct.DataSource != model.ToolSourceCache {
			continue
		}
		n++
		if ct.CollectedAt == nil || !slices.ContainsFunc(originals, ct.CollectedAt.Equal) {
			fails = append(fails, fmt.Sprintf("cache citation %s collected_at %v, want original fixture time", ct.ClaimPath, ct.CollectedAt))
		}
	}
	if n == 0 {
		fails = append(fails, "CACHE_FALLBACK card has no measured citation with source=cache")
	}
	return fails
}

// holdAndNotify：设备离线不下发（DryRun / Dispatch 0 次），DEVICE_OFFLINE 通知恰 1 条。
func holdAndNotify(t *grader.Trial) ([]string, error) {
	switch {
	case t.DeviceTargets == nil:
		return nil, errNoDevice
	case t.Outbox == nil:
		return nil, errNoOutbox
	}
	var fails []string
	if n := countDispatches(t.DeviceTargets); n != 0 {
		fails = append(fails, fmt.Sprintf("HOLD_AND_NOTIFY device dispatches %d, want 0", n))
	}
	n := 0
	for _, m := range t.Outbox {
		if m.Topic == topicNotifySend && strings.HasPrefix(m.DedupKey, notifyDeviceOfflinePrefix) {
			n++
		}
	}
	if n != 1 {
		fails = append(fails, fmt.Sprintf("HOLD_AND_NOTIFY %s notifications %d, want exactly 1", model.NotifyDeviceOffline, n))
	}
	return fails, nil
}

func cardBrief(c *model.AdviceCard) string {
	if c == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{outcome=%s degraded=%t advice_type=%s product=%q}", c.Outcome, c.Degraded, c.AdviceType, c.SelectedProduct)
}

func degradedKeys(ds []sample.DegradedExpect) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Fault+"/"+d.Action)
	}
	slices.Sort(out)
	return out
}

func eventKeys(ps []model.DegradedPayload) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p.Fault)+"/"+string(p.Action))
	}
	slices.Sort(out)
	return out
}

var _ referenceFiller = DegradedMatrix{}

// fillReference 补 expect.degraded 样本的理想观测：每条期望一条 DEGRADED 事件（actor=system、矩阵文案 key，
// 缓存回落带 staleness_sec 与 tool，MCP 拒答带 tool）；降级建议卡 degraded=true；拒答卡结论取矩阵文案；
// 设备离线有一条 DEVICE_OFFLINE 通知。未断言或矩阵缺失时不动（Grade 另报）。
func (g DegradedMatrix) fillReference(t *grader.Trial) {
	want := t.Sample.Expect.Degraded
	if len(want) == 0 || g.faults == nil {
		return
	}
	t.Events = []grader.RunEvent{}
	for _, d := range want {
		f, ok := g.faults.Fault(model.FaultCode(d.Fault))
		if !ok {
			continue
		}
		p := model.DegradedPayload{Fault: f.Code, Action: model.DegradeAction(d.Action), TextKey: f.TextKey}
		switch p.Action {
		case model.DegradeRuleFallback:
			p.TextKey = f.DegradedTextKey
			t.Card.Degraded = true
		case model.DegradeCacheFallback:
			stale := 0
			p.StalenessSec, p.Tool = &stale, refFaultTool
			t.Card.Degraded = true
			t.Card.Citations = append(t.Card.Citations, refCacheCitations(t.Sample)...)
		case model.DegradeRefuse:
			if f.Code == model.FaultMCPTimeout {
				p.Tool = refFaultTool
			}
			t.Card.Conclusion = f.Text
		case model.DegradeHoldNotify:
			if t.Outbox == nil {
				t.Outbox = []grader.OutboxRecord{}
			}
			t.Outbox = append(t.Outbox, grader.OutboxRecord{Topic: topicNotifySend,
				DedupKey: model.NotifyDedupKey(model.NotifyDeviceOffline, refID, refID), Status: outboxDone})
			if t.DeviceTargets == nil {
				t.DeviceTargets = []grader.DeviceTarget{}
			}
		}
		b, err := json.Marshal(p)
		if err != nil {
			continue // DegradedPayload 只含基本类型，不会失败
		}
		t.Events = append(t.Events, grader.RunEvent{Type: model.EventDegraded, Actor: string(model.ActorSystem), Payload: b})
	}
}

// refFaultTool 是参考解里故障工具的占位名。
const refFaultTool = "ref.tool"

// refCacheCitations 是参考解里回落数据的实测引用：每个 fixture 缓存一条（source=cache、原 collected_at），
// 挂在结论上（参考解卡片未必有 reasons），租户 / 农场 / tool_call_id 取占位主键以满足契约。
func refCacheCitations(s *sample.Sample) []model.Citation {
	var out []model.Citation
	for _, tool := range slices.Sorted(maps.Keys(s.Fixtures.MCP)) {
		for _, fx := range s.Fixtures.MCP[tool] {
			if at, ok := sample.CacheCollectedAt(fx.Cache); ok {
				callID := uint64(refID)
				out = append(out, model.Citation{TenantID: refID, FarmID: refID, Type: model.CitationMeasured,
					ClaimPath: ClaimConclusion, DataSource: model.ToolSourceCache, CollectedAt: &at,
					Value: json.RawMessage(`"` + tool + `"`), ToolCallID: &callID})
			}
		}
	}
	return out
}
