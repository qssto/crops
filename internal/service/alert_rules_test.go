package service

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/model"
)

// P1 L7-B 告警规则文件 deploy/alerts/p1.yaml 的一致性校验：指标都已登记、label 都在族上且取值在枚举内、
// 全部告警项（model.AlertNames）与 policy/fault_matrix.yaml 的告警名都有规则、级别一致、下钻与阈值注解齐全。

const (
	alertRulesPath  = "../../deploy/alerts/p1.yaml"
	faultMatrixPath = "../../policy/fault_matrix.yaml"
	faultTextsPath  = "../../configs/texts/zh-CN.yaml"
)

type alertRuleFile struct {
	Groups []struct {
		Name  string      `yaml:"name"`
		Rules []alertRule `yaml:"rules"`
	} `yaml:"groups"`
}

type alertRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

// p0SinkAlerts 是经 AlertSink 触发的红线告警（P0，电话）；其余 model.AlertNames 为 P1。
var p0SinkAlerts = map[string]bool{
	string(model.AlertAdviceNoCitation): true, string(model.AlertModelActuate): true,
	string(model.AlertPreApprovalDevice): true, string(model.AlertAuditWriteFailed): true,
	string(model.AlertAuditChainBroken): true, string(model.AlertAuditExportPIILeak): true,
}

// severityPriority 是 severity 与 priority 的对应（fault_matrix level 即 severity）。
var severityPriority = map[string]string{"critical": "P0", "warning": "P1"}

var (
	metricRef     = regexp.MustCompile(`\bas_[a-z0-9_]+`)
	selectorRef   = regexp.MustCompile(`\b(as_[a-z0-9_]+)\{([^}]*)\}`)
	matcherRef    = regexp.MustCompile(`([a-z_]+)\s*(=~|!~|!=|=)\s*"([^"]*)"`)
	groupingRef   = regexp.MustCompile(`\b(?:by|without)\s*\(([^)]*)\)`)
	histSuffixes  = []string{"_bucket", "_sum", "_count"}
	dispatchCodes = setOf(string(DispatchOK), string(DispatchRejected), string(DispatchManual), string(DispatchError),
		string(DispatchBlocked))
)

// ruleLabelValues 是规则里按 label 匹配时允许的取值（与 MetricsObserver 的 enum 表一致）；
// 不在表里的 label（farm_id 等）不校验取值。
func ruleLabelValues(label, value string) bool {
	switch label {
	case "alert":
		return knownAlerts[value]
	case "level":
		return alertLevels[value]
	case "fault":
		return faultCodes[value]
	case "mode":
		return dispatchModes[value]
	case "result":
		return dispatchCodes[value]
	case "reason":
		return value == "none" || reasonCodes[value]
	case "to":
		return model.WorkOrderStatus(value).Valid()
	case "status":
		return toolStatuses[value]
	case "server":
		return mcpServers[value]
	case "flag":
		return flagKeys[value]
	case "code":
		return denyCodes[value]
	case "decision":
		return ticketDecisions[value]
	case "kind":
		return value == "prompt" || value == "completion"
	}
	return true
}

// familyOf 把表达式里的指标名（含直方图 _bucket/_sum/_count 后缀）归到注册表里的族。
func familyOf(name string, fams map[string]MetricInfo) (MetricInfo, bool) {
	if f, ok := fams[name]; ok {
		return f, true
	}
	for _, s := range histSuffixes {
		if base, ok := strings.CutSuffix(name, s); ok {
			if f, ok := fams[base]; ok && f.Kind == MetricHistogram {
				return f, true
			}
		}
	}
	return MetricInfo{}, false
}

