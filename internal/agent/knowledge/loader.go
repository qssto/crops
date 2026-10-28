package knowledge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
)

// DefaultLimit 是单次注入条目上限（P0 §6.4-3：建议 ≤ 5）。
const DefaultLimit = 5

// RefuseReasonKBMiss 是知识未命中时 AdviceCard.refuse_reason 的取值。
const RefuseReasonKBMiss = "KB_MISS"

// ErrKBMiss 表示按情境过滤后 0 条命中：调用方必须拒答（refuse_reason=KB_MISS），不得回落到模型记忆。
var ErrKBMiss = errors.New("knowledge: KB_MISS")

// Situation 是加载情境：作物 / 生育期 / 区域来自工具与农场档案，Category / Topic 来自 question_taxonomy 分类。
type Situation struct {
	Crop     string // 作物 code，如 rice；必填
	Stage    string // 生育期枚举值；空表示未知，只匹配全生育期（"*"）条目
	Region   string // 区域 code，如 jiangsu；必填
	Category string // 一级 taxonomy code；Topic 非空时可省略（由 Topic 推出）
	Topic    string // 子类 taxonomy code，可空
	Target   string // 防治对象 id，可空；非空时植保条目只保留同 target 的
}

// Loader 是渐进式加载器（progressive_loader）：过滤 → authority_level 排序 → 截断。
type Loader struct {
	store *Store
	limit int
}

// LoaderOption 配置 Loader。
type LoaderOption func(*Loader)

// WithLimit 设置单次注入上限（< 1 时报错于 NewLoader）。
func WithLimit(n int) LoaderOption { return func(l *Loader) { l.limit = n } }

// NewLoader 构造 Loader，默认上限 DefaultLimit。
func NewLoader(store *Store, opts ...LoaderOption) (*Loader, error) {
	if store == nil {
		return nil, errors.New("knowledge: store is nil")
	}
	l := &Loader{store: store, limit: DefaultLimit}
	for _, o := range opts {
		o(l)
	}
	if l.limit < 1 {
		return nil, fmt.Errorf("knowledge: limit must be >= 1, got %d", l.limit)
	}
	return l, nil
}

// Load 打开 ref 对应的知识版本并按情境选条目。命中为 0，或只命中通用条目（无作物专属、
// 情境带 target 时无同 target 条目）均返回 ErrKBMiss。
func (l *Loader) Load(ctx context.Context, ref string, tx *Taxonomy, sit Situation) ([]*Entry, error) {
	snap, err := l.store.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	return Select(snap, tx, sit, l.limit)
}

// Select 是 Load 的纯函数部分：在给定快照上过滤、排序、截断。
func Select(snap *Snapshot, tx *Taxonomy, sit Situation, limit int) ([]*Entry, error) {
	sit, err := resolveSituation(tx, sit)
	if err != nil {
		return nil, err
	}
	var hits []*Entry
	anchored := false
	for _, e := range snap.entries {
		if !snap.deprecated[e.KBID] && sit.matches(e) {
			hits = append(hits, e)
			anchored = anchored || sit.anchors(e)
		}
	}
	// 只命中通用条目（crop="*"，如作业窗口、PHI）不足以回答：无作物 / 防治对象专属知识即 KB_MISS
	if !anchored {
		return nil, fmt.Errorf("%w: %s (%d generic hits, no crop/target-specific entry)", ErrKBMiss, sit, len(hits))
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].less(hits[j], sit) })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// resolveSituation 校验情境并由 Topic 推出 Category；未知 code 视为调用方错误而非 KB_MISS。
func resolveSituation(tx *Taxonomy, sit Situation) (Situation, error) {
	if tx == nil {
		return sit, errors.New("knowledge: taxonomy is nil")
	}
	if sit.Crop == "" || sit.Region == "" {
		return sit, fmt.Errorf("knowledge: situation needs crop and region: %s", sit)
	}
	if sit.Stage != "" && !slices.Contains(Stages, sit.Stage) {
		return sit, fmt.Errorf("knowledge: stage %q not in %v", sit.Stage, Stages)
	}
	if sit.Topic != "" {
		top, ok := tx.CategoryOf(sit.Topic)
		if !ok || top == sit.Topic {
			return sit, fmt.Errorf("knowledge: topic %q is not a taxonomy subcode", sit.Topic)
		}
		if sit.Category != "" && sit.Category != top {
			return sit, fmt.Errorf("knowledge: topic %q does not belong to category %q", sit.Topic, sit.Category)
		}
		sit.Category = top
	}
	if top, ok := tx.CategoryOf(sit.Category); !ok || top != sit.Category {
		return sit, fmt.Errorf("knowledge: category %q is not a taxonomy top-level code", sit.Category)
	}
	return sit, nil
}

func (sit Situation) matches(e *Entry) bool {
	return dimMatch(e.Crop, sit.Crop) &&
		dimMatch(e.Region, sit.Region) &&
		slices.Contains(e.Category, sit.Category) &&
		(sit.Topic == "" || slices.Contains(e.Topics, sit.Topic)) &&
		stageMatch(e.Stage, sit.Stage) &&
		(sit.Target == "" || e.Target == "" || e.Target == sit.Target)
}

func dimMatch(entry, want string) bool { return entry == Wildcard || entry == want }

// anchors 判断条目是否为「锚点」：作物专属，且情境带 target 时 target 一致。
func (sit Situation) anchors(e *Entry) bool {
	return e.Crop == sit.Crop && (sit.Target == "" || e.Target == sit.Target)
}

// stageMatch：未知生育期只匹配全生育期条目，避免把特定时期的指标套到不明时期。
func stageMatch(stages []string, want string) bool {
	if slices.Contains(stages, Wildcard) {
		return true
	}
	return want != "" && slices.Contains(stages, want)
}

// less 排序键：authority_level 降序 → 专属优先于通配（作物、区域、生育期、target）→ kb_id 升序。
func (e *Entry) less(o *Entry, sit Situation) bool {
	if a, b := e.AuthorityLevel.Rank(), o.AuthorityLevel.Rank(); a != b {
		return a > b
	}
	if a, b := e.specificity(sit), o.specificity(sit); a != b {
		return a > b
	}
	return e.KBID < o.KBID
}

func (e *Entry) specificity(sit Situation) int {
	n := 0
	for _, specific := range []bool{
		e.Crop != Wildcard, e.Region != Wildcard, !slices.Contains(e.Stage, Wildcard),
		sit.Target != "" && e.Target == sit.Target,
	} {
		if specific {
			n++
		}
	}
	return n
}

func (sit Situation) String() string {
	return fmt.Sprintf("crop=%s stage=%s region=%s category=%s topic=%s target=%s",
		sit.Crop, sit.Stage, sit.Region, sit.Category, sit.Topic, sit.Target)
}
