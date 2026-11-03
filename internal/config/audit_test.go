package config

import (
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestRetentionRequireKinds(t *testing.T) {
	kinds := []string{"LLM_IO", "EXPORT"}
	cases := []struct {
		name string
		r    Retention
		want []string // 期望错误里出现的片段；空表示通过
	}{
		{name: "complete, viper lowercased keys", r: Retention{RawObjectDays: map[string]int{"llm_io": 30, "export": 7},
			TableDays: map[string]int{"audit_log": 1095}}},
		{name: "missing kind", r: Retention{RawObjectDays: map[string]int{"llm_io": 30}},
			want: []string{"raw_object_days.EXPORT must be > 0"}},
		{name: "zero days", r: Retention{RawObjectDays: map[string]int{"llm_io": 0, "export": 7}},
			want: []string{"raw_object_days.LLM_IO must be > 0"}},
		{name: "unknown kind", r: Retention{RawObjectDays: map[string]int{"llm_io": 30, "export": 7, "llm_oi": 3}},
			want: []string{"LLM_OI is not a raw_object kind"}},
		{name: "bad table days", r: Retention{RawObjectDays: map[string]int{"llm_io": 30, "export": 7},
			TableDays: map[string]int{"run_event": -1}}, want: []string{"table_days.run_event must be > 0"}},
		{name: "empty config", want: []string{"LLM_IO", "EXPORT"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.r.RequireKinds(kinds)
			if len(c.want) == 0 {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %v", c.want)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("err %q missing %q", err, w)
				}
			}
		})
	}
}

func TestLoad_RepoFileRetentionAndAudit(t *testing.T) {
	c, err := Load("../../configs/app.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Audit.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Audit.AnchorBucket != DefaultAnchorBucket || c.Audit.ExportMaxSpan != 31*24*time.Hour {
		t.Fatalf("audit = %+v", c.Audit)
	}
	if d := c.Retention.RawDays(); d["LLM_IO"] <= 0 || d["EXPORT"] <= 0 {
		t.Fatalf("raw days = %v", d)
	}
}

func TestAuditValidate(t *testing.T) {
	if err := DefaultAudit().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := Audit{}
	err := bad.Validate()
	for _, w := range []string{"anchor_bucket", "anchor_lock_days", "export_max_span"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Fatalf("err %v missing %s", err, w)
		}
	}
}

func TestReadOnlyDSN(t *testing.T) {
	m := MySQL{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p@ss", Database: "agri_steward"}
	dsn, err := m.ReadOnlyDSN(m.Database)
	if err != nil {
		t.Fatal(err)
	}
	c, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if c.Params["transaction_read_only"] != "1" || c.DBName != "agri_steward" || c.Passwd != "p@ss" {
		t.Fatalf("parsed = db %s params %v", c.DBName, c.Params)
	}
}
