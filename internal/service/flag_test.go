package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// Set 的权限 / 注册表 / 形态 / 作用域 / 组合 / 乐观锁校验（红线：开关读写，含应失败用例）。
func TestFlagService_Set(t *testing.T) {
	robot, advice := model.FlagEnableRobotDispatch, model.FlagEnableAdvice
	tests := []struct {
		name    string
		actor   model.Scope
		in      FlagSetInput
		mutate  func(*FlagDeps)
		prep    func(*flagFixture)
		wantErr error // nil 表示成功；errAny 表示任意错误
		wantVer int
	}{
		{name: "farm advice on", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 3, Value: raw(`{"on":true}`)}},
		{name: "global force_off", in: FlagSetInput{Key: robot, Value: raw(`{"force_off":true}`)}},
		{name: "tenant force_off", in: FlagSetInput{Key: robot, TenantID: 1, Value: raw(`{"force_off":false}`)}},
		{name: "robot on with advice + agronomist", in: FlagSetInput{Key: robot, TenantID: 1, FarmID: 1, Value: raw(`{"on":true}`)}},
		{name: "robot off needs no precondition", in: FlagSetInput{Key: robot, TenantID: 1, FarmID: 3, Value: raw(`{"on":false}`)}},
		{name: "existing farm row needs row_ver", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 1, Value: raw(`{"on":false}`)},
			wantErr: ErrFlagConflict},
		{name: "upsert existing tenant row bumps row_ver", in: FlagSetInput{Key: robot, TenantID: 1, Value: raw(`{"force_off":true}`)},
			prep: func(f *flagFixture) { f.store.put(robot, 1, 0, `{"force_off":false}`) }, wantVer: 1},
		{name: "optimistic update ok", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 1, Value: raw(`{"on":false}`),
			ExpectRowVer: intPtr(0)}, wantVer: 1},
		{name: "optimistic conflict", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 1, Value: raw(`{"on":false}`),
			ExpectRowVer: intPtr(4)}, wantErr: ErrFlagConflict},
		{name: "optimistic on missing row", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 3, Value: raw(`{"on":true}`),
			ExpectRowVer: intPtr(0)}, wantErr: ErrFlagConflict},
		{name: "negative expect_row_ver", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 3, Value: raw(`{"on":true}`),
			ExpectRowVer: intPtr(-1)}, wantErr: ErrBadInput},
		{name: "farm manager denied", actor: model.Scope{TenantID: 1, FarmID: 1, UserID: 1001,
			Roles: []model.Role{model.RoleFarmManager}}, in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 1,
			Value: raw(`{"on":true}`)}, wantErr: ErrRoleInsufficient},
		{name: "platform_admin role on farm scope denied", actor: model.Scope{TenantID: 1, FarmID: 1, UserID: 9001,
			Roles: []model.Role{model.RolePlatformAdmin}}, in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 1,
			Value: raw(`{"on":true}`)}, wantErr: ErrRoleInsufficient},
		{name: "platform scope without role denied", actor: model.Scope{UserID: 1001},
			in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 1, Value: raw(`{"on":true}`)}, wantErr: ErrRoleInsufficient},
		{name: "unknown key", in: FlagSetInput{Key: "enable_magic", TenantID: 1, FarmID: 1, Value: raw(`{"on":true}`)},
			wantErr: ErrFlagUnknown},
		{name: "global rejects on", in: FlagSetInput{Key: robot, Value: raw(`{"on":true}`)}, wantErr: ErrFlagValue},
		{name: "tenant rejects on", in: FlagSetInput{Key: advice, TenantID: 1, Value: raw(`{"on":true}`)}, wantErr: ErrFlagValue},
		{name: "farm rejects force_off", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 1, Value: raw(`{"force_off":true}`)},
			wantErr: ErrFlagValue},
		{name: "bool flag rejects mode", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 1, Value: raw(`{"mode":"on"}`)},
			wantErr: ErrFlagValue},
		{name: "farm without tenant", in: FlagSetInput{Key: advice, FarmID: 1, Value: raw(`{"on":true}`)}, wantErr: ErrFlagScope},
		{name: "unknown farm", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 99, Value: raw(`{"on":true}`)},
			wantErr: ErrFlagScope},
		{name: "unknown tenant", in: FlagSetInput{Key: advice, TenantID: 7, Value: raw(`{"force_off":true}`)},
			wantErr: ErrFlagScope},
		{name: "comment required", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 1, Value: raw(`{"on":true}`), Comment: " "},
			wantErr: ErrBadInput},
		{name: "comment too long", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 1, Value: raw(`{"on":true}`),
			Comment: strings.Repeat("灰", flagCommentMaxRunes+1)}, wantErr: ErrBadInput},
		{name: "robot on without advice", in: FlagSetInput{Key: robot, TenantID: 1, FarmID: 3, Value: raw(`{"on":true}`)},
			wantErr: ErrFlagPrecondition},
		{name: "robot on advice forced off by tenant", in: FlagSetInput{Key: robot, TenantID: 1, FarmID: 1, Value: raw(`{"on":true}`)},
			prep:    func(f *flagFixture) { f.store.put(model.FlagEnableAdvice, 1, 0, `{"force_off":true}`) },
			wantErr: ErrFlagPrecondition},
		{name: "robot on without agronomist", in: FlagSetInput{Key: robot, TenantID: 1, FarmID: 2, Value: raw(`{"on":true}`)},
			wantErr: ErrFlagPrecondition},
		{name: "robot on without role checker", in: FlagSetInput{Key: robot, TenantID: 1, FarmID: 1, Value: raw(`{"on":true}`)},
			mutate: func(d *FlagDeps) { d.FarmRoles = nil }, wantErr: ErrFlagPrecondition},
		{name: "robot on role lookup error", in: FlagSetInput{Key: robot, TenantID: 1, FarmID: 1, Value: raw(`{"on":true}`)},
			mutate: func(d *FlagDeps) { d.FarmRoles = memRoles{err: errors.New("db down")} }, wantErr: errAny},
		{name: "audit failure rolls back", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 3, Value: raw(`{"on":true}`)},
			prep: func(f *flagFixture) { f.store.auditFail = errors.New("audit down") }, wantErr: errAny},
		{name: "concurrent first write deadlock is conflict", in: FlagSetInput{Key: advice, TenantID: 1, FarmID: 3,
			Value: raw(`{"on":true}`)}, prep: func(f *flagFixture) {
			f.store.writeFail = fmt.Errorf("upsert: %w", repository.ErrFlagWriteRace)
		}, wantErr: ErrFlagConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFlagFixture(t, tt.mutate)
			if tt.prep != nil {
				tt.prep(f)
			}
			actor := tt.actor
			if actor.UserID == 0 {
				actor = flagAdmin
			}
			in := tt.in
			if in.Comment == "" {
				in.Comment = "灰度第一档"
			}
			before := f.store.rows[flagLoc{in.Key, in.TenantID, in.FarmID}]
			res, err := f.svc.Set(context.Background(), actor, in)
			switch {
			case tt.wantErr == nil && err != nil:
				t.Fatalf("Set err = %v", err)
			case tt.wantErr == errAny && err == nil, tt.wantErr != nil && tt.wantErr != errAny && !errors.Is(err, tt.wantErr):
				t.Fatalf("Set err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				after := f.store.rows[flagLoc{in.Key, in.TenantID, in.FarmID}]
				if string(after.value) != string(before.value) || after.rowVer != before.rowVer || len(f.store.audits) != 0 ||
					len(f.pub.msgs) != 0 {
					t.Errorf("side effects on failure: row %+v -> %+v audits=%d pubs=%d", before, after,
						len(f.store.audits), len(f.pub.msgs))
				}
				return
			}
			checkFlagChange(t, f, actor, in, before, res, tt.wantVer)
		})
	}
}

