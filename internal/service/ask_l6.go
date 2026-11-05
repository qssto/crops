package service

import (
	"context"
	"fmt"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// P2 L6 会话续接建 run（P2 §6.7 技术实现 2）：带 prev_run_id 的提问与复检取代的新 run，在 enable_compaction 开时
// 经 SessionRunCreator 在 INSERT 新 run 的同一事务校验并复制 compaction_state；开关关 / 读失败 / 未装配时与 P1 相同，走 CreateRun。

// SessionRunCreator 按会话关联建 run，repository.RunRepo 实现；返回 run_id 与是否续接成功（校验不通过即开新会话）。
type SessionRunCreator interface {
	CreateRunLinked(ctx context.Context, s model.RunState, userID, fieldID uint64, meta agent.RunMeta,
		link model.RunLink) (uint64, bool, error)
}

// CreateRun 只建本次提问的 run（含会话续接），不校验、不限流、不启动主链路。供评测 Runner 在同步执行主链路前
// 按生产路径建带 prev_run_id 的 run（P2 L6 多轮样本）；线上入口仍走 Ask。
func (s *AskService) CreateRun(ctx context.Context, scope model.Scope, in AskInput) (uint64, error) {
	id, err := s.createRun(ctx, scope, in)
	if err != nil {
		return 0, fmt.Errorf("service: create run: %w", err)
	}
	return id, nil
}

// createRun 建本次提问的 run：无会话关联或开关不生效时走 P1 的 CreateRun。
func (s *AskService) createRun(ctx context.Context, scope model.Scope, in AskInput) (uint64, error) {
	rs := model.RunState{TenantID: scope.TenantID, FarmID: scope.FarmID}
	link, ok := s.sessionLink(ctx, scope, in)
	if !ok {
		return s.d.Runs.CreateRun(ctx, rs, scope.UserID, in.FieldID, s.d.Meta)
	}
	id, linked, err := s.d.Sessions.CreateRunLinked(ctx, rs, scope.UserID, in.FieldID, s.d.Meta, link)
	if err != nil {
		return 0, fmt.Errorf("create linked run: %w", err)
	}
	s.d.Logger.InfoContext(ctx, "ask: session link", "run_id", id, "kind", link.Kind, "from_run_id", link.RunID,
		"linked", linked)
	return id, nil
}

// sessionLink 返回本次提问的会话关联；无关联、未装配 Sessions、enable_compaction 关或读失败（失败关闭）时 ok=false。
func (s *AskService) sessionLink(ctx context.Context, scope model.Scope, in AskInput) (model.RunLink, bool) {
	link := model.RunLink{Kind: model.RunLinkSession, RunID: in.PrevRunID, Idle: s.d.SessionIdle}
	if in.Supersedes != 0 {
		link = model.RunLink{Kind: model.RunLinkSupersede, RunID: in.Supersedes}
	}
	if link.RunID == 0 || s.d.Sessions == nil {
		return model.RunLink{}, false
	}
	on, err := s.d.Flags.Bool(ctx, model.FlagEnableCompaction, scope)
	if err != nil {
		s.d.Logger.WarnContext(ctx, "ask: enable_compaction read failed, new session", "from_run_id", link.RunID, "err", err)
		return model.RunLink{}, false
	}
	return link, on
}
