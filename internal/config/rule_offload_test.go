package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRuleOffload_Validate(t *testing.T) {
	if err := DefaultRuleOffload().Validate(); err != nil {
		t.Fatal(err)
	}
	err := RuleOffload{ShadowDiffDays: -1}.Validate()
	if !errors.Is(err, ErrBadRuleOffload) {
		t.Fatalf("err = %v, want ErrBadRuleOffload", err)
	}
	for _, w := range []string{"shadow_diff_days", "purge_every"} {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("err %v missing %s", err, w)
		}
	}
	if got := DefaultRuleOffload().Retention(); got != 180*24*time.Hour {
		t.Fatalf("retention = %s", got)
	}
}

func TestLoad_RepoFileRuleOffload(t *testing.T) {
	c, err := Load("../../configs/app.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.RuleOffload != DefaultRuleOffload() {
		t.Fatalf("rule_offload = %+v, want %+v", c.RuleOffload, DefaultRuleOffload())
	}
}

// TestLoad_RuleOffloadEnvOverride：环境变量可覆盖，非法值启动失败。
func TestLoad_RuleOffloadEnvOverride(t *testing.T) {
	t.Setenv("AS_RULE_OFFLOAD_SHADOW_DIFF_DAYS", "30")
	c, err := Load("../../configs/app.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.RuleOffload.ShadowDiffDays != 30 {
		t.Fatalf("shadow_diff_days = %d, want 30", c.RuleOffload.ShadowDiffDays)
	}
	t.Setenv("AS_RULE_OFFLOAD_SHADOW_DIFF_DAYS", "0")
	if _, err := Load("../../configs/app.yaml", ""); !errors.Is(err, ErrBadRuleOffload) {
		t.Fatalf("err = %v, want ErrBadRuleOffload", err)
	}
}
