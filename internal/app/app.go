// Package app 是依赖装配：cmd/server 与 evalkit 共用同一个 Build，
// 区别只在传入的依赖实现（生产：系统时钟 + agri_steward + as:dev:；评测：FrozenClock + agri_eval_t_{n} + as:eval-t{n}:），
// 保证评测跑的就是生产链路与生产 SQL。
package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"agri-steward/internal/adapter"
	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/controller"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
	"agri-steward/internal/service"
)

// Deps 是装配所需的依赖，全部必填（Agent 与 NewAgent 二选一）。
type Deps struct {
	Clock    clock.Clock
	Logger   *slog.Logger
	DB       *gorm.DB
	Redis    redis.UniversalClient
	Keys     rediskey.Keys
	RawStore agent.RawStore
	Agent    agent.Agent
	// NewAgent 在仓储（Runs / Advice / Drafts / RunLocker）装配好之后构造主链路；设置时忽略 Agent。
	// 主链路依赖这些仓储，而仓储由 Build 创建，故以工厂注入（app 不依赖 graph 实现）。
	NewAgent func(*App) (agent.Agent, error)
	// Tools 是工具调用接缝：生产为 mcp gateway（包装 servers.Build 的四个 Server），评测为每个 trial 一个 FixtureMCP。
	Tools agent.ToolProvider
	// RunMeta 是 ask 入口建 agent_run 时写入的版本信息（可选；为空且主链路实现 Meta() 时取主链路的）。
	RunMeta agent.RunMeta
	// Risk 是审批风险矩阵（policy/risk_matrix.yaml，policy.LoadRiskMatrix），开票定级与票据 TTL 用。
	Risk service.RiskPolicy
	// Adapter 是设备下发接缝（可选）：评测注入 env.RecordingAdapter 记录调用；生产在 L8 接 MockAdapter，
	// 只允许 service/workorder 在作业单 APPROVED 后调用。
	Adapter adapter.DeviceAdapter
	// HITL 是 L6 检查点与记忆的可选依赖（零值：不中断、记忆关闭）。
	HITL HITLDeps
}

// metaer 是能报告自身版本信息的主链路（graph.Agent）。
type metaer interface{ Meta() agent.RunMeta }

// AskPerMinute 是单农场每分钟提问上限（rl:farm:{farm_id}:ask）。
const AskPerMinute = 20

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
	// L4 存储与入口：Runs / Advice / Drafts / RunLocker 供主链路注入，Ask 供 HTTP 入口。
	Runs      *repository.RunRepo
	Advice    *repository.AdviceRepo
	Drafts    *repository.OrderDraftRepo
	RunLocker *repository.RunLocker
	Ask       *service.AskService
	// L5：审批闸门（主链路 Gate）与待审批列表；Events 是图内事件（VALIDATION_FAILED）出口。
	Approvals *service.ApprovalService
	Events    agent.EventSink
	// Adapter 即 Deps.Adapter（可为 nil）。
	Adapter adapter.DeviceAdapter
	// L6：检查点 / 记忆供主链路注入；审批决策入口与扫描任务（cmd/server 启动 Scanner.Run）。
	HITL *HITL
}

// 缺依赖时返回的错误。
var (
	ErrMissingAgent    = errors.New("app: deps.Agent or deps.NewAgent is required")
	ErrMissingClock    = errors.New("app: deps.Clock is required")
	ErrMissingLogger   = errors.New("app: deps.Logger is required")
	ErrMissingDB       = errors.New("app: deps.DB is required")
	ErrMissingRedis    = errors.New("app: deps.Redis is required")
	ErrMissingKeys     = errors.New("app: deps.Keys is required (rediskey.New)")
	ErrMissingRawStore = errors.New("app: deps.RawStore is required")
	ErrMissingTools    = errors.New("app: deps.Tools is required")
	ErrMissingRisk     = errors.New("app: deps.Risk is required (policy.LoadRiskMatrix)")
)

// Build 校验依赖并装配 App；缺多个依赖时一次列全。
func Build(deps Deps) (*App, error) {
	var errs []error
	check := func(missing bool, err error) {
		if missing {
			errs = append(errs, err)
		}
	}
	check(deps.Agent == nil && deps.NewAgent == nil, ErrMissingAgent)
	check(deps.Clock == nil, ErrMissingClock)
	check(deps.Logger == nil, ErrMissingLogger)
	check(deps.DB == nil, ErrMissingDB)
	check(deps.Redis == nil, ErrMissingRedis)
	check(deps.Keys.Prefix() == "", ErrMissingKeys)
	check(deps.RawStore == nil, ErrMissingRawStore)
	check(deps.Tools == nil, ErrMissingTools)
	check(deps.Risk == nil, ErrMissingRisk)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	locker := repository.NewLocker(deps.Redis)
	runs := repository.NewRunRepo(deps.DB, deps.Clock)
	advice := repository.NewAdviceRepo(deps.DB, deps.Clock)
	orders, events := repository.WorkOrderRepo{}, repository.EventRepo{}
	workOrders := service.NewWorkOrderService(deps.Clock, orders, events)
	approvals, err := service.NewApprovalService(service.ApprovalDeps{Clock: deps.Clock, DB: deps.DB,
		Tx: repository.Transactor{DB: deps.DB}, Tickets: repository.ApprovalRepo{}, Orders: orders,
		WorkOrders: workOrders, Events: events, Raw: deps.RawStore, Risk: deps.Risk})
	if err != nil {
		return nil, fmt.Errorf("app: build approvals: %w", err)
	}
	a := &App{
		Clock: deps.Clock, Logger: deps.Logger, DB: deps.DB, Redis: deps.Redis, Keys: deps.Keys,
		RawStore: deps.RawStore, Agent: deps.Agent, Tools: deps.Tools,
		Locker:     locker,
		WorkOrders: workOrders,
		Runs:       runs,
		Advice:     advice,
		Drafts:     repository.NewOrderDraftRepo(deps.DB, deps.Clock),
		RunLocker:  repository.NewRunLocker(locker, deps.Keys),
		Approvals:  approvals,
		Events:     repository.EventSink{DB: deps.DB, Events: events},
		Adapter:    deps.Adapter,
		HITL:       buildHITLStores(deps),
	}
	if deps.NewAgent != nil {
		ag, err := deps.NewAgent(a)
		if err != nil {
			return nil, fmt.Errorf("app: build agent: %w", err)
		}
		a.Agent = ag
	}
	meta := deps.RunMeta
	if m, ok := a.Agent.(metaer); ok && meta == (agent.RunMeta{}) {
		meta = m.Meta()
	}
	a.Ask = service.NewAskService(service.AskDeps{
		Fields:  repository.FieldScope{DB: deps.DB},
		Limiter: repository.NewAskLimiter(deps.Redis, deps.Keys, AskPerMinute),
		Runs:    runs, Advice: advice, Agent: a.Agent, Meta: meta, Logger: deps.Logger,
	})
	a.buildHITLServices()
	return a, nil
}

// Handler 返回 HTTP 路由（gin），鉴权由 auth 提供。
func (a *App) Handler(auth *service.AuthService) http.Handler {
	return controller.NewRouter(auth, a.Ask, controller.ApprovalRoutes(a.HITL.Decisions))
}
