package knowledge

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Check 对照知识快照做启动校验（P2 L5 评审 P1-1 / P1-2），失败即启动失败：
//   - 每条必加规则在快照中至少匹配一个本类别的在用条目：拼错的 kb_id / 文件名不会在运行时被当成「情境不符」静默跳过；
//   - 任一可达情境下的必加条目数不超过 Limit-1：至少给作物 / 防治对象专属条目留一个名额。
//
// 可达情境取快照中出现的作物 × 区域 × 生育期（含未知），target 与 topic 取空（不收窄过滤，所得必加数是上界）。
func (s *Strategy) Check(snap *Snapshot) error {
	if snap == nil {
		return errors.New("knowledge: loader check: snapshot is nil")
	}
	cats := make([]string, 0, len(s.spec.MustInclude))
	for cat := range s.spec.MustInclude {
		cats = append(cats, cat)
	}
	sort.Strings(cats)
	for _, cat := range cats {
		if un := s.UnmatchedRules(snap, cat); len(un) > 0 {
			return fmt.Errorf("knowledge: loader %s must_include[%s] rules %s match no entry in knowledge %s",
				s.spec.Version, cat, strings.Join(un, ", "), snap.Ref())
		}
		if sit, n := s.maxMust(snap, cat); n > s.spec.Limit-1 {
			return fmt.Errorf("%w: loader %s %s has %d must_include entries, limit %d",
				ErrMustIncludeOverLimit, s.spec.Version, sit, n, s.spec.Limit)
		}
	}
	return nil
}

// UnmatchedRules 返回 category 下在整个快照中一个在用条目都匹配不到的必加规则（kb_id 或 file 原文）。
// 按文件的规则把 {crop} 换成条目自身作物比较；FormulasOnly 只认带公式段的条目。
func (s *Strategy) UnmatchedRules(snap *Snapshot, category string) []string {
	var out []string
	for _, r := range s.spec.MustInclude[category] {
		if !slices.ContainsFunc(snap.entries, func(e *Entry) bool {
			return !snap.deprecated[e.KBID] && slices.Contains(e.Category, category) && r.selects(e, Situation{Crop: e.Crop})
		}) {
			out = append(out, r.String())
		}
	}
	return out
}

// String 返回规则在配置里的写法（kb_id 或 file）。
func (r MustRule) String() string {
	if r.KBID != "" {
		return r.KBID
	}
	return r.File
}

// maxMust 枚举可达情境，返回必加条目数最多的情境与条目数。
func (s *Strategy) maxMust(snap *Snapshot, category string) (Situation, int) {
	crops, regions := []string{}, []string{}
	for _, e := range snap.entries {
		if e.Crop != Wildcard && !slices.Contains(crops, e.Crop) {
			crops = append(crops, e.Crop)
		}
		if e.Region != Wildcard && !slices.Contains(regions, e.Region) {
			regions = append(regions, e.Region)
		}
	}
	if len(crops) == 0 {
		crops = []string{Wildcard}
	}
	if len(regions) == 0 {
		regions = []string{Wildcard}
	}
	var worst Situation
	most := -1
	for _, crop := range crops {
		for _, region := range regions {
			for _, stage := range append([]string{""}, Stages...) {
				sit := Situation{Crop: crop, Region: region, Stage: stage, Category: category}
				var hits []*Entry
				for _, e := range snap.entries {
					if !snap.deprecated[e.KBID] && sit.matches(e) {
						hits = append(hits, e)
					}
				}
				if n := len(s.mustPicks(hits, sit, 0)); n > most {
					worst, most = sit, n
				}
			}
		}
	}
	return worst, most
}
