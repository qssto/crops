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
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/controller"
	"agri-steward/internal/model"
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
	// Constraints 是开票前约束拦截（P1 L3：constraint.NewReviewer(注册表, policy/regulation_map.yaml)）。
	Constraints service.ConstraintReviewer
	// Adapter 是设备下发接缝（可选）：评测注入 env.RecordingAdapter 记录调用；为空取 adapter.MockAdapter（L8，dry-run）。
	// 只允许 service/workorder_dispatch.go 在作业单 APPROVED 后调用（archtest 守卫）。
	Adapter adapter.DryRunner
	// HITL 是 L6 检查点与记忆的可选依赖（零值：不中断、记忆关闭）。
	HITL HITLDeps
	// DashboardDir 是看板 amis 页面目录（可选，如 web/amis）；为空不挂 /dashboard，文件缺失时 /dashboard 返回 404。
	DashboardDir string
	// Presign 是 L7 原文读取的预签名器（生产：repository.MinIOPresigner，与 RawStore 同一 bucket）；
	// 为空时不挂 GET /runs/:run_id/raw/:raw_id。
	Presign repository.Presigner
	// Permissions 是权限矩阵（policy/permission_matrix.yaml），路由鉴权与审批 / 原文读取的细分判定共用。
	Permissions *service.PermissionMatrix
	// DenialCounter 是拒绝计数接缝（可选；L7 接 Prometheus as_cross_farm_denied_total）。
	DenialCounter service.DenialCounter
	// Devices 是设备 Adapter 注册表配置（P1-L2，configs/devices.yaml；可选：为空即 devicesim 关闭，四类骨架全不可开放）。
	Devices *config.Devices
	// Env 是部署环境（configs/app.yaml 的 env，如 dev / staging / prod）。devicesim 开启时必填：
	// env=prod 或为空即 Build 失败（Devicesim.CheckDeploy），其余场景不读。
	Env string
	// Dispatch 是下发与重试配置（configs/app.yaml 的 dispatch 段，P1-L4-A；零值取 config.DefaultDispatch）。
	Dispatch config.Dispatch
	// ReceiptSecret 是设备回执 webhook 的 HMAC 密钥（P1-L4-B，config.DeviceReceipt.Secret；可选：为空不挂回执路由）。
	ReceiptSecret []byte
	// FieldHistory 是 field.history 回写写端（P1-L4-B，可选：为空取 mock 的 service.MemFieldHistory；
	// mcp-field real 无写接口时传 service.UnwiredFieldHistory 失败关闭为告警）。不对模型可见。
	FieldHistory service.FieldHistoryWriter
	// Faults 是故障降级矩阵（P1 L6，policy.LoadFaultMatrix：policy/fault_matrix.yaml + configs/texts/zh-CN.yaml）；
	// 主链路取拒答 / 降级文案，下发器取设备离线通知文案 key。
	Faults *policy.FaultMatrix
	// NotifyTexts 是 notify.send 文案（P1 L5，service.LoadNotifyTexts(configs/texts/notify.zh-CN.yaml)）；
	// 为空时 server 照常启动，worker / 评测排空构造 outbox 处理器即失败（ErrMissingNotifyTexts）。
	NotifyTexts *service.NotifyTexts
	// Approval 是审批配置（configs/app.yaml 的 approval 段，P1 L5；零值取 config.DefaultApproval）。
	Approval config.Approval
	// L7 是 P1 L7 审计依赖（导出 / 保留期 / 锚点，见 audit_l7.go；零值三项都不启用）。
	L7 AuditL7Deps
	// Metrics / Alerts 是 P1 L7-B 业务指标与告警出口（可选：boot 经 NewObserve 构造后传入，
	// 与 MCP gateway 共用；为空时 buildObserve 补缺省）。
	Metrics *service.MetricsObserver
	Alerts  model.AlertSink
	// AlertsConfig 是告警扫描配置（configs/app.yaml 的 alerts 段；零值取 config.DefaultAlerts）。
	AlertsConfig config.Alerts
	// Quality 是 P2-L3 可信度日报依赖（见 quality.go；零值不起 quality-daily 任务）。
	Quality QualityDeps
	// Compaction 是 P2 L6 长对话压缩（compaction_l6.go，LoadCompaction；零值不装配，行为同 P1）。
	Compaction CompactionDeps
	// RuleOffload 是规则下沉影子比对配置（P2 L4，configs/app.yaml 的 rule_offload 段；零值取 config.DefaultRuleOffload）。
	RuleOffload config.RuleOffload
	// Attribution 是 P2 L8 归因周报与线上回流依赖（attribution_l8.go；零值不起任务）。
	Attribution AttributionDeps
	// EvalReview 是 P2-L2b 专家评审的评测库连接（review_l2b.go；零值：评审接口 503、worker 不入队）。
	EvalReview EvalReviewDeps
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
	Adapter adapter.DryRunner
	// L8：run 事件流（SSE）。outbox 轮询器只在 cmd/worker 里由 NewOutboxPoller 构造（P1-L4-A，server 不再跑）。
	EventStream *service.EventService
	// P1-L4-A：下发与重试配置（Deps.Dispatch，零值取 config.DefaultDispatch；L4-B 回执侧读同一个 MaxAttempts）。
	DispatchConfig config.Dispatch
	// outboxStore 是 outbox 领取 / 续租仓储（NewOutboxPoller 用）。
	outboxStore service.OutboxStore
	// L6：检查点 / 记忆供主链路注入；审批决策入口与扫描任务（cmd/worker 启动 Scanner.Run）。
	HITL *HITL
	// L7 读路径：run 详情与看板指标，只读 Deps.DB（生产为 agri_steward）。
	RunDetail    *service.RunDetailService
	Metrics      *service.MetricsService
	DashboardDir string
	// L7：带授权审计的原文读取（Deps.Presign 为空时 nil）；分区维护（cmd/worker 启动 Partitions.Run）。
	Audit      *service.AuditService
	Partitions *service.PartitionMaintainer
	// P1-L0：开关统一读写入口（cmd/server 与 cmd/worker 都启动 Flags.Run 订阅 flags:changed）。
	Flags *service.FlagService
	// P1-L1-A：权限矩阵与拒绝审计（CROSS_FARM_DENIED）。
	Perms   *service.PermissionMatrix
	Denials *service.DenialAuditor
	// P1-L2：设备 Adapter 注册表（可开放集合决定 to_order 能否写 REAL；L4 下发器按 device_type 取 Adapter）。
	Devices *adapter.Registry
	// P1-L3：本地同地块 REAL 单（重复作业 / 安全间隔期约束），供主链路注入 graph.Config.LocalOrders。
	LocalOrders repository.LocalOrderRepo
	// constraints 即 Deps.Constraints：GraphAgent 由它取约束注册表注入 graph.Config.Constraints（见 constraint.go）。
	constraints service.ConstraintReviewer
	// eventRepo 是唯一的 run_event 写入口（同事务 audit_log 投影 + 提交后 PUBLISH sse:run:{run_id}），
	// 外包 service.ObservedEvents：提交后的事件派生指标（P1 L7-B）。
	eventRepo service.EventStore
	// P1-L4-B：回执 / 中止 / 对账 / field.history 回写；receiptWebhook 表示回执密钥已配置、webhook 挂载。
	// FieldHistory 是实际装配的回写写端（评测读 *service.MemFieldHistory 的记录）。
	OrderOps       *service.OrderOpsService
	FieldHistory   service.FieldHistoryWriter
	receiptWebhook bool
	// P1 L6：故障降级矩阵（Deps.Faults）。
	Faults *policy.FaultMatrix
	// P1 L5：待审批详情、驳回导出与 notify.send 文案（见 approval_l5.go）。
	L5 *ApprovalL5
	// P1 L7：审计导出、保留期清理与每日锚点（见 audit_l7.go）。
	L7 *AuditL7
	// P1 L7-B：告警出口（指标 + ALERT.* 审计 + Error 日志）与指标观测（见 observe.go）。
	Alerts   model.AlertSink
	Observer service.Observer
	// AlertList 是告警列表（GET /alerts，规则 annotation 下钻入口）。
	AlertList *service.AlertService
	metrics   *service.MetricsObserver
	alerts    config.Alerts
	// quality 即 Deps.Quality（P2-L3，buildQualityLoops 读）。
	quality QualityDeps
	// compaction 即 Deps.Compaction：GraphAgent 构造压缩引擎、ask 入口挂会话续接（见 compaction_l6.go）。
	compaction CompactionDeps
	// ruleOffload 即 Deps.RuleOffload（P2 L4 rule_shadow_diff 保留期清理，rule_offload.go）。
	ruleOffload config.RuleOffload
	// L8 是 P2 L8 采纳 / 差评（见 feedback_l8.go）。
	L8 *L8
	// attribution 即 Deps.Attribution（P2 L8 worker 任务，attribution_l8.go）。
	attribution AttributionDeps
	// Review 是 P2-L2b 专家评审（评测库关闭时为 DisabledReviewService）；evalReview 即 Deps.EvalReview。
	Review     *service.ReviewService
	evalReview EvalReviewDeps
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
	// ErrMissingConstraints：P1 L3 起开票前必须经约束拦截。
	ErrMissingConstraints = errors.New("app: deps.Constraints is required (constraint.NewReviewer)")
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
	check(deps.Constraints == nil, ErrMissingConstraints)
	check(deps.Permissions == nil, ErrMissingPermissions)
	check(deps.Faults == nil, ErrMissingFaults)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if deps.Adapter == nil {
		deps.Adapter = adapter.MockAdapter{}
	}
	if err := buildObserve(&deps); err != nil {
		return nil, err
	}
	locker := repository.NewLocker(deps.Redis)
	runs := repository.NewRunRepo(deps.DB, deps.Clock)
	advice := repository.NewAdviceRepo(deps.DB, deps.Clock)
	orders := repository.WorkOrderRepo{}
	rawEvents := repository.EventRepo{Pub: repository.RedisPublisher{Client: deps.Redis}, Keys: deps.Keys, Log: deps.Logger}
	events := service.ObservedEvents{Next: rawEvents, Obs: deps.Metrics}
	workOrders := service.NewWorkOrderService(deps.Clock, orders, events)
	approvals, err := service.NewApprovalService(service.ApprovalDeps{Clock: deps.Clock, DB: deps.DB,
		Tx: repository.Transactor{DB: deps.DB}, Tickets: repository.ApprovalRepo{}, Orders: orders,
		WorkOrders: workOrders, Events: events, Raw: deps.RawStore, Risk: deps.Risk,
		Constraints: deps.Constraints, History: repository.LocalOrderRepo{DB: deps.DB},
		Notify: repository.NotifyOutboxRepo{}, Logger: deps.Logger})
	if err != nil {
		return nil, fmt.Errorf("app: build approvals: %w", err)
	}
	a := &App{
		Clock: deps.Clock, Logger: deps.Logger, DB: deps.DB, Redis: deps.Redis, Keys: deps.Keys,
		RawStore: deps.RawStore, Agent: deps.Agent, Tools: deps.Tools,
		Locker:      locker,
		WorkOrders:  workOrders,
		Runs:        runs,
		Advice:      advice,
		Drafts:      repository.NewOrderDraftRepo(deps.DB, deps.Clock),
		LocalOrders: repository.LocalOrderRepo{DB: deps.DB},
		RunLocker:   repository.NewRunLocker(locker, deps.Keys),
		Approvals:   approvals,
		Events:      service.ObservedEventSink{Next: repository.EventSink{DB: deps.DB, Events: rawEvents}, Obs: deps.Metrics},
		Adapter:     deps.Adapter,
		HITL:        buildHITLStores(deps),
		RunDetail:   service.NewRunDetailService(repository.NewRunDetailRepo(deps.DB, runs, advice)),
		Metrics:     service.NewMetricsService(repository.NewMetricsRepo(deps.DB), deps.Clock),
		eventRepo:   events,

		DashboardDir: deps.DashboardDir,
		Alerts:       deps.Alerts,
		Observer:     deps.Metrics,
		metrics:      deps.Metrics,
		alerts:       deps.AlertsConfig,
		quality:      deps.Quality,
		ruleOffload:  deps.RuleOffload, // P2 L4
		attribution:  deps.Attribution, // P2 L8
		evalReview:   deps.EvalReview,  // P2-L2b
	}
	a.constraints = deps.Constraints
	a.Faults = deps.Faults
	a.compaction = deps.Compaction
	if err := a.buildAlertList(); err != nil {
		return nil, err
	}
	if err := a.buildAccess(deps); err != nil {
		return nil, err
	}
	if err := a.buildFlags(); err != nil {
		return nil, err
	}
	if err := a.buildDevices(deps.Env, deps.Devices); err != nil {
		return nil, err
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
	askDeps := service.AskDeps{
		Fields:  repository.FieldScope{DB: deps.DB},
		Limiter: repository.NewAskLimiter(deps.Redis, deps.Keys, AskPerMinute),
		Runs:    runs, Advice: advice, Agent: a.Agent, Meta: meta, Logger: deps.Logger, Flags: a.Flags,
	}
	a.withSessions(&askDeps) // P2 L6
	a.Ask = service.NewAskService(askDeps)
	if err := a.buildAuditL7(deps); err != nil {
		return nil, err
	}
	if err := a.buildHITLServices(); err != nil {
		return nil, err
	}
	if err := a.buildDispatch(deps.Dispatch); err != nil {
		return nil, err
	}
	if err := a.buildAudit(deps); err != nil {
		return nil, err
	}
	if err := a.buildOrderOps(deps); err != nil {
		return nil, err
	}
	if err := a.buildApprovalL5(deps); err != nil {
		return nil, err
	}
	if err := a.buildL8(); err != nil { // P2 L8
		return nil, err
	}
	if err := a.buildReview(); err != nil { // P2-L2b
		return nil, err
	}
	return a, nil
}

