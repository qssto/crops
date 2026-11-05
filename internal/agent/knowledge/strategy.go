package knowledge

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"agri-steward/internal/model"
)

// PickReason 是条目进入加载结果的原因（kb_load_item.pick_reason）。
type PickReason string

// 加载原因（P2 §6.6）：加载精度的分母排除 MUST_INCLUDE 与 COMMON。
const (
	PickMustInclude PickReason = model.PickReasonMustInclude // 问题类型必加条目
	PickRanked      PickReason = model.PickReasonRanked      // 排序截断选入的专属条目
	PickCommon      PickReason = model.PickReasonCommon      // 排序截断选入的 common/ 全国通用条目
)

// Pick 是一条加载结果：条目、选入原因与名次（从 0 开始，即 kb_load_item.rank_no）。
type Pick struct {
	Entry  *Entry
	Reason PickReason
	Rank   int
}

// SortKey 是排序键名（configs/loader/{版本}.yaml 的 order）。
type SortKey string

// 排序键：均为降序；全部相同时按 kb_id 升序（隐含末键，保证确定性）。
const (
	SortAuthority   SortKey = "authority"   // 权威等级
	SortSpecificity SortKey = "specificity" // 作物 / 区域 / 生育期 / target 专属程度（Phase 0 口径）
	SortStageMatch  SortKey = "stage_match" // 生育期匹配度：限定生育期越窄越优先，全生育期最低
	SortVersion     SortKey = "version"     // 版本新旧：vN 数字越大越优先
	SortTopicMatch  SortKey = "topic_match" // 问题子类匹配：条目 topics 含 Situation.RankTopic 的优先
)

// PhaseZeroLimit 是 Phase 0 graph 现行加载上限（v0 须逐条复现）。
const PhaseZeroLimit = 8

// MaxLimit 是加载上限的硬上界：v1 植保必加 common/safety.md 与 common/operation_window.md 共 8 条安全条目，
// 另须给作物 / 防治对象专属条目留名额（路线建议 ≤ 5 装不下必加安全条目，见 configs/loader/v1.yaml）。
const MaxLimit = 12

// ErrMustIncludeOverLimit 是必加条目占满上限（不给作物 / 防治对象专属条目留名额）。启动时 Check 按快照拦截，
// Select 运行时兜底报错，不静默截断专属条目。
var ErrMustIncludeOverLimit = errors.New("knowledge: must_include entries leave no ranked slot")

// placeholderCrop 在 MustRule.File 中替换为情境作物 code。
const placeholderCrop = "{crop}"

// MustRule 是一条必加规则：按 kb_id 或按文件（可含 {crop}）指定；FormulasOnly 只取带公式段的条目。
// 必加条目同样只从情境过滤后的命中里取（不绕过作物 / 区域 / 生育期过滤与 KB_MISS 判定）。
type MustRule struct {
	KBID         string
	File         string
	FormulasOnly bool
}

// StrategySpec 是一个加载策略版本的配置。
type StrategySpec struct {
	Version     string
	Limit       int
	Order       []SortKey
	MustInclude map[string][]MustRule // 一级 taxonomy code → 必加规则（按规则顺序放入）
}

// PhaseZeroSpec 是 Phase 0 现行加载口径（graph 上限 8、权威等级 → 专属程度、无必加），
// 即 configs/loader/v0.yaml 应有的内容；未装配策略配置时 graph 用它，与 Select 逐条一致。
func PhaseZeroSpec() StrategySpec {
	return StrategySpec{Version: model.LoaderStrategyV0, Limit: PhaseZeroLimit, Order: []SortKey{SortAuthority, SortSpecificity}}
}

// Strategy 是校验过的加载策略：过滤 → 必加 → 按 Order 排序 → 截断到 Limit。
type Strategy struct{ spec StrategySpec }

var strategyVersionPattern = regexp.MustCompile(`^v[0-9]+$`)

var knownSortKeys = map[SortKey]bool{SortAuthority: true, SortSpecificity: true, SortStageMatch: true, SortVersion: true,
	SortTopicMatch: true}

