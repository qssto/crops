package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// 开关读写参数（P1 §6.1）。
const (
	flagCacheTTL        = 30 * time.Second
	flagCommentMaxRunes = 256 // feature_flag.comment varchar(256)
	flagsChangedChannel = "flags:changed"
	flagPublishTimeout  = 2 * time.Second
	flagResubscribe     = 5 * time.Second
)

// 开关错误；取值即接口错误码（controller 映射：UNKNOWN 404，VALUE / SCOPE 400，CONFLICT / PRECONDITION 409）。
var (
	ErrFlagUnknown      = errors.New("FLAG_UNKNOWN")
	ErrFlagValue        = errors.New("FLAG_VALUE_INVALID")
	ErrFlagScope        = errors.New("FLAG_SCOPE_INVALID")
	ErrFlagConflict     = errors.New("FLAG_CONFLICT")
	ErrFlagPrecondition = errors.New("FLAG_PRECONDITION_FAILED")
	// ErrFlagKind 表示按 Bool 读 mode 形态的开关（或反之），属编码错误。
	ErrFlagKind = errors.New("service: flag kind mismatch")
)

// FlagStore 是 feature_flag 读写，repository.FlagRepo 实现；*Tx 方法只在调用方事务内使用。
type FlagStore interface {
	Rows(ctx context.Context, key model.FlagKey, tenantID, farmID uint64) ([]model.FlagRow, error)
	LockTx(ctx context.Context, tx *repository.DB, key model.FlagKey, tenantID, farmID uint64) (repository.FlagCurrent, bool, error)
	UpsertTx(ctx context.Context, tx *repository.DB, w model.FlagWrite) error
	UpdateTx(ctx context.Context, tx *repository.DB, w model.FlagWrite, expectRowVer int) (bool, error)
	List(ctx context.Context, key model.FlagKey) ([]model.FlagSetting, error)
	ScopeExists(ctx context.Context, tenantID, farmID uint64) (bool, error)
}

// AuditTxAppender 在调用方事务内写审计，repository.AuditRepo 实现。
type AuditTxAppender interface {
	AppendTx(ctx context.Context, tx *repository.DB, r model.AuditRecord) error
}

// FlagDeps 是 FlagService 的依赖。FarmRoles 为空时 enable_robot_dispatch 一律不可置开（失败关闭）；
// Registry 为空取 model.DefaultFlagRegistry；Sub 只供 Run。
type FlagDeps struct {
	Store     FlagStore
	Tx        TxRunner
	Audit     AuditTxAppender
	Pub       repository.Publisher
	Sub       EventSubscriber
	FarmRoles FarmRoleChecker
	Keys      rediskey.Keys
	Clock     clock.Clock
	Logger    *slog.Logger
	Registry  model.FlagRegistry
	// Observer 计开关变更 as_flag_change_total（P1 L7-B，可选；为空不计）。
	Observer Observer
}

// FlagService 是开关的统一读写入口（P1 §6.1 流程 2-5）：业务代码只经 Bool / Mode / BoolLive 读取。
type FlagService struct {
	d     FlagDeps
	mu    sync.Mutex
	cache map[flagCacheKey]flagCacheEntry
	gen   uint64 // 每次清缓存 +1；读库前后不一致时不回填，避免并发读把清掉的旧值写回
}

type flagCacheKey struct {
	key              model.FlagKey
	tenantID, farmID uint64
}

type flagCacheEntry struct {
	mode model.FlagMode
	at   time.Time
}

var _ agent.DispatchModeSource = (*FlagService)(nil)

// NewFlagService 校验依赖并构造 FlagService。
func NewFlagService(d FlagDeps) (*FlagService, error) {
	if d.Store == nil || d.Tx == nil || d.Audit == nil || d.Pub == nil || d.Clock == nil || d.Logger == nil {
		return nil, errors.New("service: flag needs store / tx / audit / pub / clock / logger")
	}
	if d.Registry == nil {
		d.Registry = model.DefaultFlagRegistry()
	}
	if d.Observer == nil {
		d.Observer = NopObserver{}
	}
	return &FlagService{d: d, cache: map[flagCacheKey]flagCacheEntry{}}, nil
}

// Bool 读 bool 形态开关在 scope 农场上的生效值（30s 进程内缓存）。
func (s *FlagService) Bool(ctx context.Context, key model.FlagKey, scope model.Scope) (bool, error) {
	m, err := s.read(ctx, key, scope, model.FlagKindBool, true)
	return m == model.FlagModeOn, err
}

