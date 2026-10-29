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
	// Adapter 是设备下发接缝（可选）：评测注入 env.RecordingAdapter 记录调用；为空取 adapter.MockAdapter（L8，dry-run）。
	// 只允许 service/workorder_dispatch.go 在作业单 APPROVED 后调用（archtest 守卫）。
	Adapter adapter.DeviceAdapter
	// HITL 是 L6 检查点与记忆的可选依赖（零值：不中断、记忆关闭）。
	HITL HITLDeps
	// DashboardDir 是看板 amis 页面目录（可选，如 web/amis）；为空不挂 /dashboard，文件缺失时 /dashboard 返回 404。
	DashboardDir string
	// Presign 是 L7 原文读取的预签名器（生产：repository.MinIOPresigner，与 RawStore 同一 bucket）；
	// 为空时不挂 GET /runs/:run_id/raw/:raw_id。
	Presign repository.Presigner
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
	// Adapter 即 Deps.Adapter，为空时是 adapter.MockAdapter。
	Adapter adapter.DeviceAdapter
	// L8：outbox 轮询器（cmd/server 启动 Outbox.Run；评测不跑，见 dev-log L8-A）与 run 事件流（SSE）。
	Outbox      *service.OutboxPoller
	EventStream *service.EventService
	// L6：检查点 / 记忆供主链路注入；审批决策入口与扫描任务（cmd/server 启动 Scanner.Run）。
	HITL *HITL
	// L7 读路径：run 详情与看板指标，只读 Deps.DB（生产为 agri_steward）。
	RunDetail    *service.RunDetailService
	Metrics      *service.MetricsService
	DashboardDir string
	// L7：带授权审计的原文读取（Deps.Presign 为空时 nil）；分区维护（cmd/server 启动 Partitions.Run）。
	Audit      *service.AuditService
	Partitions *service.PartitionMaintainer
	// eventRepo 是唯一的 run_event 写入口（同事务 audit_log 投影 + 提交后 PUBLISH sse:run:{run_id}）。
	eventRepo repository.EventRepo
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
	if deps.Adapter == nil {
		deps.Adapter = adapter.MockAdapter{}
	}
	locker := repository.NewLocker(deps.Redis)
	runs := repository.NewRunRepo(deps.DB, deps.Clock)
	advice := repository.NewAdviceRepo(deps.DB, deps.Clock)
	orders := repository.WorkOrderRepo{}
	events := repository.EventRepo{Pub: repository.RedisPublisher{Client: deps.Redis}, Keys: deps.Keys, Log: deps.Logger}
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
		RunDetail:  service.NewRunDetailService(repository.NewRunDetailRepo(deps.DB, runs, advice)),
		Metrics:    service.NewMetricsService(repository.NewMetricsRepo(deps.DB), deps.Clock),
		eventRepo:  events,

		DashboardDir: deps.DashboardDir,
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
	if err := a.buildDispatch(); err != nil {
		return nil, err
	}
	if err := a.buildAudit(deps); err != nil {
		return nil, err
	}
	return a, nil
}

// buildAudit 装配 L7 原文读取（有 Presign 时）与分区维护。
func (a *App) buildAudit(deps Deps) error {
	if deps.Presign != nil {
		svc, err := service.NewAuditService(service.AuditDeps{Store: repository.RawAccessRepo{DB: a.DB},
			Presign: deps.Presign, Clock: a.Clock, Logger: a.Logger})
		if err != nil {
			return fmt.Errorf("app: build audit: %w", err)
		}
		a.Audit = svc
	}
	pm, err := service.NewPartitionMaintainer(service.PartitionDeps{Store: repository.PartitionRepo{DB: a.DB},
		Locks: a.Locker, Keys: a.Keys, Clock: a.Clock, Logger: a.Logger})
	if err != nil {
		return fmt.Errorf("app: build partition maintainer: %w", err)
	}
	a.Partitions = pm
	return nil
}

// Handler 返回 HTTP 路由（gin），鉴权由 auth 提供；DashboardDir 非空时挂 /dashboard。
func (a *App) Handler(auth *service.AuthService) http.Handler {
	mounts := []controller.Mount{controller.ApprovalRoutes(a.HITL.Decisions), controller.RunRoutes(a.RunDetail, a.Metrics),
		controller.EventRoutes(a.EventStream)}
	if a.Audit != nil {
		mounts = append(mounts, controller.AuditRoutes(a.Audit))
	}
	r := controller.NewRouter(auth, a.Ask, mounts...)
	if a.DashboardDir != "" {
		controller.MountDashboard(r, a.DashboardDir)
	}
	return r
}

// buildDispatch 装配 L8 下发器（WorkOrderService.Dispatch）、outbox 轮询器与 SSE 事件流。
// 主链路不支持复检（a.HITL.Recheck 为空）时下发跳过 ④ 并告警。
func (a *App) buildDispatch() error {
	store := repository.NewDispatchRepo(a.DB)
	d := service.DispatchDeps{Adapter: a.Adapter, Store: store, Tx: a.HITL.decisions, Cards: a.Advice, Epochs: a.Runs,
		Locks: a.Locker, Keys: a.Keys, Logger: a.Logger}
	if a.HITL.Recheck != nil {
		d.Recheck = a.HITL.Recheck
	}
	if err := a.WorkOrders.SetDispatch(d); err != nil {
		return fmt.Errorf("app: build dispatch: %w", err)
	}
	a.Outbox = service.NewOutboxPoller(service.OutboxDeps{Store: store.OutboxRepo,
		Handlers: map[string]service.OutboxHandler{repository.TopicOrderDispatch: a.WorkOrders.Dispatch},
		Clock:    a.Clock, Logger: a.Logger})
	a.EventStream = service.NewEventService(service.EventDeps{Subscriber: repository.RedisSubscriber{Client: a.Redis},
		Backfill: repository.EventBackfillRepo{DB: a.DB}, Runs: a.Runs, Keys: a.Keys, Logger: a.Logger})
	return nil
}
