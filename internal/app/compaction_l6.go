package app

import (
	"fmt"

	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/agent/harness/compaction"
	"agri-steward/internal/config"
	"agri-steward/internal/service"
)

// DefaultLexiconPath 是用户口述约束词表（P2 L6 白名单 user_constraints 的命中依据）。
const DefaultLexiconPath = "policy/user_constraint_lexicon.yaml"

// CompactionDeps 是 P2 L6 长对话压缩的装配依赖（零值：不装配压缩，主链路与 ask 入口行为同 P1）。
// 装配后仍只在本农场 enable_compaction 开时生效。
type CompactionDeps struct {
	Config  config.Compaction   // configs/app.yaml 的 compaction 段
	Lexicon *compaction.Lexicon // compaction.LoadLexicon(DefaultLexiconPath)
}

// enabled 表示是否装配压缩（词表为空即不装配）。
func (d CompactionDeps) enabled() bool { return d.Lexicon != nil }

// LoadCompaction 读约束词表并组出 CompactionDeps（cmd/server、cmd/worker 与评测共用）。
func LoadCompaction(cfg config.Compaction, lexiconPath string) (CompactionDeps, error) {
	lex, err := compaction.LoadLexicon(lexiconPath)
	if err != nil {
		return CompactionDeps{}, fmt.Errorf("app: %w", err)
	}
	return CompactionDeps{Config: cfg, Lexicon: lex}, nil
}

// compactionEngine 构造主链路的压缩引擎（graph.Config.Compaction）：会话原料取 Runs，原文转存 RawStore；未装配返回 nil。
func (a *App) compactionEngine() (*compaction.Engine, error) {
	d := a.compaction
	if !d.enabled() || a.Runs == nil {
		return nil, nil
	}
	e, err := compaction.NewEngine(compaction.Deps{Config: compaction.Config{TriggerTokens: d.Config.TriggerTokens,
		KeepRecentTurns: d.Config.KeepRecentTurns, SummaryMaxTokens: d.Config.SummaryMaxTokens,
		MaxBlockTokens: d.Config.MaxBlockTokens, SummaryTimeout: d.Config.SummaryTimeout},
		Lexicon: d.Lexicon, Source: a.Runs, Raw: a.RawStore, Clock: a.Clock, Mask: pii.Mask, Log: a.Logger})
	if err != nil {
		return nil, fmt.Errorf("app: build compaction engine: %w", err)
	}
	return e, nil
}

// withSessions 给 ask 入口挂会话续接（prev_run_id / 复检取代复制 compaction_state）；未装配压缩时不挂，同 P1。
func (a *App) withSessions(d *service.AskDeps) {
	if !a.compaction.enabled() || a.Runs == nil {
		return
	}
	d.Sessions, d.SessionIdle = a.Runs, a.compaction.Config.SessionIdle
}
