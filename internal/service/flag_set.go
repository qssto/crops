package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// FlagSetInput 是 PUT /api/v1/admin/flags/{flag_key}/scopes/{tenant_id}/{farm_id} 的入参。
// ExpectRowVer 非空时走乐观锁更新已有行（冲突 → ErrFlagConflict）；为空走 upsert，但已有 FARM 行必须带它。
type FlagSetInput struct {
	Key          model.FlagKey
	TenantID     uint64
	FarmID       uint64
	Value        json.RawMessage
	Comment      string
	ExpectRowVer *int
}

// FlagSetResult 是写入后的行状态。
type FlagSetResult struct {
	FlagKey   model.FlagKey       `json:"flag_key"`
	TenantID  uint64              `json:"tenant_id"`
	FarmID    uint64              `json:"farm_id"`
	ScopeType model.FlagScopeType `json:"scope_type"`
	Value     json.RawMessage     `json:"value"`
	RowVer    int                 `json:"row_ver"`
}

// flagChange 是 FLAG_CHANGE 审计的 summary（actor 在 actor_type / actor_id 列）。
type flagChange struct {
	FlagKey   model.FlagKey       `json:"flag_key"`
	ScopeType model.FlagScopeType `json:"scope_type"`
	TenantID  uint64              `json:"tenant_id"`
	FarmID    uint64              `json:"farm_id"`
	Old       json.RawMessage     `json:"old"` // 首次设置为 null
	New       json.RawMessage     `json:"new"`
	Comment   string              `json:"comment"`
	RowVer    int                 `json:"row_ver"`
}

// flagsChangedMessage 是 PUBLISH flags:changed 的消息体；消费方忽略未知字段。
type flagsChangedMessage struct {
	FlagKey model.FlagKey `json:"flag_key"`
}

// requirePlatformAdmin：开关读写只开放给平台 Scope (0,0) 上的 platform_admin（controller 已鉴权，这里纵深防御）。
func requirePlatformAdmin(actor model.Scope) error {
	if actor.TenantID != 0 || actor.FarmID != 0 || !actor.HasRole(model.RolePlatformAdmin) {
		return fmt.Errorf("service: flag admin needs platform_admin: %w", ErrRoleInsufficient)
	}
	return nil
}

// Set 校验形态与组合后，同事务写 feature_flag + audit_log(FLAG_CHANGE)，提交后清本地缓存并 PUBLISH flags:changed。
func (s *FlagService) Set(ctx context.Context, actor model.Scope, in FlagSetInput) (FlagSetResult, error) {
	w, err := s.validateSet(ctx, actor, in)
	if err != nil {
		return FlagSetResult{}, err
	}
	res := FlagSetResult{FlagKey: w.Key, TenantID: w.TenantID, FarmID: w.FarmID, ScopeType: w.ScopeType, Value: w.Value}
	err = s.d.Tx.InTx(ctx, func(tx *repository.DB) error {
		if err := s.lockRobotPrecondition(ctx, tx, w); err != nil {
			return err
		}
		cur, found, err := s.d.Store.LockTx(ctx, tx, w.Key, w.TenantID, w.FarmID)
		if err != nil {
			return err
		}
		if res.RowVer, err = s.write(ctx, tx, w, in.ExpectRowVer, cur, found); err != nil {
			return err
		}
		var old json.RawMessage
		if found {
			old = cur.Value
		}
		summary, err := json.Marshal(flagChange{FlagKey: w.Key, ScopeType: w.ScopeType, TenantID: w.TenantID, FarmID: w.FarmID,
			Old: old, New: w.Value, Comment: w.Comment, RowVer: res.RowVer})
		if err != nil {
			return fmt.Errorf("service: encode FLAG_CHANGE: %w", err)
		}
		return s.d.Audit.AppendTx(ctx, tx, repository.NewAuditRecord(model.Scope{TenantID: w.TenantID, FarmID: w.FarmID}, nil,
			model.Actor{Type: model.ActorUser, ID: actor.UserID}, model.AuditFlagChange, "flag:"+string(w.Key), summary,
			model.AuditResultOK, w.At))
	})
	if errors.Is(err, repository.ErrFlagWriteRace) {
		// 同一行并发首次写入：一方提交、一方死锁回滚，按冲突返回（409）让调用方重读后重试。
		err = fmt.Errorf("%w: %w", ErrFlagConflict, err)
	}
	if err != nil {
		return FlagSetResult{}, fmt.Errorf("service: set flag %s (%d,%d): %w", w.Key, w.TenantID, w.FarmID, err)
	}
	s.invalidate(w.Key)
	s.publish(ctx, w.Key)
	s.d.Logger.InfoContext(ctx, "flag changed", "flag_key", w.Key, "tenant_id", w.TenantID, "farm_id", w.FarmID,
		"value", string(w.Value), "row_ver", res.RowVer, "actor_id", actor.UserID)
	return res, nil
}

