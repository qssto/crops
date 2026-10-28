// Package app 是依赖装配：cmd/server 与 evalkit 共用同一个 Build，
// 区别只在传入的依赖实现（生产：系统时钟 + agri_steward + as:dev:；评测：FrozenClock + agri_eval_t_{n} + as:eval-t{n}:），
// 保证评测跑的就是生产链路与生产 SQL。
package app

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/controller"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
	"agri-steward/internal/service"
)

// Deps 是装配所需的依赖，全部必填。
type Deps struct {
	Clock    clock.Clock
	Logger   *slog.Logger
	DB       *gorm.DB
	Redis    redis.UniversalClient
	Keys     rediskey.Keys
	RawStore agent.RawStore
	Agent    agent.Agent
	// Tools 是工具调用接缝：生产为 mcp gateway（包装 servers.Build 的四个 Server），评测为每个 trial 一个 FixtureMCP。
	Tools agent.ToolProvider
}

// App 是装配结果。
type App struct {
	Clock      clock.Clock
	Logger     *slog.Logger
	DB         *gorm.DB
	Redis      redis.UniversalClient
	Keys       rediskey.Keys
	RawStore   agent.RawStore
	Agent      agent.Agent
	Tools      agent.ToolProvider
	Locker     *repository.Locker
	WorkOrders *service.WorkOrderService
}

// 缺依赖时返回的错误。
var (
	ErrMissingAgent    = errors.New("app: deps.Agent is required")
	ErrMissingClock    = errors.New("app: deps.Clock is required")
	ErrMissingLogger   = errors.New("app: deps.Logger is required")
	ErrMissingDB       = errors.New("app: deps.DB is required")
	ErrMissingRedis    = errors.New("app: deps.Redis is required")
	ErrMissingKeys     = errors.New("app: deps.Keys is required (rediskey.New)")
	ErrMissingRawStore = errors.New("app: deps.RawStore is required")
	ErrMissingTools    = errors.New("app: deps.Tools is required")
)

// Build 校验依赖并装配 App；缺多个依赖时一次列全。
func Build(deps Deps) (*App, error) {
	var errs []error
	check := func(missing bool, err error) {
		if missing {
			errs = append(errs, err)
		}
	}
	check(deps.Agent == nil, ErrMissingAgent)
	check(deps.Clock == nil, ErrMissingClock)
	check(deps.Logger == nil, ErrMissingLogger)
	check(deps.DB == nil, ErrMissingDB)
	check(deps.Redis == nil, ErrMissingRedis)
	check(deps.Keys.Prefix() == "", ErrMissingKeys)
	check(deps.RawStore == nil, ErrMissingRawStore)
	check(deps.Tools == nil, ErrMissingTools)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &App{
		Clock: deps.Clock, Logger: deps.Logger, DB: deps.DB, Redis: deps.Redis, Keys: deps.Keys,
		RawStore: deps.RawStore, Agent: deps.Agent, Tools: deps.Tools,
		Locker:     repository.NewLocker(deps.Redis),
		WorkOrders: service.NewWorkOrderService(deps.Clock, repository.WorkOrderRepo{}, repository.EventRepo{}),
	}, nil
}

// Handler 返回 HTTP 路由（gin），鉴权由 auth 提供。
func (a *App) Handler(auth *service.AuthService) http.Handler {
	return controller.NewRouter(auth)
}