// NewStrategy 校验配置：版本号 vN、上限 1..MaxLimit、排序键已知且不重复、必加规则恰好指定 kb_id 或 file
// 之一，且每类必加规则数小于上限（至少留一个排序名额）。tx 非空时校验 MustInclude 的键是一级 taxonomy code。
func NewStrategy(spec StrategySpec, tx *Taxonomy) (*Strategy, error) {
	if !strategyVersionPattern.MatchString(spec.Version) {
		return nil, fmt.Errorf("knowledge: loader version %q must match vN", spec.Version)
	}
	if spec.Limit < 1 || spec.Limit > MaxLimit {
		return nil, fmt.Errorf("knowledge: loader %s limit %d not in [1, %d]", spec.Version, spec.Limit, MaxLimit)
	}
	if len(spec.Order) == 0 {
		return nil, fmt.Errorf("knowledge: loader %s order is empty", spec.Version)
	}
	for i, k := range spec.Order {
		if !knownSortKeys[k] {
			return nil, fmt.Errorf("knowledge: loader %s unknown sort key %q", spec.Version, k)
		}
		if slices.Contains(spec.Order[:i], k) {
			return nil, fmt.Errorf("knowledge: loader %s duplicate sort key %q", spec.Version, k)
		}
	}
	for cat, rules := range spec.MustInclude {
		if tx != nil {
			if top, ok := tx.CategoryOf(cat); !ok || top != cat {
				return nil, fmt.Errorf("knowledge: loader %s must_include key %q is not a top-level category", spec.Version, cat)
			}
		}
		if len(rules) == 0 {
			return nil, fmt.Errorf("knowledge: loader %s must_include[%s] has no rules", spec.Version, cat)
		}
		if len(rules) >= spec.Limit {
			return nil, fmt.Errorf("knowledge: loader %s must_include[%s] has %d rules, limit %d leaves no ranked slot",
				spec.Version, cat, len(rules), spec.Limit)
		}
		for _, r := range rules {
			if err := r.validate(); err != nil {
				return nil, fmt.Errorf("knowledge: loader %s must_include[%s]: %w", spec.Version, cat, err)
			}
		}
	}
	return &Strategy{spec: cloneSpec(spec)}, nil
}

func (r MustRule) validate() error {
	if (r.KBID == "") == (r.File == "") {
		return errors.New("rule must set exactly one of kb_id / file")
	}
	if r.KBID != "" {
		if _, err := FileOf(r.KBID); err != nil {
			return err
		}
		return nil
	}
	if !strings.HasSuffix(r.File, ".md") || strings.HasPrefix(r.File, "/") || strings.Contains(r.File, "..") {
		return fmt.Errorf("file %q must be a relative .md path under knowledge/", r.File)
	}
	return nil
}

func cloneSpec(s StrategySpec) StrategySpec {
	out := StrategySpec{Version: s.Version, Limit: s.Limit, Order: slices.Clone(s.Order),
		MustInclude: make(map[string][]MustRule, len(s.MustInclude))}
	for k, v := range s.MustInclude {
		out.MustInclude[k] = slices.Clone(v)
	}
	return out
}

// Version 返回策略版本号（agent_run.loader_strategy）。
func (s *Strategy) Version() string { return s.spec.Version }

// Limit 返回加载上限。
func (s *Strategy) Limit() int { return s.spec.Limit }