// validateSet 做权限、注册表、形态、作用域与组合校验，返回规范化后的写入行。
func (s *FlagService) validateSet(ctx context.Context, actor model.Scope, in FlagSetInput) (model.FlagWrite, error) {
	if err := requirePlatformAdmin(actor); err != nil {
		return model.FlagWrite{}, err
	}
	spec, ok := s.d.Registry[in.Key]
	if !ok {
		return model.FlagWrite{}, fmt.Errorf("service: flag %q: %w", in.Key, ErrFlagUnknown)
	}
	st, err := model.FlagScopeOf(in.TenantID, in.FarmID)
	if err != nil {
		return model.FlagWrite{}, fmt.Errorf("%w: %w", ErrFlagScope, err)
	}
	v, err := model.ParseFlagValue(spec, st, in.Value)
	if err != nil {
		return model.FlagWrite{}, fmt.Errorf("%w: %w", ErrFlagValue, err)
	}
	comment := strings.TrimSpace(in.Comment)
	if comment == "" || utf8.RuneCountInString(comment) > flagCommentMaxRunes {
		return model.FlagWrite{}, fmt.Errorf("%w: comment required, 1-%d chars", ErrBadInput, flagCommentMaxRunes)
	}
	if in.ExpectRowVer != nil && *in.ExpectRowVer < 0 {
		return model.FlagWrite{}, fmt.Errorf("%w: expect_row_ver must be >= 0", ErrBadInput)
	}
	if st != model.FlagScopeGlobal {
		exists, err := s.d.Store.ScopeExists(ctx, in.TenantID, in.FarmID)
		if err != nil {
			return model.FlagWrite{}, fmt.Errorf("service: set flag: %w", err)
		}
		if !exists {
			return model.FlagWrite{}, fmt.Errorf("%w: no farm under (%d,%d)", ErrFlagScope, in.TenantID, in.FarmID)
		}
	}
	if in.Key == model.FlagEnableRobotDispatch && st == model.FlagScopeFarm && v.FarmMode() == model.FlagModeOn {
		if err := s.checkRobotDispatchOn(ctx, in.TenantID, in.FarmID); err != nil {
			return model.FlagWrite{}, err
		}
	}
	value, err := json.Marshal(v)
	if err != nil {
		return model.FlagWrite{}, fmt.Errorf("service: encode flag value: %w", err)
	}
	return model.FlagWrite{TenantID: in.TenantID, FarmID: in.FarmID, Key: in.Key, ScopeType: st, Value: value,
		Comment: comment, UpdatedBy: actor.UserID, At: s.d.Clock.Now()}, nil
}

// lockRobotPrecondition：enable_robot_dispatch 置开时在写事务内锁住同农场 enable_advice 的 FARM 行再确认为开，
// 与并发关 enable_advice 的写入串行，避免两边都提交后出现「下发开、建议关」。
func (s *FlagService) lockRobotPrecondition(ctx context.Context, tx *repository.DB, w model.FlagWrite) error {
	if w.Key != model.FlagEnableRobotDispatch || w.ScopeType != model.FlagScopeFarm {
		return nil
	}
	v, err := model.ParseFlagValue(s.d.Registry[w.Key], w.ScopeType, w.Value)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFlagValue, err)
	}
	if v.FarmMode() != model.FlagModeOn {
		return nil
	}
	cur, found, err := s.d.Store.LockTx(ctx, tx, model.FlagEnableAdvice, w.TenantID, w.FarmID)
	if err != nil {
		return err
	}
	if found {
		av, err := model.ParseFlagValue(s.d.Registry[model.FlagEnableAdvice], model.FlagScopeFarm, cur.Value)
		if err == nil && av.FarmMode() == model.FlagModeOn {
			return nil
		}
	}
	return fmt.Errorf("%w: enable_advice changed on farm (%d,%d)", ErrFlagPrecondition, w.TenantID, w.FarmID)
}