// checkRuleExpr 校验一条规则的表达式。
func checkRuleExpr(r alertRule, fams map[string]MetricInfo) error {
	var errs []error
	refs := metricRef.FindAllString(r.Expr, -1)
	if len(refs) == 0 {
		errs = append(errs, errors.New("expr references no as_* metric"))
	}
	used := map[string]MetricInfo{}
	for _, name := range refs {
		f, ok := familyOf(name, fams)
		if !ok {
			errs = append(errs, fmt.Errorf("unknown metric %q", name))
			continue
		}
		used[f.Name] = f
	}
	for _, m := range selectorRef.FindAllStringSubmatch(r.Expr, -1) {
		f, ok := familyOf(m[1], fams)
		if !ok {
			continue // 已在上面报过
		}
		for _, mm := range matcherRef.FindAllStringSubmatch(m[2], -1) {
			label, op, value := mm[1], mm[2], mm[3]
			if !slices.Contains(f.Labels, label) {
				errs = append(errs, fmt.Errorf("%s has no label %q", f.Name, label))
				continue
			}
			values := []string{value}
			if op == "=~" || op == "!~" {
				values = strings.Split(value, "|")
			}
			for _, v := range values {
				if !ruleLabelValues(label, v) {
					errs = append(errs, fmt.Errorf("%s{%s}: value %q not in enum", f.Name, label, v))
				}
			}
		}
	}
	for _, m := range groupingRef.FindAllStringSubmatch(r.Expr, -1) {
		for l := range strings.SplitSeq(m[1], ",") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			for _, f := range used {
				if !slices.Contains(f.Labels, l) {
					errs = append(errs, fmt.Errorf("grouping label %q not on %s", l, f.Name))
				}
			}
		}
	}
	for l := range forbiddenLabels {
		if regexp.MustCompile(`\b` + l + `\b`).MatchString(r.Expr) {
			errs = append(errs, fmt.Errorf("expr uses forbidden label %q", l))
		}
	}
	return errors.Join(errs...)
}

// checkRule 校验一条规则的表达式、级别与注解。
func checkRule(r alertRule, fams map[string]MetricInfo) error {
	errs := []error{checkRuleExpr(r, fams)}
	sev := r.Labels["severity"]
	want, ok := severityPriority[sev]
	switch {
	case !ok:
		errs = append(errs, fmt.Errorf("severity %q not critical|warning", sev))
	case r.Labels["priority"] != want:
		errs = append(errs, fmt.Errorf("severity %s needs priority %s, got %q", sev, want, r.Labels["priority"]))
	}
	if r.Labels["receiver"] == "" {
		errs = append(errs, errors.New("labels.receiver is empty"))
	}
	for _, k := range []string{"summary", "drilldown", "threshold"} {
		if strings.TrimSpace(r.Annotations[k]) == "" {
			errs = append(errs, fmt.Errorf("annotations.%s is empty", k))
		}
	}
	if d := r.Annotations["drilldown"]; d != "" && !strings.HasPrefix(d, "/api/v1/farms/") {
		errs = append(errs, fmt.Errorf("drilldown %q is not a farm API link", d))
	}
	if !strings.Contains(r.Annotations["threshold"], "待实测") {
		errs = append(errs, errors.New("threshold not marked 待实测"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("rule %s: %w", r.Alert, err)
	}
	return nil
}

// checkRuleCoverage 校验：告警名不重复，want（告警名 → severity）里的每一项都有规则且 severity 一致。
func checkRuleCoverage(rules []alertRule, want map[string]string) error {
	var errs []error
	got := map[string]string{}
	for _, r := range rules {
		if _, dup := got[r.Alert]; dup {
			errs = append(errs, fmt.Errorf("duplicate rule %q", r.Alert))
		}
		got[r.Alert] = r.Labels["severity"]
	}
	for name, sev := range want {
		g, ok := got[name]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("alert %q has no rule", name))
		case g != sev:
			errs = append(errs, fmt.Errorf("alert %q: severity %q, want %q", name, g, sev))
		}
	}
	return errors.Join(errs...)
}

func registeredFamilies() map[string]MetricInfo {
	fams := map[string]MetricInfo{}
	for _, f := range NewMetricsObserver().Registry().Families() {
		fams[f.Name] = f
	}
	return fams
}

func loadAlertRules(t *testing.T) []alertRule {
	t.Helper()
	b, err := os.ReadFile(alertRulesPath)
	if err != nil {
		t.Fatal(err)
	}
	var f alertRuleFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse %s: %v", alertRulesPath, err)
	}
	var rules []alertRule
	for _, g := range f.Groups {
		rules = append(rules, g.Rules...)
	}
	if len(rules) == 0 {
		t.Fatalf("%s has no rules", alertRulesPath)
	}
	return rules
}

