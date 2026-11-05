package graph

// P2 L5 知识加载策略（§6.6 技术实现 1-3）：进入 load_knowledge 时按农场读 enable_loader_v1，开取 v1、
// 否则 v0；策略写 agent_run.loader_strategy，加载结果发 KB_LOADED 并同步批量写 kb_load_item。
// 开关读失败、v1 未装配一律按 v0（失败关闭）；写库失败只记日志，不让 run 失败。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/model"
)

// KnowledgeLoading 是 Config.Loading：加载策略与明细写入，均可空。
type KnowledgeLoading struct {
	V0    *knowledge.Strategy   // 开关关时的策略；空取 knowledge.PhaseZeroSpec（与 P1 Select 逐条一致）
	V1    *knowledge.Strategy   // 开关开时的策略；空时开关开也用 V0 并告警
	Items agent.KBLoadItemStore // kb_load_item；空则不写明细
}

// loadSeqFirst 是 run 内第一次加载的 load_seq：主链路每个 run 只加载一次；resume 重跑时整批替换该 load_seq 的旧明细
// （ReplaceKBLoadItems），开关在中断期间切换也只留最后一次策略的行。
const loadSeqFirst = 0

// phaseZero 是未装配 Loading.V0 时的 v0 策略。
var phaseZero = mustStrategy(knowledge.PhaseZeroSpec())

func mustStrategy(spec knowledge.StrategySpec) *knowledge.Strategy {
	s, err := knowledge.NewStrategy(spec, nil)
	if err != nil {
		panic(fmt.Sprintf("graph: built-in loader strategy %s: %v", spec.Version, err))
	}
	return s
}

// loaderStrategy 按农场开关选策略：读失败（含未装配开关）与 v1 未装配均回落 v0。
func (a *Agent) loaderStrategy(ctx context.Context, s *state) *knowledge.Strategy {
	v0, l := phaseZero, a.cfg.Loading
	if l != nil && l.V0 != nil {
		v0 = l.V0
	}
	on, err := agent.FlagOn(ctx, a.cfg.Flags, model.FlagEnableLoaderV1, s.q.Scope)
	if err != nil && !agent.IsNoFlagReader(err) {
		a.log.WarnContext(ctx, "read enable_loader_v1 failed, using v0", "run_id", s.runID, "err", err)
	}
	if !on {
		return v0
	}
	if l == nil || l.V1 == nil {
		a.log.WarnContext(ctx, "enable_loader_v1 on but v1 strategy not configured, using v0", "run_id", s.runID)
		return v0
	}
	return l.V1
}

// warnUnmatchedRules：本类别有必加规则在整个快照中一个条目都匹配不到时告警（启动校验对照的是装配时的快照，
// 这里兜住运行时快照与之不同的情形；运行时不报错，按「情境不符」继续加载）。
func (a *Agent) warnUnmatchedRules(ctx context.Context, s *state, st *knowledge.Strategy, category string) {
	if un := st.UnmatchedRules(s.env.snap, category); len(un) > 0 {
		a.log.WarnContext(ctx, "must_include rules match no knowledge entry", "run_id", s.runID, "strategy", st.Version(),
			"category", category, "rules", un, "kb_ref", s.env.snap.Ref())
	}
}

// selectKnowledge 按策略选条目；无生育期且带 target 时 KB_MISS 回退到 target 条目的首个生育期再选一次（P1 口径）。
func (a *Agent) selectKnowledge(s *state, st *knowledge.Strategy, sit knowledge.Situation) (knowledge.Situation, []knowledge.Pick, error) {
	picks, err := st.Select(s.env.snap, s.env.tx, sit)
	if isKBMiss(err) && sit.Stage == "" && sit.Target != "" {
		if en, terr := s.env.targetEntry(sit.Target); terr == nil && len(en.Stage) > 0 && en.Stage[0] != knowledge.Wildcard {
			sit.Stage = en.Stage[0]
			picks, err = st.Select(s.env.snap, s.env.tx, sit)
		}
	}
	return sit, picks, err
}

