package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// argFieldID 是工具入参里的地块键（MCP 网关与 FixtureMCP 一致）。
const argFieldID = "field_id"

// knowledgeLimit 是 load_knowledge 截断条数。
const knowledgeLimit = 8

// callLLM 调用模型：成功返回 true；模型不可用（含限流、回复非法）返回 false 且不报错；
// ctx 已取消（含租约丢失）时返回错误，run 以失败收尾。
func (a *Agent) callLLM(ctx context.Context, s *state, task llm.Task, in, out any) (bool, error) {
	u, err := a.cfg.LLM.Do(ctx, a.cfg.Tier, task, in, out)
	s.usage.Add(u)
	if err == nil {
		return true, nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return false, fmt.Errorf("%s: %w", task, cause)
	}
	a.log.WarnContext(ctx, "llm unavailable", "run_id", s.runID, "task", task, "err", err)
	return false, nil
}

// classify：问题分类（LLM）。不在范围 → OUT_OF_SCOPE；知识库未覆盖的主体 → KB_MISS；模型不可用 → LLM_UNAVAILABLE。
func (a *Agent) classify(ctx context.Context, s *state) (*state, error) {
	in := llm.ClassifyInput{Question: s.q.Text, Targets: s.env.snap.Registry().Targets}
	for _, c := range s.env.tx.Categories {
		in.Categories = append(in.Categories, c.Code)
		for _, sub := range c.Subcategories {
			in.Topics = append(in.Topics, sub.Code)
		}
	}
	var out llm.ClassifyOutput
	ok, err := a.callLLM(ctx, s, llm.TaskClassify, in, &out)
	if err != nil || !ok {
		s.degrade(llm.TaskClassify)
		return s, err
	}
	s.cls = out
	switch _, known := rules[s.category()]; {
	case !out.InScope:
		s.refuseWith(RefuseOutOfScope, "classified out of scope")
	case out.Unknown:
		s.refuseWith(RefuseKBMiss, "subject not covered by knowledge")
	case !known:
		s.refuseWith(RefuseOutOfScope, fmt.Sprintf("unknown category %q", out.Category))
	}
	return s, nil
}

// enrich：按一级类规则并发调用只读工具。超时 / 空 / 非法返回 → DATA_MISSING；REJECTED 视为该数据不存在。
func (a *Agent) enrich(ctx context.Context, s *state) (*state, error) {
	if s.stopped() {
		return s, nil
	}
	names := rules[s.category()].tools(s)
	args, err := json.Marshal(map[string]uint64{argFieldID: s.q.FieldID})
	if err != nil {
		return s, fmt.Errorf("enrich: encode args: %w", err)
	}
	outs := make([]*toolOut, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = a.callTool(ctx, s, name, args)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return s, fmt.Errorf("enrich: %w", err)
	}
	s.tools = make(map[string]*toolOut, len(outs))
	for _, o := range outs {
		s.tools[o.Tool] = o
		if st := o.Result.Status; (st == statusOK && o.Data == nil) || (st != statusOK && st != statusRejected) {
			s.refuseWith(RefuseDataMissing, fmt.Sprintf("tool %s: %s %s", o.Tool, st, o.Result.ErrorCode))
		}
	}
	if s.stopped() {
		return s, nil
	}
	s.facts = buildFacts(s.tools, s.now, a.loc)
	s.stage, _ = s.facts.str(factStage)
	return s, nil
}

// callTool 调用一个工具并解码数据；只有 ctx 取消或网关内部错误返回 error。
func (a *Agent) callTool(ctx context.Context, s *state, name string, args json.RawMessage) (*toolOut, error) {
	res, err := a.cfg.Tools.Call(ctx, s.q.Scope, agent.ToolCall{RunID: s.runID, StepID: NodeEnrich, Tool: name, Args: args,
		Epoch: runner.Epoch(ctx)}) // L6：重入重跑时带新 resume_epoch，时效工具不读缓存、重取
	if err != nil {
		return nil, fmt.Errorf("tool %s: %w", name, err)
	}
	o := &toolOut{Tool: name, Result: res}
	if res.Status == statusOK {
		if o.Data, err = decodeData(res.Data); err != nil {
			a.log.WarnContext(ctx, "tool data rejected", "run_id", s.runID, "tool", name, "err", err)
		}
	}
	return o, nil
}

// loadKnowledge：渐进式加载（作物 × 区域 × 生育期 × 一级类 × 防治对象）；无作物 / 对象专属条目即 KB_MISS。
func (a *Agent) loadKnowledge(_ context.Context, s *state) (*state, error) {
	if s.stopped() {
		return s, nil
	}
	sit := knowledge.Situation{Crop: s.env.crop, Region: s.env.region, Category: s.cls.Category, Target: s.cls.Target}
	if slices.Contains(knowledge.Stages, s.stage) {
		sit.Stage = s.stage
	}
	hits, err := knowledge.Select(s.env.snap, s.env.tx, sit, knowledgeLimit)
	if errors.Is(err, knowledge.ErrKBMiss) && sit.Stage == "" && sit.Target != "" {
		// 生育期未知（本问未调物候工具）时，按防治对象条目自身的适用期加载；阈值仍由规则层按实测生育期判定。
		if en, terr := s.env.targetEntry(sit.Target); terr == nil && len(en.Stage) > 0 && en.Stage[0] != knowledge.Wildcard {
			sit.Stage = en.Stage[0]
			hits, err = knowledge.Select(s.env.snap, s.env.tx, sit, knowledgeLimit)
		}
	}
	if errors.Is(err, knowledge.ErrKBMiss) {
		s.refuseWith(RefuseKBMiss, err.Error())
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("load knowledge: %w", err)
	}
	s.hits = hits
	return s, nil
}

