package config

import (
	"errors"
	"strings"
	"testing"
)

func TestAttribution_Validate(t *testing.T) {
	if err := DefaultAttribution().Validate(); err != nil {
		t.Fatal(err)
	}
	err := Attribution{Timezone: "Mars/Base"}.Validate()
	if !errors.Is(err, ErrBadAttribution) {
		t.Fatalf("err = %v, want ErrBadAttribution", err)
	}
	for _, w := range []string{"timezone", "top_n", "interval", "drafts_dir", "kb_miss_min_hits", "max_runs", "service_user_id"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("err %v missing %s", err, w)
		}
	}
	bad := DefaultAttribution()
	bad.Backflow.DraftsDir = "eval/../drafts"
	if err := bad.Validate(); !errors.Is(err, ErrBadAttribution) {
		t.Fatalf("unclean drafts_dir err = %v", err)
	}
	if loc, err := DefaultAttribution().Location(); err != nil || loc.String() != DefaultAttributionTimezone {
		t.Fatalf("location = %v, %v", loc, err)
	}
}

// TestLoad_RepoFileAttribution：仓库配置与缺省一致（两项任务缺省不启用）；环境变量可覆盖，非法值启动失败。
func TestLoad_RepoFileAttribution(t *testing.T) {
	c, err := Load("../../configs/app.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Attribution != DefaultAttribution() {
		t.Fatalf("attribution = %+v, want %+v", c.Attribution, DefaultAttribution())
	}
	t.Setenv("AS_ATTRIBUTION_TOP_N", "5")
	t.Setenv("AS_ATTRIBUTION_BACKFLOW_ENABLED", "true")
	c, err = Load("../../configs/app.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Attribution.TopN != 5 || !c.Attribution.Backflow.Enabled {
		t.Fatalf("override = %+v", c.Attribution)
	}
	t.Setenv("AS_ATTRIBUTION_TOP_N", "0")
	if _, err := Load("../../configs/app.yaml", ""); !errors.Is(err, ErrBadAttribution) {
		t.Fatalf("err = %v, want ErrBadAttribution", err)
	}
}
