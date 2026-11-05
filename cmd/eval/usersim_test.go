package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agri-steward/internal/evalkit/usersim"
)

// P2 L7-b：run / resume 装配模拟用户通道：默认取 --repo 下仓库配置（model=replay），加载成功即挂到 loadedSamples，
// 供 newRunner 注入；配置非法时失败关闭，不退回脚本用户。
func TestUsersimFlags_Attach(t *testing.T) {
	var def usersimFlags
	ls := &loadedSamples{}
	if err := def.attach(ls, repoRoot); err != nil {
		t.Fatal(err)
	}
	if ls.users == nil {
		t.Fatal("user channel not attached")
	}
	// 评审 P2：模拟用户 model / model_tier 留给快照记录（不再丢弃）。
	if ls.usersimCfg.Model == "" || ls.usersimCfg.ModelTier == "" {
		t.Fatalf("usersim config not kept: %+v", ls.usersimCfg)
	}

	bad := filepath.Join(t.TempDir(), "usersim.yaml")
	if err := os.WriteFile(bad, []byte("eval:\n  usersim:\n    model: gpt\n    model_tier: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := usersimFlags{config: bad}
	if err := f.attach(&loadedSamples{}, repoRoot); !errors.Is(err, usersim.ErrBadConfig) {
		t.Fatalf("bad config: err = %v", err)
	}
	missing := usersimFlags{config: filepath.Join(t.TempDir(), "none.yaml")}
	if err := missing.attach(&loadedSamples{}, repoRoot); err == nil {
		t.Fatal("missing config accepted")
	}
}