// checkRobotDispatchOn：enable_robot_dispatch 置开要求同农场 enable_advice 生效（直读）且有 agronomist 绑定。
func (s *FlagService) checkRobotDispatchOn(ctx context.Context, tenantID, farmID uint64) error {
	advice, err := s.BoolLive(ctx, model.FlagEnableAdvice, model.Scope{TenantID: tenantID, FarmID: farmID})
	if err != nil {
		return fmt.Errorf("service: set flag precheck: %w", err)
	}
	if !advice {
		return fmt.Errorf("%w: enable_advice not effective on farm (%d,%d)", ErrFlagPrecondition, tenantID, farmID)
	}
	if s.d.FarmRoles == nil {
		return fmt.Errorf("%w: role checker not configured", ErrFlagPrecondition)
	}
	has, err := s.d.FarmRoles.FarmHasRole(ctx, tenantID, farmID, model.RoleAgronomist)
	if err != nil {
		return fmt.Errorf("service: set flag precheck: %w", err)
	}
	if !has {
		return fmt.Errorf("%w: farm (%d,%d) has no %s", ErrFlagPrecondition, tenantID, farmID, model.RoleAgronomist)
	}
	return nil
}

// write 执行写入①（upsert）或写入②（乐观锁），返回新 row_ver。
func (s *FlagService) write(ctx context.Context, tx *repository.DB, w model.FlagWrite, expect *int,
	cur repository.FlagCurrent, found bool) (int, error) {
	if expect == nil {
		// upsert 只用于首次设置与 GLOBAL / TENANT 的 kill switch；已有 FARM 行必须带 row_ver，防止丢失更新。
		if found && w.ScopeType == model.FlagScopeFarm {
			return 0, fmt.Errorf("%w: existing FARM row needs expect_row_ver (current %d)", ErrFlagConflict, cur.RowVer)
		}
		if err := s.d.Store.UpsertTx(ctx, tx, w); err != nil {
			return 0, err
		}
		if !found {
			return 0, nil
		}
		return cur.RowVer + 1, nil
	}
	ok, err := s.d.Store.UpdateTx(ctx, tx, w, *expect)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%w: expect row_ver %d, current %d (exists=%t)", ErrFlagConflict, *expect, cur.RowVer, found)
	}
	return *expect + 1, nil
}

// List 返回 key 的全部行（灰度档位一览）。
func (s *FlagService) List(ctx context.Context, actor model.Scope, key model.FlagKey) ([]model.FlagSetting, error) {
	if err := requirePlatformAdmin(actor); err != nil {
		return nil, err
	}
	if _, ok := s.d.Registry[key]; !ok {
		return nil, fmt.Errorf("service: flag %q: %w", key, ErrFlagUnknown)
	}
	rows, err := s.d.Store.List(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("service: list flag %s: %w", key, err)
	}
	return rows, nil
}

// publish 提交后广播 flags:changed；失败只告警（各进程回落 30s 缓存过期，下发开关另有直读兜底）。
func (s *FlagService) publish(ctx context.Context, key model.FlagKey) {
	body, err := json.Marshal(flagsChangedMessage{FlagKey: key})
	if err != nil {
		s.d.Logger.WarnContext(ctx, "flags:changed encode failed", "flag_key", key, "err", err)
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flagPublishTimeout)
	defer cancel()
	if err := s.d.Pub.Publish(pctx, s.d.Keys.Raw(flagsChangedChannel), body); err != nil {
		s.d.Logger.WarnContext(ctx, "flags:changed publish failed", "flag_key", key, "err", err)
	}
}

// Run 订阅 flags:changed 清本地缓存，直到 ctx 结束。订阅失败或断开后每 5s 重试；
// 重新订阅成功时清全部缓存（断开期间可能漏消息）。
func (s *FlagService) Run(ctx context.Context) {
	if s.d.Sub == nil {
		s.d.Logger.WarnContext(ctx, "flags:changed subscriber not configured, rely on cache TTL")
		return
	}
	for {
		if err := s.consume(ctx); err != nil {
			s.d.Logger.WarnContext(ctx, "flags:changed subscription lost", "err", err)
		}
		t := time.NewTimer(flagResubscribe)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// consume 订阅一次并处理消息；ctx 结束返回 nil。
func (s *FlagService) consume(ctx context.Context) (err error) {
	sub, err := s.d.Sub.Subscribe(ctx, s.d.Keys.Raw(flagsChangedChannel))
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer func() {
		if cerr := sub.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	s.invalidate("")
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-sub.Messages():
			if !ok {
				return errors.New("service: flags:changed channel closed")
			}
			s.onChanged(ctx, msg)
		}
	}
}

// onChanged 按消息里的 flag_key 清缓存；消息无法解析时清全部。
func (s *FlagService) onChanged(ctx context.Context, msg []byte) {
	var m flagsChangedMessage
	if err := json.Unmarshal(msg, &m); err != nil || m.FlagKey == "" {
		s.d.Logger.WarnContext(ctx, "flags:changed bad message, clear all", "msg", string(msg), "err", err)
		s.invalidate("")
		return
	}
	s.invalidate(m.FlagKey)
}
