package service

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/model"
)

// 灰度前检查的文件类检查项：D5、法规签字、告警规则（都读仓库内文件，失败关闭）。

// externalDecisionsFile 是 policy/external_decisions.yaml。
type externalDecisionsFile struct {
	Version   int                `yaml:"version"`
	Decisions []externalDecision `yaml:"decisions"`
}

// externalDecision 是一项外部决策；CLOSED 时 closed_at / approved_by / record_ref 都必须非空。
type externalDecision struct {
	ID         string `yaml:"id"`
	Title      string `yaml:"title"`
	Status     string `yaml:"status"`
	ClosedAt   string `yaml:"closed_at"`
	ApprovedBy string `yaml:"approved_by"`
	RecordRef  string `yaml:"record_ref"`
}

// 签字 / 闭环日期按北京时间的日历日填写（2006-01-02），晚于当天的日期拒绝。
const grayDateLayout = "2006-01-02"

var grayDateZone = time.FixedZone("CST", 8*3600)

// checkGrayDate 校验日期字段：须为 2006-01-02 且不晚于 now 所在的北京时间日期。
func checkGrayDate(field, v string, now time.Time) error {
	d, err := time.ParseInLocation(grayDateLayout, strings.TrimSpace(v), grayDateZone)
	if err != nil {
		return fmt.Errorf("%s %q is not a %s date", field, v, grayDateLayout)
	}
	if d.After(now) {
		return fmt.Errorf("%s %s is in the future", field, v)
	}
	return nil
}

// decodeStrict 按已知字段严格解码 YAML（拼错字段名即失败，避免签字字段被静默忽略）。
func decodeStrict(b []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// checkD5 要求决策 id 不重复，D5 状态为 CLOSED、闭环记录字段齐全且 closed_at 是不晚于今天的日期。
func (g *GrayPreflight) checkD5() CheckResult {
	b, err := fs.ReadFile(g.d.Files, ExternalDecisionsPath)
	if err != nil {
		return checkFail(CheckD5Closed, "read %s: %v", ExternalDecisionsPath, err)
	}
	var f externalDecisionsFile
	if err := decodeStrict(b, &f); err != nil {
		return checkFail(CheckD5Closed, "parse %s: %v", ExternalDecisionsPath, err)
	}
	seen := map[string]bool{}
	for _, d := range f.Decisions {
		if seen[d.ID] {
			return checkFail(CheckD5Closed, "%s: duplicate decision id %q", ExternalDecisionsPath, d.ID)
		}
		seen[d.ID] = true
	}
	for _, d := range f.Decisions {
		if d.ID != grayDecisionD5 {
			continue
		}
		if d.Status != decisionClosed {
			return checkFail(CheckD5Closed, "D5 %s status %s (待签字)", d.Title, d.Status)
		}
		if missing := blankFields(map[string]string{"closed_at": d.ClosedAt, "approved_by": d.ApprovedBy,
			"record_ref": d.RecordRef}); len(missing) > 0 {
			return checkFail(CheckD5Closed, "D5 CLOSED but %s empty", strings.Join(missing, ", "))
		}
		if err := checkGrayDate("closed_at", d.ClosedAt, g.d.Clock.Now()); err != nil {
			return checkFail(CheckD5Closed, "D5: %v", err)
		}
		return checkPass(CheckD5Closed, "D5 closed at %s by %s (%s)", d.ClosedAt, d.ApprovedBy, d.RecordRef)
	}
	return checkFail(CheckD5Closed, "D5 not found in %s", ExternalDecisionsPath)
}

// blankFields 返回值为空白的字段名（按名字排序，输出稳定）。
func blankFields(fields map[string]string) []string {
	var out []string
	for k, v := range fields {
		if strings.TrimSpace(v) == "" {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// checkSignoff 要求 regulation_map.yaml 头部 signoff 三项都非空，legal_signed_at 是不晚于今天的日期。
func (g *GrayPreflight) checkSignoff() CheckResult {
	b, err := fs.ReadFile(g.d.Files, RegulationMapPath)
	if err != nil {
		return checkFail(CheckLegalSignoff, "read %s: %v", RegulationMapPath, err)
	}
	s, err := constraint.ParseRegulationSignoff(b)
	if err != nil {
		return checkFail(CheckLegalSignoff, "%v", err)
	}
	if missing := s.Missing(); len(missing) > 0 {
		return checkFail(CheckLegalSignoff, "signoff %s empty (待签字)", strings.Join(missing, ", "))
	}
	if err := checkGrayDate("legal_signed_at", s.LegalSignedAt, g.d.Clock.Now()); err != nil {
		return checkFail(CheckLegalSignoff, "signoff: %v", err)
	}
	return checkPass(CheckLegalSignoff, "signed by %s at %s (%s)", s.LegalSigner, s.LegalSignedAt, s.RecordRef)
}

// alertRulesFile 是 Prometheus 规则文件中本检查用到的字段（未知字段忽略）；record 规则不是告警，跳过。
type alertRulesFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert  string `yaml:"alert"`
			Record string `yaml:"record"`
			Expr   string `yaml:"expr"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// alertRulesFileOnly 标明本检查只核对仓库文件。
const alertRulesFileOnly = "仅文件，Prometheus 加载待演练"

// errNoAlertRules 表示规则文件里没有任何规则。
var errNoAlertRules = errors.New("no alert rules")

// checkAlertRules 要求规则文件解析通过、每条规则有 expr、全部告警项（model.AlertNames）都有规则。
// 只核对仓库内文件；Prometheus 实际加载由值班在演练记录里截图确认（待演练）。
func (g *GrayPreflight) checkAlertRules() CheckResult {
	b, err := fs.ReadFile(g.d.Files, AlertRulesPath)
	if err != nil {
		return checkFail(CheckAlertRules, "read %s: %v", AlertRulesPath, err)
	}
	n, err := parseAlertRules(b)
	if err != nil {
		return checkFail(CheckAlertRules, "%s: %v", AlertRulesPath, err)
	}
	return checkPass(CheckAlertRules, "%s parsed, %d alert rules cover %d alert names (%s)", AlertRulesPath, n,
		len(model.AlertNames), alertRulesFileOnly)
}

// parseAlertRules 解析规则文件并返回告警规则数（record 规则不计）。
func parseAlertRules(b []byte) (int, error) {
	var f alertRulesFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	have := map[string]bool{}
	n := 0
	for _, grp := range f.Groups {
		for _, r := range grp.Rules {
			if strings.TrimSpace(r.Record) != "" && strings.TrimSpace(r.Alert) == "" {
				continue
			}
			if strings.TrimSpace(r.Alert) == "" || strings.TrimSpace(r.Expr) == "" {
				return 0, fmt.Errorf("group %s: rule %q has empty alert or expr", grp.Name, r.Alert)
			}
			have[r.Alert] = true
			n++
		}
	}
	if n == 0 {
		return 0, errNoAlertRules
	}
	var missing []string
	for _, a := range model.AlertNames {
		if !have[string(a)] {
			missing = append(missing, string(a))
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("no rule for alerts %s", strings.Join(missing, ", "))
	}
	return n, nil
}