// checkFlagChange：行已写、row_ver 正确、一条 FLAG_CHANGE 审计（actor / 旧值 / 新值 / scope）、一条 flags:changed 广播。
func checkFlagChange(t *testing.T, f *flagFixture, actor model.Scope, in FlagSetInput, before memFlagRow, res FlagSetResult,
	wantVer int) {
	t.Helper()
	row := f.store.rows[flagLoc{in.Key, in.TenantID, in.FarmID}]
	if string(row.value) != string(in.Value) || row.rowVer != wantVer || res.RowVer != wantVer {
		t.Errorf("row = %s ver %d, result ver %d, want %s ver %d", row.value, row.rowVer, res.RowVer, in.Value, wantVer)
	}
	if len(f.store.audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(f.store.audits))
	}
	a := f.store.audits[0]
	var ch flagChange
	if err := json.Unmarshal(a.Summary, &ch); err != nil {
		t.Fatal(err)
	}
	wantOld := string(before.value)
	if wantOld == "" {
		wantOld = "null"
	}
	if a.Action != string(model.AuditFlagChange) || a.Actor != (model.Actor{Type: model.ActorUser, ID: actor.UserID}) ||
		a.TenantID != in.TenantID || a.FarmID != in.FarmID || a.Target != "flag:"+string(in.Key) || a.Result != model.AuditResultOK ||
		!a.CreatedAt.Equal(flagNow) || string(ch.Old) != wantOld || string(ch.New) != string(in.Value) ||
		ch.Comment != in.Comment || ch.FlagKey != in.Key || ch.RowVer != wantVer {
		t.Errorf("audit = %+v summary %s", a, a.Summary)
	}
	if len(f.pub.msgs) != 1 || f.pub.msgs[0] != `as:test:flags:changed {"flag_key":"`+string(in.Key)+`"}` {
		t.Errorf("published = %v", f.pub.msgs)
	}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// 广播失败不回滚已提交的变更（各进程回落缓存过期）。
