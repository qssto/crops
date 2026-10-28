package graph

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"agri-steward/internal/agent"
)

// 工具名（MCP 网关 / FixtureMCP 一致）。主链路只读，不调用 device.*。
const (
	toolPhenology     = "crop.phenology"
	toolProfile       = "field.profile"
	toolHistory       = "field.history"
	toolPestSurvey    = "field.pest_survey"
	toolForecast      = "weather.forecast"
	toolAlert         = "weather.alert"
	toolWeatherRecent = "weather.history"
)

// 工具结果状态（tool_call.status）。REJECTED 视为该数据不存在，其余非 OK 视为数据缺失。
const (
	statusOK       = "OK"
	statusRejected = "REJECTED"
)

// 派生事实名（与知识库阈值 metric 同名）。
const (
	factStage           = "stage"
	factAreaMu          = "area_mu"
	factSoilN           = "soil_n_level"
	factWaterDepth      = "water_depth_cm"
	factSurveyAge       = "survey_age_days"
	factWeatherAge      = "weather_age_hours"
	factDaysTransplant  = "days_after_transplant"
	factDaysBeforeHead  = "days_before_heading"
	factSubmergedHours  = "submerged_hours"
	factLastDrain       = "last_drain"
	factLastFertilize   = "last_fertilize"
	factLastSpray       = "last_spray"
	factSprays          = "sprays"
	factExpectedHarvest = "expected_harvest"
	keyCollectedAt      = "collected_at"
	keyTransplantDate   = "transplant_date"
	keyExpectedHeading  = "expected_heading"
	hoursPerDay         = 24
	recentFertilizeDays = 7 // 7 天内追过肥则不建议重复追施
)

// aliases 把工具返回字段归一为知识库阈值 metric（工具与知识库命名不一致处）。
var aliases = map[string]string{
	"rain_mm":           "rain_mm_24h",
	"egg_masses_per_mu": "egg_mass_per_mu",
	"disease_centers":   "blast_centers",
}

// toolOut 是一次工具调用的结果与解码后的数据。
type toolOut struct {
	Tool   string
	Result agent.ToolResult
	Data   map[string]any
}

// fact 是一个事实值及其来源工具（用于 measured 引用）。
type fact struct {
	Tool  string
	Value any
}

// facts 是 enrich 汇总的事实：叶子字段名 → 值（先到先得，按工具名排序保证确定性）。
type facts map[string]fact

// buildFacts 从 OK 的工具结果展开叶子字段，并按 now 派生时效类事实。
func buildFacts(outs map[string]*toolOut, now time.Time, loc *time.Location) facts {
	f := facts{}
	names := make([]string, 0, len(outs))
	for n := range outs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		o := outs[n]
		if o.Data == nil {
			continue
		}
		f.flatten(n, o.Data)
	}
	if o := outs[toolPestSurvey]; o != nil && o.Data != nil {
		if at, ok := parseTime(o.Data[keyCollectedAt], loc); ok {
			f.set(toolPestSurvey, factSurveyAge, now.Sub(at).Hours()/hoursPerDay)
		}
	}
	if o := outs[toolForecast]; o != nil && o.Data != nil {
		if at, ok := parseTime(o.Data[keyCollectedAt], loc); ok {
			f.set(toolForecast, factWeatherAge, now.Sub(at).Hours())
		}
	}
	if s, ok := f.str(keyTransplantDate); ok {
		if d, ok := civilDays(s, now, loc); ok {
			f.set(f[keyTransplantDate].Tool, factDaysTransplant, float64(d))
		}
	}
	if s, ok := f.str(keyExpectedHeading); ok {
		if d, ok := civilDays(s, now, loc); ok {
			f.set(f[keyExpectedHeading].Tool, factDaysBeforeHead, float64(-d))
		}
	}
	return f
}

func (f facts) set(tool, key string, v any) {
	if _, ok := f[key]; !ok {
		f[key] = fact{Tool: tool, Value: v}
	}
}

// flatten 递归展开对象；对象本身也按键登记（如 last_drain），数组按原样登记。
func (f facts) flatten(tool string, m map[string]any) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		f.set(tool, k, v)
		if a, ok := aliases[k]; ok {
			f.set(tool, a, v)
		}
		if sub, ok := v.(map[string]any); ok {
			f.flatten(tool, sub)
		}
	}
}

func (f facts) num(key string) (float64, bool) {
	v, ok := f[key].Value.(float64)
	return v, ok
}

func (f facts) str(key string) (string, bool) {
	v, ok := f[key].Value.(string)
	return v, ok && v != ""
}

func (f facts) has(key string) bool {
	_, ok := f[key]
	return ok
}

// series 返回 metric 的逐日序列及其事实名（工具字段 <metric>_<N>d，如 mean_temp_c_3d）。
func (f facts) series(metric string) ([]float64, string, bool) {
	keys := make([]string, 0)
	for k := range f {
		if strings.HasPrefix(k, metric+"_") && strings.HasSuffix(k, "d") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		arr, ok := f[k].Value.([]any)
		if !ok {
			continue
		}
		out := make([]float64, 0, len(arr))
		for _, x := range arr {
			n, ok := x.(float64)
			if !ok {
				return nil, "", false
			}
			out = append(out, n)
		}
		return out, k, true
	}
	return nil, "", false
}

// parseTime 接受 RFC3339 或 YYYY-MM-DD（按 loc 零点）。
func parseTime(v any, loc *time.Location) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, loc); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// civilDays 返回 date 到 now（loc 日历日）的天数，date 在未来为负。
func civilDays(date string, now time.Time, loc *time.Location) (int, bool) {
	d, err := time.ParseInLocation(time.DateOnly, date, loc)
	if err != nil {
		return 0, false
	}
	n := now.In(loc)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	return int(today.Sub(d).Hours() / hoursPerDay), true
}

// decodeData 把工具返回解码为对象；非对象或非法 JSON 返回错误（按数据缺失处理）。
func decodeData(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty tool data")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode tool data: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("tool data is not an object")
	}
	return m, nil
}