// candidates：规则层判定（阈值 → 作业 / 观察），植保作业类再按登记表与约束给出药剂候选；候选为空即 KB_MISS。
func (a *Agent) candidates(_ context.Context, s *state) (*state, error) {
	if s.stopped() {
		return s, nil
	}
	if err := rules[s.category()].decide(s.env, s); err != nil {
		return s, s.fail(err)
	}
	if s.category() != model.CategoryProtect || !s.operation() {
		return s, nil
	}
	all, err := s.env.calc.Candidates(s.env.crop, s.dec.Target, s.env.region)
	if err != nil {
		return s, s.fail(err)
	}
	for _, id := range all {
		ok, err := s.env.admits(s, id)
		if err != nil {
			return s, s.fail(err)
		}
		if ok {
			s.candidates = append(s.candidates, id)
		}
	}
	if len(s.candidates) == 0 {
		s.refuseWith(RefuseKBMiss, "no candidate product after constraints for "+s.dec.Target)
	}
	return s, nil
}

// choose：模型在候选集合内选药（LLM）；选中项是否在集合内由 validate 把关。模型不可用则降级。
func (a *Agent) choose(ctx context.Context, s *state) (*state, error) {
	if s.stopped() || len(s.candidates) == 0 {
		return s, nil
	}
	in := llm.ChooseInput{Question: s.q.Text, Target: s.dec.Target}
	for _, id := range s.candidates {
		in.Candidates = append(in.Candidates, llm.Candidate{ID: id, CommonName: s.env.commonName(id)})
	}
	var out llm.ChooseOutput
	ok, err := a.callLLM(ctx, s, llm.TaskChoose, in, &out)
	if err != nil || !ok {
		s.degrade(llm.TaskChoose)
		return s, err
	}
	s.selected = strings.TrimSpace(out.ProductID)
	return s, nil
}

// compute：按一级类规则补算依赖所选药剂的数值（calc，带 trace）。所选药剂不在集合内时不算，交 validate 拒绝。
func (a *Agent) compute(_ context.Context, s *state) (*state, error) {
	fn := rules[s.category()].compute
	if s.stopped() || fn == nil || (s.selected != "" && !slices.Contains(s.candidates, s.selected)) {
		return s, nil
	}
	if err := fn(s.env, s); err != nil {
		return s, s.fail(err)
	}
	return s, nil
}

// explain：模型组织结论（LLM），数值只能写 {{nK}} 占位符；模型不可用则降级为保守文案或拒答。
func (a *Agent) explain(ctx context.Context, s *state) (*state, error) {
	if s.stopped() {
		return s, nil
	}
	in := llm.ExplainInput{Question: s.q.Text, Category: s.cls.Category, AdviceType: string(s.dec.AdviceType),
		Decision: s.dec.Text, Product: s.productName()}
	for _, r := range s.dec.Reasons {
		in.Reasons = append(in.Reasons, noDigits(r.Brief))
	}
	in.Reasons = append(in.Reasons, a.memoryHints(ctx, s)...) // L6 记忆注入（开关关闭为空）
	for i, n := range s.numbers {
		in.Numbers = append(in.Numbers, llm.NumberSlot{Placeholder: placeholder(i), Field: n.Field, Label: noDigits(n.Label)})
	}
	if !s.degraded {
		var out llm.ExplainOutput
		ok, err := a.callLLM(ctx, s, llm.TaskExplain, in, &out)
		if err != nil {
			return s, err
		}
		if ok {
			s.conclusion, s.modelWhy = out.Conclusion, out.Reasons
			return s, nil
		}
		if s.degrade(llm.TaskExplain); s.stopped() {
			return s, nil
		}
	}
	s.conclusion = degradedPrefix + s.dec.Text + degradedSuffix
	return s, nil
}

// 降级文案（不含数字）。
const (
	degradedPrefix = "模型暂不可用，按规则层判定给出保守建议："
	degradedSuffix = "；请人工复核后再安排作业"
)

// placeholder 是第 i 个数值的占位符。
func placeholder(i int) string { return fmt.Sprintf("{{n%d}}", i) }

// productName 是交给模型与工单的物料名：药剂取通用名，非药剂作业取规则层物料名。
func (s *state) productName() string {
	if s.selected != "" && slices.Contains(s.candidates, s.selected) {
		return s.env.commonName(s.selected)
	}
	return s.dec.Product
}

// commonName 取登记表里药剂的通用名，未登记返回 id。
func (e *env) commonName(id string) string {
	for _, p := range e.snap.Registry().Products {
		if p.ID == id && p.CommonName != "" {
			return p.CommonName
		}
	}
	return id
}