// Select 按策略选条目：与 Select 共用过滤与 KB_MISS 判定；先放情境类别的必加条目（MUST_INCLUDE），
// 再把其余命中按 Order 排序填满上限（common/ 条目记 COMMON，其余记 RANKED）。
// 必加条目只从过滤后的命中里取，命中里没有的规则跳过（例如区域 / 生育期不符），不回落到全库。
// 必加条目超过 Limit-1 时返回 ErrMustIncludeOverLimit（启动时 Check 已按快照拦截，这里是兜底）；
// 有必加条目且其中没有作物 / 防治对象专属条目（非 common/）时，最后一个名额留给排序最前的专属条目。
func (s *Strategy) Select(snap *Snapshot, tx *Taxonomy, sit Situation) ([]Pick, error) {
	sit, hits, err := filterHits(snap, tx, sit)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool { return s.less(hits[i], hits[j], sit) })
	picks := s.mustPicks(hits, sit, s.spec.Limit)
	if len(picks) > s.spec.Limit-1 {
		return nil, fmt.Errorf("%w: loader %s %s has %d must_include entries, limit %d",
			ErrMustIncludeOverLimit, s.spec.Version, sit, len(picks), s.spec.Limit)
	}
	taken := make(map[string]bool, len(hits))
	for _, p := range picks {
		taken[p.Entry.KBID] = true
	}
	reserve := len(picks) > 0 && !slices.ContainsFunc(picks, func(p Pick) bool { return rankedReason(p.Entry) == PickRanked })
	fill := func(lastSlotSpecific bool) {
		for _, e := range hits {
			if len(picks) >= s.spec.Limit {
				return
			}
			if taken[e.KBID] || (lastSlotSpecific && len(picks) == s.spec.Limit-1 && rankedReason(e) != PickRanked) {
				continue
			}
			taken[e.KBID] = true
			picks = append(picks, Pick{Entry: e, Reason: rankedReason(e)})
			if rankedReason(e) == PickRanked {
				lastSlotSpecific = false
			}
		}
	}
	fill(reserve)
	fill(false) // 命中里没有专属条目时，留出的名额按排序补满
	for i := range picks {
		picks[i].Rank = i
	}
	return picks, nil
}

// mustPicks 按规则顺序取 hits 中情境类别的必加条目（同一条目只取一次）。
func (s *Strategy) mustPicks(hits []*Entry, sit Situation, capHint int) []Pick {
	picks := make([]Pick, 0, capHint)
	taken := make(map[string]bool)
	for _, r := range s.spec.MustInclude[sit.Category] {
		for _, e := range hits {
			if !taken[e.KBID] && r.selects(e, sit) {
				taken[e.KBID] = true
				picks = append(picks, Pick{Entry: e, Reason: PickMustInclude})
			}
		}
	}
	return picks
}

func rankedReason(e *Entry) PickReason {
	if strings.HasPrefix(e.KBID, commonPrefix) {
		return PickCommon
	}
	return PickRanked
}

// selects 判断规则是否点名条目 e。
func (r MustRule) selects(e *Entry, sit Situation) bool {
	if r.KBID != "" {
		return e.KBID == r.KBID
	}
	return e.File == strings.ReplaceAll(r.File, placeholderCrop, sit.Crop) && (!r.FormulasOnly || len(e.Formulas) > 0)
}

// less 按 Order 逐键降序比较，全部相同时按 kb_id 升序。
func (s *Strategy) less(a, b *Entry, sit Situation) bool {
	for _, k := range s.spec.Order {
		if x, y := sortValue(k, a, sit), sortValue(k, b, sit); x != y {
			return x > y
		}
	}
	return a.KBID < b.KBID
}

func sortValue(k SortKey, e *Entry, sit Situation) int {
	switch k {
	case SortAuthority:
		return e.AuthorityLevel.Rank()
	case SortSpecificity:
		return e.specificity(sit)
	case SortStageMatch:
		return stageMatchScore(e.Stage)
	case SortVersion:
		return versionNumber(e.Version)
	case SortTopicMatch:
		if sit.RankTopic != "" && slices.Contains(e.Topics, sit.RankTopic) {
			return 1
		}
		return 0
	}
	return 0 // NewStrategy 已拒绝未知键
}

// stageMatchScore：全生育期为 0；限定生育期的条目（已通过过滤，故与情境生育期匹配）列得越窄分越高。
func stageMatchScore(stages []string) int {
	if slices.Contains(stages, Wildcard) || len(stages) == 0 {
		return 0
	}
	return len(Stages) + 1 - len(stages)
}

// versionNumber 解析 vN；条目版本在加载时已按 versionPattern 校验，解析失败记 0。
func versionNumber(v string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(v, "v"))
	if err != nil {
		return 0
	}
	return n
}

// Entries 取出 picks 中的条目（保持名次顺序）。
func Entries(picks []Pick) []*Entry {
	out := make([]*Entry, len(picks))
	for i, p := range picks {
		out[i] = p.Entry
	}
	return out
}
