package app

// P2 L5 知识加载策略装配（§6.6）：configs/loader/{v0,v1}.yaml → knowledge.Strategy，注入 graph.Config.Loading；
// kb_load_item 明细写入与保留期清理任务（registerP2Loops("P2-L5", …)）。

import (
	"fmt"

	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/config"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// DefaultLoaderDir 是加载策略配置目录（相对仓库根）。
const DefaultLoaderDir = "configs/loader"

// kbLoadItemTable 是 retention.table_days 中 kb_load_item 的键。
const kbLoadItemTable = "kb_load_item"

// KnowledgeLoaders 是 v0 / v1 两个加载策略（GraphConfig.Loaders）。
type KnowledgeLoaders struct {
	V0, V1 *knowledge.Strategy
}

// v1RedLineCategories 是 v1 必须配置必加规则的问题类型（§6.6 红线：植保必加安全条目、施肥必加公式段）。
var v1RedLineCategories = []model.Category{model.CategoryProtect, model.CategoryFertilize}

// LoadKnowledgeLoaders 读 dir 下 v0.yaml / v1.yaml，按 taxonomy 深度校验，并对照运行时知识快照 snap
// 校验必加规则（每条至少匹配一个条目、任一情境必加数 ≤ limit-1）；v1 缺植保 / 施肥必加规则也拒绝。
// 任一缺失或非法返回错误（启动失败）。
func LoadKnowledgeLoaders(dir string, tx *knowledge.Taxonomy, snap *knowledge.Snapshot) (*KnowledgeLoaders, error) {
	if tx == nil || snap == nil {
		return nil, fmt.Errorf("app: load knowledge loaders: taxonomy and knowledge snapshot are required")
	}
	cfgs, err := config.LoadLoaderStrategies(dir, []string{model.LoaderStrategyV0, model.LoaderStrategyV1})
	if err != nil {
		return nil, fmt.Errorf("app: load knowledge loaders: %w", err)
	}
	for _, cat := range v1RedLineCategories {
		if len(cfgs[model.LoaderStrategyV1].MustInclude[string(cat)]) == 0 {
			return nil, fmt.Errorf("app: loader %s: must_include.%s is required (red line)", model.LoaderStrategyV1, cat)
		}
	}
	out := &KnowledgeLoaders{}
	for ver, dst := range map[string]**knowledge.Strategy{model.LoaderStrategyV0: &out.V0, model.LoaderStrategyV1: &out.V1} {
		s, err := knowledge.NewStrategy(strategySpec(cfgs[ver]), tx)
		if err != nil {
			return nil, fmt.Errorf("app: loader %s: %w", ver, err)
		}
		if err := s.Check(snap); err != nil {
			return nil, fmt.Errorf("app: loader %s: %w", ver, err)
		}
		*dst = s
	}
	return out, nil
}

func strategySpec(c *config.LoaderStrategy) knowledge.StrategySpec {
	spec := knowledge.StrategySpec{Version: c.Version, Limit: c.Limit}
	for _, k := range c.Order {
		spec.Order = append(spec.Order, knowledge.SortKey(k))
	}
	if len(c.MustInclude) > 0 {
		spec.MustInclude = make(map[string][]knowledge.MustRule, len(c.MustInclude))
		for cat, rules := range c.MustInclude {
			for _, r := range rules {
				spec.MustInclude[cat] = append(spec.MustInclude[cat],
					knowledge.MustRule{KBID: r.KBID, File: r.File, FormulasOnly: r.FormulasOnly})
			}
		}
	}
	return spec
}

// knowledgeLoading 组装 graph.Config.Loading：未配置 Loaders 时为 nil（内置 v0，不写明细，行为同 P1）。
func (a *App) knowledgeLoading(l *KnowledgeLoaders) *graph.KnowledgeLoading {
	if l == nil {
		return nil
	}
	out := &graph.KnowledgeLoading{V0: l.V0, V1: l.V1}
	if a.DB != nil {
		out.Items = repository.KBLoadItemRepo{DB: a.DB}
	}
	return out
}

func init() { registerP2Loops("P2-L5", kbLoadLoops) }

// kbLoadLoops 构造 kb_load_item 保留期清理任务；DB / Locker 未装配时不起任务。
func kbLoadLoops(a *App) ([]Loop, error) {
	if a == nil || a.DB == nil || a.Locker == nil {
		return nil, nil
	}
	days := service.DefaultKBLoadRetentionDays
	if a.L7 != nil {
		if d, ok := a.L7.TableDays[kbLoadItemTable]; ok {
			if d <= 0 {
				return nil, fmt.Errorf("app: retention.table_days.%s = %d, must be > 0", kbLoadItemTable, d)
			}
			days = d
		}
	}
	job := &service.KBLoadRetention{Store: repository.KBLoadItemRepo{DB: a.DB}, Locks: a.Locker,
		LockKey: a.Keys.LockP2Job(service.KBLoadRetentionJob), Clock: a.Clock, Logger: a.Logger, Days: days}
	return []Loop{{Name: service.KBLoadRetentionJob, Run: job.Run}}, nil
}
