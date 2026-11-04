package sample

import (
	"fmt"
	"slices"
	"strings"

	"agri-steward/internal/agent/knowledge"
)

// RuleStrata 是分层维度规则名（P2 L1-T）。
const RuleStrata = "strata"

// 难度取值（分层维度之一）。
const (
	DifficultyEasy   = "easy"
	DifficultyMedium = "medium"
	DifficultyHard   = "hard"
)

// Difficulties 是难度枚举（覆盖报表列序）。
var Difficulties = []string{DifficultyEasy, DifficultyMedium, DifficultyHard}

// phenologyTool 是物候工具全名；其 fixture 返回的 stage 是样本所处生育期的权威来源。
const phenologyTool = "crop.phenology"

// TagUserSim 标记 P2 L7 多轮模拟用户样本：覆盖报表单列，不计入分层格子与总数目标。
const TagUserSim = "user_sim"

// Strata 是一条样本的分层坐标：作物 × 生育期 × 问题类型 × 难度。
type Strata struct {
	Crop       string
	Stage      string
	Topic      string // 问题类型：第一个 taxonomy 子类 tag（如 protect.dosage）
	Category   string // Topic 所属一级分类
	Difficulty string
}

// StrataOf 按确定、可校验的方式读出分层坐标（不校验，取不到的维度为空串）：
//   - 作物：crop 字段；
//   - 生育期：stage 字段（taxonomy.stages 之一，含「不限」）；
//   - 问题类型：tags 中第一个 taxonomy 子类 code（一级分类 code 不算）；
//   - 难度：difficulty 字段。
func StrataOf(s *Sample, tx *knowledge.Taxonomy) Strata {
	st := Strata{Crop: s.Crop, Stage: s.Stage, Difficulty: s.Difficulty}
	for _, tag := range s.Tags {
		if cat, ok := tx.CategoryOf(tag); ok && cat != tag {
			st.Topic, st.Category = tag, cat
			break
		}
	}
	return st
}

// PhenologyStage 返回 crop.phenology fixture 的第一个 stage（return 或 returns 首条），未声明时为空串。
func PhenologyStage(s *Sample) string {
	for _, f := range s.Fixtures.MCP[phenologyTool] {
		ret := f.Return
		if ret == nil && len(f.Returns) > 0 {
			ret = f.Returns[0]
		}
		if m, ok := ret.(map[string]any); ok {
			if st, ok := m["stage"].(string); ok {
				return st
			}
		}
	}
	return ""
}

// strataCheck：四个分层维度齐全且取值合法；advice.category 须与问题类型同属一个一级分类；
// 声明了物候 fixture 的 stage 时 stage 字段须与之一致（防止元数据与情境脱节）。
func strataCheck(tx *knowledge.Taxonomy) SampleCheck {
	return func(s *Sample) []string {
		var msgs []string
		st := StrataOf(s, tx)
		switch {
		case st.Crop == "":
			msgs = append(msgs, "crop is required (strata)")
		case st.Crop != tx.Crop:
			msgs = append(msgs, fmt.Sprintf("crop %q must be taxonomy crop %q", st.Crop, tx.Crop))
		}
		switch {
		case st.Stage == "":
			msgs = append(msgs, fmt.Sprintf("stage is required (strata; one of taxonomy stages, %q when stage-independent)", knowledge.StageAny))
		case !tx.HasStage(st.Stage):
			msgs = append(msgs, fmt.Sprintf("stage %q is not a taxonomy stage", st.Stage))
		case PhenologyStage(s) != "" && PhenologyStage(s) != st.Stage:
			msgs = append(msgs, fmt.Sprintf("stage %q differs from crop.phenology fixture stage %q", st.Stage, PhenologyStage(s)))
		}
		if st.Topic == "" {
			msgs = append(msgs, "tags must include a taxonomy subcategory code as question type (strata)")
		} else if a := s.Expect.Advice; a != nil && a.Category != "" && a.Category != st.Category {
			msgs = append(msgs, fmt.Sprintf("question type %q (first subcategory tag) does not belong to expect.advice.category %q", st.Topic, a.Category))
		}
		switch {
		case strings.TrimSpace(st.Difficulty) == "":
			msgs = append(msgs, "difficulty is required (strata)")
		case !slices.Contains(Difficulties, st.Difficulty):
			msgs = append(msgs, fmt.Sprintf("difficulty %q not in %v", st.Difficulty, Difficulties))
		}
		return msgs
	}
}
