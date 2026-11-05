package reduce

import (
	"slices"
	"sort"

	"agri-steward/internal/evalkit"
)

// LowHitThreshold 是 L8 低命中切片的阈值：子类加载命中率低于它即列入清单（P2 L5 功能任务 6）。
// 取 0.9：stub 下 regression 全量 v0 / v1 命中率均约 93%，低于 90% 的子类才值得补知识条目。
const LowHitThreshold = 0.9

// LoadGaps 是交给 L8 归因的清单（P2 L5 功能任务 6）：高频 KB_MISS 拒答与加载命中率低的子类，驱动补知识条目而不是放宽加载。
// 只出清单，不做 attribution-weekly。
type LoadGaps struct {
	KBMiss []KBMissGap `json:"kb_miss"`
	LowHit []LowHitGap `json:"low_hit"`
}

// KBMissGap 是一个以 KB_MISS 拒答的样本（按次数降序，同次数按 sample_id 升序）。
type KBMissGap struct {
	SampleID     string `json:"sample_id"`
	QuestionType string `json:"question_type"`
	Topic        string `json:"topic"`
	Trials       int    `json:"trials"` // 以 KB_MISS 拒答的 trial 数
}

// LowHitGap 是一个命中率低于 LowHitThreshold 的子类及其未加载的 must_cite（按命中率升序，同值按子类升序）。
type LowHitGap struct {
	Topic    string   `json:"topic"`
	Hit      Ratio    `json:"hit"`
	HitRate  float64  `json:"hit_rate"`
	Missing  []string `json:"missing"`  // 未被加载的 must_cite（去重升序）
	Samples  []string `json:"samples"`  // 有未加载 must_cite 的样本（去重升序）
	Strategy []string `json:"strategy"` // 本切片出现的策略版本
}

// ReduceLoadGaps 由带 LoadObs 的 trial 得出 L8 清单；INFRA_ERROR trial 不计。
func ReduceLoadGaps(trials []evalkit.Trial) LoadGaps {
	miss := map[string]*KBMissGap{}
	type topicAcc struct {
		hit        Ratio
		missing    []string
		samples    []string
		strategies []string
	}
	topics := map[string]*topicAcc{}
	for _, t := range trials {
		o := t.Load
		if o == nil || t.Error != "" {
			continue
		}
		if o.KBMiss {
			g, ok := miss[t.SampleID]
			if !ok {
				g = &KBMissGap{SampleID: t.SampleID, QuestionType: o.QuestionType, Topic: o.Topic}
				miss[t.SampleID] = g
			}
			g.Trials++
		}
		if len(o.MustCite) == 0 {
			continue
		}
		acc, ok := topics[o.Topic]
		if !ok {
			acc = &topicAcc{}
			topics[o.Topic] = acc
		}
		num, den := hitCount(o)
		acc.hit.Num += num
		acc.hit.Den += den
		acc.strategies = appendNew(acc.strategies, o.Strategies...)
		for _, ref := range o.MustCite {
			if !o.Loaded(ref) {
				acc.missing = appendNew(acc.missing, ref)
				acc.samples = appendNew(acc.samples, t.SampleID)
			}
		}
	}
	out := LoadGaps{KBMiss: []KBMissGap{}, LowHit: []LowHitGap{}}
	for _, g := range miss {
		out.KBMiss = append(out.KBMiss, *g)
	}
	sort.Slice(out.KBMiss, func(i, j int) bool {
		a, b := out.KBMiss[i], out.KBMiss[j]
		if a.Trials != b.Trials {
			return a.Trials > b.Trials
		}
		return a.SampleID < b.SampleID
	})
	for topic, acc := range topics {
		if acc.hit.Value() >= LowHitThreshold {
			continue
		}
		slices.Sort(acc.missing)
		slices.Sort(acc.samples)
		slices.Sort(acc.strategies)
		out.LowHit = append(out.LowHit, LowHitGap{Topic: topic, Hit: acc.hit, HitRate: acc.hit.Value(),
			Missing: acc.missing, Samples: acc.samples, Strategy: acc.strategies})
	}
	sort.Slice(out.LowHit, func(i, j int) bool {
		a, b := out.LowHit[i], out.LowHit[j]
		if a.HitRate != b.HitRate {
			return a.HitRate < b.HitRate
		}
		return a.Topic < b.Topic
	})
	return out
}

// appendNew 追加 s 中尚未出现的值（保持首次出现顺序）。
func appendNew(dst []string, s ...string) []string {
	for _, v := range s {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}