func TestFlagService_SetPublishFailure(t *testing.T) {
	f := newFlagFixture(t, nil)
	f.pub.err = errors.New("redis down")
	if _, err := f.svc.Set(context.Background(), flagAdmin, FlagSetInput{Key: model.FlagEnableAdvice, TenantID: 1, FarmID: 3,
		Value: raw(`{"on":true}`), Comment: "x"}); err != nil {
		t.Fatalf("Set err = %v", err)
	}
	if len(f.store.audits) != 1 {
		t.Errorf("audits = %d", len(f.store.audits))
	}
}

// 三层合并真值表：生效值 = !force_off(GLOBAL) && !force_off(TENANT) && FARM 值，无 FARM 行取默认（关）。
func TestMergeFlag(t *testing.T) {
	row := func(st model.FlagScopeType, v string) model.FlagRow {
		return model.FlagRow{ScopeType: st, Value: raw(v)}
	}
	g, tn, fm := model.FlagScopeGlobal, model.FlagScopeTenant, model.FlagScopeFarm
	boolSpec := model.FlagSpec{Kind: model.FlagKindBool, Default: model.FlagModeOff}
	tests := []struct {
		name    string
		spec    model.FlagSpec
		rows    []model.FlagRow
		want    model.FlagMode
		wantErr bool
	}{
		{"no rows default off", boolSpec, nil, model.FlagModeOff, false},
		{"default on spec", model.FlagSpec{Kind: model.FlagKindBool, Default: model.FlagModeOn}, nil, model.FlagModeOn, false},
		{"farm on", boolSpec, []model.FlagRow{row(fm, `{"on":true}`)}, model.FlagModeOn, false},
		{"farm off", boolSpec, []model.FlagRow{row(fm, `{"on":false}`)}, model.FlagModeOff, false},
		{"global off-flag false + farm on", boolSpec, []model.FlagRow{row(g, `{"force_off":false}`), row(fm, `{"on":true}`)},
			model.FlagModeOn, false},
		{"global force_off beats farm on", boolSpec, []model.FlagRow{row(g, `{"force_off":true}`), row(fm, `{"on":true}`)},
			model.FlagModeOff, false},
		{"tenant force_off beats farm on", boolSpec, []model.FlagRow{row(tn, `{"force_off":true}`), row(fm, `{"on":true}`)},
			model.FlagModeOff, false},
		{"both clear + farm on", boolSpec, []model.FlagRow{row(g, `{"force_off":false}`), row(tn, `{"force_off":false}`),
			row(fm, `{"on":true}`)}, model.FlagModeOn, false},
		{"global clear without farm row stays off", boolSpec, []model.FlagRow{row(g, `{"force_off":false}`)},
			model.FlagModeOff, false},
		{"mode shadow", model.FlagSpec{Kind: model.FlagKindMode, Default: model.FlagModeOff},
			[]model.FlagRow{row(fm, `{"mode":"shadow"}`)}, model.FlagModeShadow, false},
		{"mode forced off", model.FlagSpec{Kind: model.FlagKindMode, Default: model.FlagModeOff},
			[]model.FlagRow{row(tn, `{"force_off":true}`), row(fm, `{"mode":"on"}`)}, model.FlagModeOff, false},
		{"corrupt stored row fails closed", boolSpec, []model.FlagRow{row(g, `{"on":true}`), row(fm, `{"on":true}`)},
			model.FlagModeOff, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mergeFlag(tt.spec, tt.rows)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("mergeFlag = %s, %v; want %s err=%t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// 验收 2：单农场开关只影响该农场；GLOBAL force_off 置真再置假后只有原 FARM on=true 的农场恢复。
func TestFlagService_GlobalForceOffRecovery(t *testing.T) {
	f := newFlagFixture(t, nil)
	ctx := context.Background()
	farm := func(id uint64) model.Scope { return model.Scope{TenantID: 1, FarmID: id} }
	set := func(t uint64, fm uint64, v string, expect *int) {
		if _, err := f.svc.Set(ctx, flagAdmin, FlagSetInput{Key: model.FlagEnableAdvice, TenantID: t, FarmID: fm, Value: raw(v),
			Comment: "灰度", ExpectRowVer: expect}); err != nil {
			panic(err)
		}
	}
	want := func(step string, w map[uint64]bool) {
		t.Helper()
		for id, on := range w {
			got, err := f.svc.Bool(ctx, model.FlagEnableAdvice, farm(id))
			if err != nil || got != on {
				t.Errorf("%s: farm %d = %t (%v), want %t", step, id, got, err, on)
			}
		}
	}
	want("seed", map[uint64]bool{1: true, 2: true, 3: false})
	set(1, 2, `{"on":false}`, intPtr(0))
	want("farm 2 off", map[uint64]bool{1: true, 2: false, 3: false})
	set(0, 0, `{"force_off":true}`, nil)
	want("global force_off", map[uint64]bool{1: false, 2: false, 3: false})
	set(0, 0, `{"force_off":false}`, nil)
	want("global cleared", map[uint64]bool{1: true, 2: false, 3: false})
	set(1, 0, `{"force_off":true}`, nil)
	want("tenant force_off", map[uint64]bool{1: false, 2: false, 3: false})
	set(1, 0, `{"force_off":false}`, nil)
	want("tenant cleared", map[uint64]bool{1: true, 2: false, 3: false})
	if len(f.store.audits) != 5 {
		t.Errorf("FLAG_CHANGE audits = %d, want 5", len(f.store.audits))
	}
}

// 缓存：30s 内命中；到期重读；本进程 Set 与 flags:changed 消息清缓存；BoolLive 每次直读。
func TestFlagService_Cache(t *testing.T) {
	f := newFlagFixture(t, nil)
	ctx := context.Background()
	s := model.Scope{TenantID: 1, FarmID: 1}
	read := func(step string, want bool, wantReads int) {
		t.Helper()
		got, err := f.svc.Bool(ctx, model.FlagEnableAdvice, s)
		if err != nil || got != want || f.store.readCount() != wantReads {
			t.Errorf("%s: Bool = %t (%v), reads %d; want %t, %d", step, got, err, f.store.readCount(), want, wantReads)
		}
	}
	read("first", true, 1)
	read("cached", true, 1)
	f.store.put(model.FlagEnableAdvice, 1, 1, `{"on":false}`) // 绕过 Set 直接改库：缓存期内仍是旧值
	f.clock.advance(flagCacheTTL - time.Second)
	read("within ttl", true, 1)
	f.clock.advance(time.Second)
	read("expired", false, 2)
	f.store.put(model.FlagEnableAdvice, 1, 1, `{"on":true}`)
	f.svc.onChanged(ctx, []byte(`{"flag_key":"enable_robot_dispatch","change_id":"x"}`))
	read("other key message keeps cache", false, 2)
	f.svc.onChanged(ctx, []byte(`{"flag_key":"enable_advice","change_id":"x"}`))
	read("message clears key", true, 3)
	f.store.put(model.FlagEnableAdvice, 1, 1, `{"on":false}`)
	f.svc.onChanged(ctx, []byte(`not json`))
	read("bad message clears all", false, 4)
	if _, err := f.svc.Set(ctx, flagAdmin, FlagSetInput{Key: model.FlagEnableAdvice, TenantID: 1, FarmID: 1,
		Value: raw(`{"on":true}`), Comment: "恢复", ExpectRowVer: intPtr(0)}); err != nil {
		t.Fatal(err)
	}
	read("set clears", true, 5)
	for i := range 2 {
		if _, err := f.svc.BoolLive(ctx, model.FlagEnableAdvice, s); err != nil {
			t.Fatal(err)
		}
		if f.store.readCount() != 6+i {
			t.Errorf("BoolLive reads = %d, want %d", f.store.readCount(), 6+i)
		}
	}
}

// 读取失败关闭：库错误、未注册 key、形态不符、缺 farm_id 都返回关并报错；错误不进缓存。
func TestFlagService_ReadFailsClosed(t *testing.T) {
	f := newFlagFixture(t, nil)
	ctx := context.Background()
	s := model.Scope{TenantID: 1, FarmID: 1}
	f.store.readErr = errors.New("mysql down")
	if on, err := f.svc.Bool(ctx, model.FlagEnableAdvice, s); on || err == nil {
		t.Errorf("db error: %t, %v", on, err)
	}
	f.store.readErr = nil
	if on, err := f.svc.Bool(ctx, model.FlagEnableAdvice, s); !on || err != nil {
		t.Errorf("after recovery: %t, %v", on, err)
	}
	if _, err := f.svc.Bool(ctx, "enable_magic", s); !errors.Is(err, ErrFlagUnknown) {
		t.Errorf("unknown key err = %v", err)
	}
	if _, err := f.svc.Mode(ctx, model.FlagEnableAdvice, s); !errors.Is(err, ErrFlagKind) {
		t.Errorf("kind mismatch err = %v", err)
	}
	if on, err := f.svc.Bool(ctx, model.FlagEnableAdvice, model.Scope{TenantID: 1}); on || !errors.Is(err, ErrFlagScope) {
		t.Errorf("missing farm: %t, %v", on, err)
	}
}

// DispatchMode：enable_robot_dispatch 开 → REAL，关 / 读失败 → DRY_RUN。
func TestFlagService_DispatchMode(t *testing.T) {
	f := newFlagFixture(t, nil)
	ctx := context.Background()
	s := model.Scope{TenantID: 1, FarmID: 1}
	if m, err := f.svc.DispatchMode(ctx, s); m != model.DispatchDryRun || err != nil {
		t.Errorf("default = %s, %v", m, err)
	}
	f.store.put(model.FlagEnableRobotDispatch, 1, 1, `{"on":true}`)
	if m, err := f.svc.DispatchMode(ctx, s); m != model.DispatchReal || err != nil {
		t.Errorf("on = %s, %v", m, err)
	}
	f.store.put(model.FlagEnableRobotDispatch, 0, 0, `{"force_off":true}`)
	if m, err := f.svc.DispatchMode(ctx, s); m != model.DispatchDryRun || err != nil {
		t.Errorf("global force_off = %s, %v", m, err)
	}
	f.store.readErr = errors.New("mysql down")
	if m, err := f.svc.DispatchMode(ctx, s); m != model.DispatchDryRun || err == nil {
		t.Errorf("read error = %s, %v", m, err)
	}
}

// List 只给 platform_admin，未注册 key 拒绝。
func TestFlagService_List(t *testing.T) {
	f := newFlagFixture(t, nil)
	ctx := context.Background()
	rows, err := f.svc.List(ctx, flagAdmin, model.FlagEnableAdvice)
	if err != nil || len(rows) != 2 {
		t.Errorf("List = %+v, %v", rows, err)
	}
	if _, err := f.svc.List(ctx, model.Scope{TenantID: 1, FarmID: 1, UserID: 1001}, model.FlagEnableAdvice); !errors.Is(err, ErrRoleInsufficient) {
		t.Errorf("non-admin err = %v", err)
	}
	if _, err := f.svc.List(ctx, flagAdmin, "enable_magic"); !errors.Is(err, ErrFlagUnknown) {
		t.Errorf("unknown key err = %v", err)
	}
}

// Run：订阅 flags:changed 并按消息清缓存，ctx 结束退出；订阅成功时先清全部。
func TestFlagService_Run(t *testing.T) {
	sub := &memSubscriber{sub: newMemSub()}
	f := newFlagFixture(t, func(d *FlagDeps) { d.Sub = sub })
	ctx, cancel := context.WithCancel(context.Background())
	s := model.Scope{TenantID: 1, FarmID: 1}
	if _, err := f.svc.Bool(ctx, model.FlagEnableAdvice, s); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc.Run(ctx)
	}()
	f.store.put(model.FlagEnableAdvice, 1, 1, `{"on":false}`)
	sub.sub.ch <- []byte(`{"flag_key":"enable_advice"}`)
	sub.sub.ch <- []byte(`{"flag_key":"enable_robot_dispatch"}`) // 第二条被取走即第一条已处理完
	if on, err := f.svc.Bool(ctx, model.FlagEnableAdvice, s); on || err != nil {
		t.Errorf("after message: %t, %v", on, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit")
	}
	if sub.channel != "as:test:flags:changed" {
		t.Errorf("subscribed %q", sub.channel)
	}
}

func TestNewFlagService_Deps(t *testing.T) {
	if _, err := NewFlagService(FlagDeps{}); err == nil {
		t.Error("want missing deps error")
	}
}