// Mode 读 mode 形态开关的生效档位（30s 进程内缓存）；出错时为 off。
func (s *FlagService) Mode(ctx context.Context, key model.FlagKey, scope model.Scope) (model.FlagMode, error) {
	return s.read(ctx, key, scope, model.FlagKindMode, true)
}

// BoolLive 直读 MySQL，不走缓存（enable_robot_dispatch 的 to_order 与下发器第 ② 步）。
func (s *FlagService) BoolLive(ctx context.Context, key model.FlagKey, scope model.Scope) (bool, error) {
	m, err := s.read(ctx, key, scope, model.FlagKindBool, false)
	return m == model.FlagModeOn, err
}

// DispatchMode 实现 agent.DispatchModeSource：BoolLive(enable_robot_dispatch) 开 → REAL，关 → DRY_RUN。
func (s *FlagService) DispatchMode(ctx context.Context, scope model.Scope) (model.DispatchMode, error) {
	on, err := s.BoolLive(ctx, model.FlagEnableRobotDispatch, scope)
	if err != nil {
		return model.DispatchDryRun, err
	}
	if on {
		return model.DispatchReal, nil
	}
	return model.DispatchDryRun, nil
}

// read 取三层合并后的档位；任何错误都返回 off（失败关闭）。
func (s *FlagService) read(ctx context.Context, key model.FlagKey, scope model.Scope, kind model.FlagKind,
	cached bool) (model.FlagMode, error) {
	spec, ok := s.d.Registry[key]
	switch {
	case !ok:
		return model.FlagModeOff, fmt.Errorf("service: flag %q: %w", key, ErrFlagUnknown)
	case spec.Kind != kind:
		return model.FlagModeOff, fmt.Errorf("%w: %s is %s, read as %s", ErrFlagKind, key, spec.Kind, kind)
	case scope.TenantID == 0 || scope.FarmID == 0:
		return model.FlagModeOff, fmt.Errorf("service: flag %s read (%d,%d): %w", key, scope.TenantID, scope.FarmID, ErrFlagScope)
	}
	ck := flagCacheKey{key: key, tenantID: scope.TenantID, farmID: scope.FarmID}
	var gen uint64
	if cached {
		s.mu.Lock()
		e, hit := s.cache[ck]
		gen = s.gen
		s.mu.Unlock()
		if hit && s.d.Clock.Now().Sub(e.at) < flagCacheTTL {
			return e.mode, nil
		}
	}
	rows, err := s.d.Store.Rows(ctx, key, scope.TenantID, scope.FarmID)
	if err != nil {
		return model.FlagModeOff, fmt.Errorf("service: flag %s: %w", key, err)
	}
	mode, err := mergeFlag(spec, rows)
	if err != nil {
		return model.FlagModeOff, fmt.Errorf("service: flag %s (%d,%d): %w", key, scope.TenantID, scope.FarmID, err)
	}
	if cached {
		s.mu.Lock()
		if s.gen == gen {
			s.cache[ck] = flagCacheEntry{mode: mode, at: s.d.Clock.Now()}
		}
		s.mu.Unlock()
	}
	return mode, nil
}

// mergeFlag：生效值 = !force_off(GLOBAL) && !force_off(TENANT) && FARM 值；没有 FARM 行取注册表默认值。
// 库中取值不合法时报错（调用方按关处理）。
func mergeFlag(spec model.FlagSpec, rows []model.FlagRow) (model.FlagMode, error) {
	mode, forced := spec.Default, false
	for _, r := range rows {
		v, err := model.ParseFlagValue(spec, r.ScopeType, r.Value)
		if err != nil {
			return model.FlagModeOff, fmt.Errorf("stored value: %w", err)
		}
		if r.ScopeType == model.FlagScopeFarm {
			mode = v.FarmMode()
		} else if *v.ForceOff {
			forced = true
		}
	}
	if forced {
		return model.FlagModeOff, nil
	}
	return mode, nil
}

// invalidate 清掉 key 的缓存；key 为空清全部。
func (s *FlagService) invalidate(key model.FlagKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	if key == "" {
		clear(s.cache)
		return
	}
	for k := range s.cache {
		if k.key == key {
			delete(s.cache, k)
		}
	}
}
