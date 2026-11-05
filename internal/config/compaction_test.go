package config

import (
	"errors"
	"testing"
)

func TestCompaction_Validate(t *testing.T) {
	ok := Compaction{TriggerTokens: DefaultCompactionTriggerTokens, SessionIdle: DefaultCompactionSessionIdle,
		KeepRecentTurns: DefaultCompactionKeepRecentTurns, SummaryMaxTokens: DefaultCompactionSummaryMaxTokens,
		MaxBlockTokens: DefaultCompactionMaxBlockTokens, SummaryTimeout: DefaultCompactionSummaryTimeout}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(c *Compaction){
		"trigger 0":          func(c *Compaction) { c.TriggerTokens = 0 },
		"idle 0":             func(c *Compaction) { c.SessionIdle = 0 },
		"keep 0":             func(c *Compaction) { c.KeepRecentTurns = 0 },
		"summary 0":          func(c *Compaction) { c.SummaryMaxTokens = 0 },
		"summary >= trigger": func(c *Compaction) { c.SummaryMaxTokens = c.TriggerTokens },
		"max <= trigger":     func(c *Compaction) { c.MaxBlockTokens = c.TriggerTokens },
		"timeout 0":          func(c *Compaction) { c.SummaryTimeout = 0 },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, ErrBadCompaction) {
			t.Errorf("%s: err = %v, want ErrBadCompaction", name, err)
		}
	}
}

// 仓库自带的 configs/app.yaml 带 compaction 段，取值与缺省一致。
func TestLoad_CompactionFromRepoConfig(t *testing.T) {
	c, err := Load("../../configs/app.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	want := Compaction{TriggerTokens: DefaultCompactionTriggerTokens, SessionIdle: DefaultCompactionSessionIdle,
		KeepRecentTurns: DefaultCompactionKeepRecentTurns, SummaryMaxTokens: DefaultCompactionSummaryMaxTokens,
		MaxBlockTokens: DefaultCompactionMaxBlockTokens, SummaryTimeout: DefaultCompactionSummaryTimeout}
	if c.Compaction != want {
		t.Fatalf("compaction = %+v, want %+v", c.Compaction, want)
	}
}