// wantAlertSeverities 是规则必须覆盖的告警名与 severity：全部 model.AlertNames（红线 P0 为 critical，其余 warning）
// 加上 policy/fault_matrix.yaml 四个故障的 alert.name / alert.level。
func wantAlertSeverities(t *testing.T) map[string]string {
	t.Helper()
	m, err := policy.LoadFaultMatrix(faultMatrixPath, faultTextsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, n := range model.AlertNames {
		want[string(n)] = "warning"
		if p0SinkAlerts[string(n)] {
			want[string(n)] = "critical"
		}
	}
	for _, f := range m.Faults() {
		want[f.Alert.Name] = f.Alert.Level
	}
	return want
}

// 规则文件：每条规则的指标 / label / 取值 / 级别 / 注解都合法。
func TestAlertRules_File(t *testing.T) {
	fams := registeredFamilies()
	for _, r := range loadAlertRules(t) {
		if err := checkRule(r, fams); err != nil {
			t.Error(err)
		}
	}
}

// 覆盖：全部告警项与 fault_matrix 告警名都有规则；as_fault_llm_unavailable 为 critical/P0，其余三个故障为 warning。
func TestAlertRules_CoverAlertNamesAndFaultMatrix(t *testing.T) {
	rules := loadAlertRules(t)
	want := wantAlertSeverities(t)
	if err := checkRuleCoverage(rules, want); err != nil {
		t.Fatal(err)
	}
	faults := map[string]string{"as_fault_llm_unavailable": "critical", "as_fault_mcp_timeout": "warning",
		"as_fault_kb_miss": "warning", "as_fault_device_offline": "warning"}
	for name, sev := range faults {
		if want[name] != sev {
			t.Errorf("fault_matrix %s level = %q, want %q", name, want[name], sev)
		}
	}
	for _, r := range rules {
		if r.Alert == "as_fault_llm_unavailable" && r.Labels["priority"] != "P0" {
			t.Errorf("as_fault_llm_unavailable priority = %q, want P0", r.Labels["priority"])
		}
	}
}

// 应失败用例：校验器必须拒绝这些规则。
func TestAlertRules_CheckerRejects(t *testing.T) {
	good := alertRule{Alert: "advice_no_citation",
		Expr:        `sum by (farm_id) (increase(as_alert_total{alert="advice_no_citation"}[2m])) > 0`,
		Labels:      map[string]string{"severity": "critical", "priority": "P0", "receiver": "oncall_engineer"},
		Annotations: map[string]string{"summary": "s", "drilldown": "/api/v1/farms/1/alerts", "threshold": "任一次（待实测）"}}
	fams := registeredFamilies()
	if err := checkRule(good, fams); err != nil {
		t.Fatalf("baseline rule rejected: %v", err)
	}
	tests := []struct {
		name    string
		mutate  func(r *alertRule)
		wantErr string
	}{
		{name: "unknown metric", mutate: func(r *alertRule) { r.Expr = `increase(as_made_up_total[5m]) > 0` },
			wantErr: `unknown metric "as_made_up_total"`},
		{name: "label not on family", mutate: func(r *alertRule) { r.Expr = `increase(as_mcp_calls_total{farm_id="1"}[5m]) > 0` },
			wantErr: `has no label "farm_id"`},
		{name: "unknown alert value", mutate: func(r *alertRule) { r.Expr = `increase(as_alert_total{alert="nope"}[5m]) > 0` },
			wantErr: `value "nope" not in enum`},
		{name: "unknown regex value", mutate: func(r *alertRule) {
			r.Expr = `increase(as_dispatch_total{result=~"ok|lost"}[5m]) > 0`
		}, wantErr: `value "lost" not in enum`},
		{name: "grouping by missing label", mutate: func(r *alertRule) { r.Expr = `sum by (farm_id) (increase(as_mcp_calls_total[5m])) > 0` },
			wantErr: `grouping label "farm_id" not on as_mcp_calls_total`},
		{name: "run_id in expr", mutate: func(r *alertRule) { r.Expr = `sum by (run_id) (increase(as_alert_total[5m])) > 0` },
			wantErr: `forbidden label "run_id"`},
		{name: "severity priority mismatch", mutate: func(r *alertRule) { r.Labels["priority"] = "P1" },
			wantErr: "needs priority P0"},
		{name: "bad severity", mutate: func(r *alertRule) { r.Labels["severity"] = "page" }, wantErr: "not critical|warning"},
		{name: "no drilldown", mutate: func(r *alertRule) { delete(r.Annotations, "drilldown") },
			wantErr: "annotations.drilldown is empty"},
		{name: "threshold not draft", mutate: func(r *alertRule) { r.Annotations["threshold"] = "> 0" },
			wantErr: "not marked 待实测"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := good
			r.Labels = maps.Clone(good.Labels)
			r.Annotations = maps.Clone(good.Annotations)
			tt.mutate(&r)
			err := checkRule(r, fams)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}

	cov := []struct {
		name    string
		rules   []alertRule
		wantErr string
	}{
		{name: "missing rule", rules: nil, wantErr: `alert "advice_no_citation" has no rule`},
		{name: "wrong severity", rules: []alertRule{{Alert: "as_fault_llm_unavailable", Labels: map[string]string{"severity": "warning"}}},
			wantErr: `severity "warning", want "critical"`},
		{name: "duplicate", rules: []alertRule{good, good}, wantErr: `duplicate rule "advice_no_citation"`},
	}
	want := map[string]string{"advice_no_citation": "critical", "as_fault_llm_unavailable": "critical"}
	for _, tt := range cov {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRuleCoverage(tt.rules, want)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// anyOccurrenceRules 是「任一次即告警」的规则：6 条红线、as_fault_llm_unavailable、as_fault_device_offline、flag_changed。
// 这些规则必须带新序列分支（审查修复 P0：首样本为 1 时 increase 算不出）。
var anyOccurrenceRules = func() map[string]bool {
	m := map[string]bool{"as_fault_llm_unavailable": true, "as_fault_device_offline": true,
		string(model.AlertFlagChanged): true}
	for n := range p0SinkAlerts {
		m[n] = true
	}
	return m
}()

// listDrilldownRules 是有文档级下钻（ALERT.* 审计或业务审计）、指向告警列表的规则；其余规则是纯指标告警，指向看板 /metrics。
var listDrilldownRules = func() map[string]bool {
	m := map[string]bool{string(model.AlertCrossFarmDenied): true, string(model.AlertFlagChanged): true}
	for n := range p0SinkAlerts {
		m[n] = true
	}
	return m
}()

var (
	offsetRef = regexp.MustCompile(`\boffset\s+\d+[smhd]\b`)
	orRef     = regexp.MustCompile(`\bor\b`)
	unlessRef = regexp.MustCompile(`\bunless\b`)
)

// checkNewSeriesBranch 校验「任一次即告警」规则带 `or (X > 0 unless X offset w)` 分支。
func checkNewSeriesBranch(r alertRule) error {
	if !anyOccurrenceRules[r.Alert] {
		return nil
	}
	var errs []error
	for _, kw := range []struct {
		name string
		re   *regexp.Regexp
	}{{"increase", regexp.MustCompile(`\bincrease\(`)}, {"or", orRef}, {"unless", unlessRef}, {"offset", offsetRef}} {
		if !kw.re.MatchString(r.Expr) {
			errs = append(errs, fmt.Errorf("rule %s: any-occurrence rule missing %q (new-series branch)", r.Alert, kw.name))
		}
	}
	return errors.Join(errs...)
}

// checkDrilldownTarget 校验下钻目标：有审计行的指向 /alerts?name={alert}，纯指标告警指向 /metrics（不得指向空列表）。
func checkDrilldownTarget(r alertRule) error {
	d := r.Annotations["drilldown"]
	if listDrilldownRules[r.Alert] {
		if !strings.HasSuffix(d, "/alerts?name="+r.Alert) {
			return fmt.Errorf("rule %s: drilldown %q, want alert list ?name=%s", r.Alert, d, r.Alert)
		}
		return nil
	}
	if !strings.HasSuffix(d, "/metrics") {
		return fmt.Errorf("rule %s: metric-only alert drilldown %q, want farm /metrics dashboard", r.Alert, d)
	}
	return nil
}

// 审查修复 P0 / P2：规则文件里「任一次即告警」的规则都带新序列分支；下钻目标与是否有审计行一致。
func TestAlertRules_NewSeriesAndDrilldown(t *testing.T) {
	rules := loadAlertRules(t)
	seen := map[string]bool{}
	for _, r := range rules {
		seen[r.Alert] = true
		if err := checkNewSeriesBranch(r); err != nil {
			t.Error(err)
		}
		if err := checkDrilldownTarget(r); err != nil {
			t.Error(err)
		}
	}
	for n := range anyOccurrenceRules {
		if !seen[n] {
			t.Errorf("any-occurrence rule %s missing from %s", n, alertRulesPath)
		}
	}

	// 应失败用例：校验器必须拒绝缺新序列分支的红线规则、指向空告警列表的纯指标规则、指向 /metrics 的红线规则。
	bad := []struct {
		name    string
		rule    alertRule
		check   func(alertRule) error
		wantErr string
	}{
		{name: "P0 without new-series branch", check: checkNewSeriesBranch, wantErr: `missing "unless"`,
			rule: alertRule{Alert: "advice_no_citation", Expr: `sum by (farm_id) (increase(as_alert_total{alert="advice_no_citation"}[2m])) > 0`}},
		{name: "unless without offset", check: checkNewSeriesBranch, wantErr: `missing "offset"`,
			rule: alertRule{Alert: "model_actuate", Expr: `(increase(as_alert_total[2m]) > 0) or (as_alert_total > 0 unless as_alert_total)`}},
		{name: "metric-only to alert list", check: checkDrilldownTarget, wantErr: "want farm /metrics",
			rule: alertRule{Alert: "approval_backlog", Annotations: map[string]string{"drilldown": "/api/v1/farms/1/alerts?name=approval_backlog"}}},
		{name: "sink alert to metrics", check: checkDrilldownTarget, wantErr: "want alert list",
			rule: alertRule{Alert: "model_actuate", Annotations: map[string]string{"drilldown": "/api/v1/farms/1/metrics"}}},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.check(tt.rule); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// 审查修复 P0：规则引用的计数器序列在构造注册表时（farm_id=0）与 Preseed(已知农场) 后以 0 值存在，
// 第一次计数是 0 → 1，increase() 可见；未预置的农场不出现（应失败的反例：确认不是无条件全量输出）。
func TestMetricsObserver_PreseededZeroSeries(t *testing.T) {
	obs := NewMetricsObserver()
	base := metricsText(t, obs)
	for _, want := range []string{
		MetricAlerts + `{alert="advice_no_citation",level="P0",farm_id="0"} 0`,
		MetricDegraded + `{farm_id="0",fault="LLM_UNAVAILABLE"} 0`,
	} {
		if !strings.Contains(base, want) {
			t.Fatalf("missing preseeded %q:\n%s", want, base)
		}
	}
	if strings.Contains(base, `farm_id="7"`) {
		t.Fatal("farm 7 present before Preseed")
	}
	obs.Preseed([]uint64{7})
	obs.Preseed([]uint64{7}) // 幂等
	got := metricsText(t, obs)
	for _, want := range []string{
		MetricAlerts + `{alert="pre_approval_device",level="P0",farm_id="7"} 0`,
		MetricDegraded + `{farm_id="7",fault="DEVICE_OFFLINE"} 0`,
		MetricOrderTransitions + `{farm_id="7",to="EXPIRED",reason="APPROVAL_EXPIRED"} 0`,
		MetricOrderTransitions + `{farm_id="7",to="PENDING_APPROVAL",reason="RECHECK_FAILED"} 0`,
		MetricDispatch + `{farm_id="7",mode="REAL",result="error"} 0`,
		MetricLLMTokens + `{farm_id="7",kind="prompt"} 0`,
		MetricFlagChanges + `{flag="` + string(model.FlagEnableRobotDispatch) + `"} 0`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing preseeded %q:\n%s", want, got)
		}
	}
	// 首次告警：0 → 1（不是新序列首样本 1）。
	obs.AlertFired(model.Alert{Name: model.AlertPreApprovalDevice, Level: model.AlertP0, Scope: model.Scope{TenantID: 1, FarmID: 7}})
	if after := metricsText(t, obs); !strings.Contains(after,
		MetricAlerts+`{alert="pre_approval_device",level="P0",farm_id="7"} 1`) {
		t.Fatalf("counter not incremented from preseeded 0:\n%s", after)
	}
	obs.Preseed([]uint64{7})
	if after := metricsText(t, obs); !strings.Contains(after,
		MetricAlerts+`{alert="pre_approval_device",level="P0",farm_id="7"} 1`) {
		t.Fatal("Preseed reset an existing series")
	}
}