// buildAudit 装配 L7 原文读取（有 Presign 时）与分区维护。
func (a *App) buildAudit(deps Deps) error {
	if deps.Presign != nil {
		svc, err := service.NewAuditService(service.AuditDeps{Store: repository.RawAccessRepo{DB: a.DB},
			Presign: deps.Presign, Clock: a.Clock, Logger: a.Logger, Perms: a.Perms})
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

// Handler 返回 HTTP 路由（gin），鉴权由 auth（a.NewAuth）提供、权限按 a.Perms 判定；DashboardDir 非空时挂 /dashboard。
func (a *App) Handler(auth *service.AuthService) http.Handler {
	mounts := []controller.Mount{controller.ApprovalRoutes(a.HITL.Decisions), controller.RunRoutes(a.RunDetail, a.Metrics),
		controller.EventRoutes(a.EventStream)}
	if a.Audit != nil {
		mounts = append(mounts, controller.AuditRoutes(a.Audit))
	}
	mounts = append(mounts, a.orderOpsMounts()...)
	mounts = append(mounts, a.approvalL5Mounts()...)
	mounts = append(mounts, a.auditL7Mounts()...)
	mounts = append(mounts, a.l8Mounts()...) // P2 L8
	if a.AlertList != nil {
		mounts = append(mounts, controller.AlertRoutes(a.AlertList)) // P1 L7-B
	}
	g := a.guard(auth)
	r := controller.NewRouter(g, a.Ask, mounts...)
	controller.MountAdmin(r, g, a.Flags)   // P1-L0：/api/v1/admin/flags（flag.write）
	for _, m := range a.l8AdminMounts(g) { // P2 L8：/api/v1/admin/attribution-issues/:id（attribution.write）
		m(r)
	}
	for _, m := range a.reviewAdminMounts(g) { // P2-L2b：/api/v1/admin/review-tasks（review.read / review.submit）
		m(r)
	}
	// P1-L4：设备回执 webhook（签名校验，不经用户鉴权）
	for _, m := range a.orderOpsEngineMounts() {
		m(r)
	}
	if a.DashboardDir != "" {
		controller.MountDashboard(r, a.DashboardDir)
	}
	return r
}

// buildDispatch 装配下发器（WorkOrderService.Dispatch：L8 dry-run + P1-L4-A 真实通道 / 设备绑定 / 复检退回）与 SSE 事件流。
// outbox 轮询器在 cmd/worker 启动时由 NewOutboxPoller 按合并后的处理器表构造（见 worker.go）。
// 主链路不支持复检（a.HITL.Recheck 为空）时 DRY_RUN 单跳过 ④ 并告警，REAL 单失败关闭。
func (a *App) buildDispatch(cfg config.Dispatch) error {
	if cfg == (config.Dispatch{}) {
		cfg = config.DefaultDispatch()
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("app: build dispatch: %w", err)
	}
	a.DispatchConfig = cfg
	reopener, err := service.NewReopenService(a.Approvals, repository.ReopenRepo{DB: a.DB})
	if err != nil {
		return fmt.Errorf("app: build dispatch: %w", err)
	}
	store := repository.NewDispatchRepo(a.DB)
	a.outboxStore = store.OutboxRepo
	d := service.DispatchDeps{Adapter: a.Adapter, Store: store, Tx: a.HITL.decisions, Cards: a.Advice, Epochs: a.Runs,
		Locks: a.Locker, Keys: a.Keys, Logger: a.Logger, Flags: a.Flags, Audit: repository.AuditRepo{DB: a.DB},
		Devices: a.Devices, Attempts: repository.DispatchAttemptRepo{DB: a.DB}, Idem: repository.NewRedisIdem(a.Redis),
		Picker: service.ToolDevicePicker{Tools: a.Tools}, Reopener: reopener,
		MaxAttempts: cfg.MaxAttempts, ReceiptTimeout: cfg.ReceiptTimeout, DeviceWaitMax: cfg.DeviceWaitMax,
		Notify: repository.NotifyOutboxRepo{}, OfflineTextKey: offlineTextKey(a.Faults),
		Tickets: repository.ReopenRepo{DB: a.DB}, Alerts: a.Alerts, Observer: a.Observer}
	if a.HITL.Recheck != nil {
		d.Recheck = a.HITL.Recheck
	}
	if err := a.WorkOrders.SetDispatch(d); err != nil {
		return fmt.Errorf("app: build dispatch: %w", err)
	}
	a.EventStream = service.NewEventService(service.EventDeps{Subscriber: repository.RedisSubscriber{Client: a.Redis},
		Backfill: repository.EventBackfillRepo{DB: a.DB}, Runs: a.Runs, Keys: a.Keys, Logger: a.Logger})
	return nil
}