// recordKnowledgeLoad 写 loader_strategy 与 kb_load_item 并发 KB_LOADED。runner 外（下发前复检）不写库：
// 复检不写 run 侧数据（recheck.go），KB_LOADED 由 emitStrict 自行跳过。
// 首次执行用 v0 时不写 loader_strategy（列默认值即 v0，开关关时不多一次 UPDATE）；resume（epoch > 0）总写，
// 覆盖中断前可能记下的 v1。
func (a *Agent) recordKnowledgeLoad(ctx context.Context, s *state, st *knowledge.Strategy, sit knowledge.Situation, picks []knowledge.Pick) {
	if runner.Alive(ctx) == nil {
		if st.Version() != model.LoaderStrategyV0 || runner.Epoch(ctx) > 0 {
			a.setLoaderStrategy(ctx, s, st.Version())
		}
		a.insertKBLoadItems(ctx, s, st.Version(), picks)
	}
	a.emitKBLoaded(ctx, s, kbLoadedPayload(a.cfg.KBRef, s.cls.Category, st.Version(), sit, picks))
}

func (a *Agent) setLoaderStrategy(ctx context.Context, s *state, version string) {
	if a.cfg.Usage == nil {
		return
	}
	if err := a.cfg.Usage.SetLoaderStrategy(context.WithoutCancel(ctx), s.q.Scope, s.runID, version); err != nil {
		a.log.WarnContext(ctx, "set loader_strategy", "run_id", s.runID, "strategy", version, "err", err)
	}
}

func (a *Agent) insertKBLoadItems(ctx context.Context, s *state, version string, picks []knowledge.Pick) {
	if a.cfg.Loading == nil || a.cfg.Loading.Items == nil || len(picks) == 0 {
		return
	}
	items := kbLoadItems(s.q.Scope, s.runID, s.cls.Category, version, a.cfg.Clock.Now(), picks)
	if err := a.cfg.Loading.Items.ReplaceKBLoadItems(context.WithoutCancel(ctx), s.q.Scope, items); err != nil {
		a.log.WarnContext(ctx, "replace kb_load_item", "run_id", s.runID, "n", len(items), "err", err)
	}
}

func isKBMiss(err error) bool { return errors.Is(err, knowledge.ErrKBMiss) }

// kbLoadedPayload 组装 KB_LOADED：Entries 保持 P1 口径（kb:<kb_id>@<version>，按名次），其余为 L5 追加字段。
func kbLoadedPayload(kbRef, category, version string, sit knowledge.Situation, picks []knowledge.Pick) model.KBLoadedPayload {
	p := model.KBLoadedPayload{KBRef: kbRef, Entries: make([]string, 0, len(picks)), QuestionType: category,
		Situation: &model.KBSituation{Crop: sit.Crop, Stage: sit.Stage, Region: sit.Region, Topic: sit.Topic, Target: sit.Target,
			RankTopic: sit.RankTopic},
		Items: make([]model.KBLoadedEntry, 0, len(picks)), Strategy: version, LoadSeq: loadSeqFirst}
	for _, pk := range picks {
		ref := pk.Entry.Ref()
		p.Entries = append(p.Entries, ref)
		p.Items = append(p.Items, model.KBLoadedEntry{Ref: ref, PickReason: string(pk.Reason), Rank: pk.Rank})
	}
	return p
}

// kbLoadItems 把一次加载转为 kb_load_item 行（一个条目一行，rank_no = 名次）。
func kbLoadItems(scope model.Scope, runID uint64, category, version string, now time.Time, picks []knowledge.Pick) []model.KBLoadItem {
	out := make([]model.KBLoadItem, 0, len(picks))
	for _, pk := range picks {
		out = append(out, model.KBLoadItem{TenantID: scope.TenantID, FarmID: scope.FarmID, RunID: runID, LoadSeq: loadSeqFirst,
			RankNo: pk.Rank, QuestionType: category, KBID: pk.Entry.KBID, KBVersion: pk.Entry.Version,
			PickReason: string(pk.Reason), Strategy: version, CreatedAt: now})
	}
	return out
}
