# 智能农作物 Agent 管家 Phase 0（P0）MVP 闭环可用期 实施路线（6 周）

> 文档类型：阶段详细实施路线（执行手册）
> 上游文档：《智能农作物 Agent 管家 分阶段实施路线图》§5.0（同时引用 §4.2 七件套、§4.4 红线落地、§4.5 统一口径、§4.6 工程结构、§4.7 评测 Harness、§9 首批任务）；《评测 Harness 设计方案》§4.1 注入接缝、§5 样本格式、§7 打分器、§15 分阶段落地
> 下游文档：《Phase 1（P1）生产可用期 实施路线》
> 读者：项目 Owner / 架构负责人、Agent Harness RD、数据与物联网 RD、知识工程 RD、前端、QA、SRE、农艺专家
> 状态：草案，Phase 0 开工前评审

---

## 1. 文档定位

1. 本文把母路线图 §5.0 展开为团队可直接排期的执行手册：每个 route（`L0 → L8`）补齐涉及模块、功能任务、交付物、验收，并给出第 1-6 周的周排期与合入规则。
2. 本文只覆盖 Phase 0，不重复母文档的立项论证（为什么做、决策层拍板项），只引用其结论。
3. 与母文档冲突时以母文档为准，并回改本文；本文新增的拆细任务若需要改动母文档的契约、门禁或范围，必须由 Owner 批准后同步回母文档。
4. 代码提交、PR 标题、评测样本 `tags` 均对齐本文 `Ln` 编号（母文档 §1-3、§8.3-2）。

## 2. 阶段基线与输入

### 2.1 上一阶段产出

Phase 0 是首个阶段，无上一阶段产出。可复用的只有极飞侧存量资产（母文档 §2.1）：设备矩阵、巡田与物联网数据入口、`XGEO` 地块数据、`极飞学园` 农艺内容。设备是否具备可编程下发接口尚未确认，本阶段不依赖。

### 2.2 本阶段依赖的外部决策

| 编号 | 待确认项 | 本阶段最晚闭环 | 未闭环时的处理 |
|------|----------|----------------|----------------|
| D1 | 底座模型形态与档位 | 第 3 周 L4 开工前（法务正式结论可延到 Phase 1 开工前） | L4 无法开工；期间按「脱敏后入模」最严口径开发 |
| D2 | 首个作物 + 区域 + 示范农场 | 第 1 周 | L0 场景冻结、L3 知识范围、评测集全部无法收敛 |
| D3 | 农艺知识权威来源与署名专家 | 第 1 周 | 知识条目与评测样本无人签署，红线一无地基 |
| D6 | 气象数据源与授权范围 | 第 2 周 | `mcp-weather` 只能走 mock，Gate 0 无法用真实问题演示 |
| D7 | Agent 框架与语言栈 | 已定：Eino（Go） | — |

D4（设备下发接口）、D5（作业事故责任与保险）不阻塞 Phase 0，但须在 L8 收口时形成结论写入《Phase 1 启动包》。

### 2.3 本阶段沿用的口径

1. 契约：`AdviceCard` / `WorkOrder` / `Citation` / `RunState` / `ApprovalTicket`，定义见母文档 §4.5 与 L1，`tenant_id / farm_id` 从本阶段起贯穿全部契约（单值）。
2. 状态机：`DRAFT → PENDING_APPROVAL → APPROVED → DISPATCHED → EXECUTING → DONE`，异常分支 `REJECTED / EXPIRED / FAILED / CANCELED / PARTIALLY_DONE`，含 `APPROVED → PENDING_APPROVAL` 复审迁移；Phase 1 才用到的状态本阶段一并定义。
3. 开关：`enable_*` 开关框架本阶段落地，默认全关；本阶段实际使用的只有 `enable_advice` 与记忆注入开关，`enable_robot_dispatch` 固定为关（只 dry-run）。
4. 指标：母文档 §3.3 工程与安全类指标本阶段埋点；可信度类指标只出 baseline，不设达标线。
5. 工程约定：表设计、Redis 键（本文键表省略 `as:{env}:` 前缀）、锁与幂等占位按母文档 §4.6.1；鉴权、下发路径、outbox 去重键、审批票据、事件类型、对象 key、`WorkOrder` 迁移表按母文档 §4.6.2，本文只写 P0 的落地方式。§6 各 Ln 的 SQL 是 P0 定版表结构，P1–P4 的列名以此为准。

## 3. 范围边界

### 3.1 本阶段必须完成

1. Agent Harness 七件套最小可用版：母文档 §4.2 表「Phase 0 最小实现」列全部落地（长对话压缩只在 `RunState` 预留 `compaction_state` 字段）。
2. 4 个 MCP Server（`mcp-weather` / `mcp-crop` / `mcp-field` / `mcp-device`）经 `mcp-gateway` 接入，每个工具 `mock` 与 `real` 双实现、接口一致；评测另有 `FixtureMCP`。
3. 固定文件知识库 + `progressive_loader`，覆盖首个作物「施肥 / 植保 / 灌溉 / 灾害防备」四类问题；`calc` 规则层承担全部数值与药剂候选集合。
4. 五个核心契约冻结，评测注入接缝（`Clock`、`ToolProvider`、`DeviceAdapter`、存储、`KnowledgeSource`、`MemoryStore`、`UserChannel`、`Approver`）全部以接口注入，`cmd/server` 与 `evalkit` 共用 `app.Build`。
5. 100 条评测样本（应拒答 ≥ 20 条）+ 专家签署的标准答案；`evalkit` 跑出 Phase 0 baseline。
6. 最小内部 Web 页：对话区、待审批区、run 详情区，dry-run 执行结果可视化。

### 3.2 本阶段明确不做

1. 不做 RAG、不做向量检索。
2. 不做真实设备下发（只 dry-run），不做设备故障处理与作业补偿；`constraint_checker` 的硬约束拦截（审批前禁止提交、下发前复检）留 Phase 1；本阶段 `WorkOrder.constraint_checks[]` 字段就绪，审批页按母文档 L8 展示校验结论，仅展示、不拦截。
3. 不做多模态图像诊断、主动预警推送、多机协同排程。
4. 不做多租户与精细权限（单农场；身份与角色只用静态配置，见 L1，权限矩阵与 `user_role` 留 Phase 1），不做多作物 / 多区域扩展。
5. 不做模型微调与自建推理集群。
6. 不做长对话压缩。
7. 不追求建议准确率达标；不做 LLM-judge 门禁、LLM 模拟用户、`redteam` / `holdout` 套件、CI 阻断（均为 Phase 1-2）。

## 4. 目标与通过标准（Gate 0）

| # | 通过标准（母文档 §5.0.2） | 判定方式 | 证据 |
|---|---------------------------|----------|------|
| 1 | 演示 20 个真实问题，100% 产出带 `Citation` 的建议或明确拒答，无一条无依据结论 | `evalkit` 自动：`citation_valid`、`refusal_correct`、`kb_miss_refuse`，红线按 pass^3 | 评测报告 + 各 trial `run_id` |
| 2 | 涉及设备动作的请求 100% 被护栏拦下并转审批，无一次绕过 | `evalkit` 自动：`no_model_actuate`、`stop_at_approval`，pass^3 | 评测报告 + `RecordingAdapter` 调用记录 |
| 3 | 审批点中断后 `resume(run_id)` 恢复成功率 ≥ 95%，且不重复执行已完成步骤 | 集成测试 + L6 恢复用例（工具调用计数断言） | 测试报告 |
| 4 | 每个 run 可在审计日志与 trace 中完整还原 | 人工抽查（L7 验收 1、2、3），Owner 签字 | 抽查记录 |
| 5 | 脱敏用例全通过：日志与模型输入中无手机号、身份证、精确坐标明文 | `evalkit` 自动：`pii_clean`，pass^3 | 评测报告 |
| 6 | 100 条评测集跑出 baseline 数据（不设达标线） | `evalkit` 自动：n=3 全量，评测负责人签字 | baseline 报告 + 配置快照 |
| 7 | 首个示范农场至少完成 1 次「建议 → 审批 → dry-run」真人参与的端到端演练，并留下书面反馈 | 由田间验证方执行，本团队提供功能与数据支撑 | 演练反馈文档 |

## 5. 实现路线总览（L0 → L8）

1. `L0`：场景冻结与评测底座
2. `L1`：核心契约与数据模型冻结
3. `L2`：MCP 数据接入层
4. `L3`：固定知识库与渐进式加载
5. `L4`：Agent 主链路与建议生成
6. `L5`：护栏层（审批 / 脱敏 / 输出校验）
7. `L6`：HITL 中断恢复 + 记忆
8. `L7`：审计日志与可观测链路
9. `L8`：最小交互界面 + 示范农场冒烟 + Gate 0 收口

建议顺序：`L0 → L1 → L2 + L3（并行）→ L4 → L5 + L6（并行）→ L7 → L8`

```
            ┌──────── L2 MCP 接入 ────────┐
L0 ──► L1 ──┤                             ├──► L4 主链路 ──┬──► L5 护栏 ────┬──► L7 审计/trace ──► L8 收口
场景/评测  契约 └──── L3 知识库 + calc ─────┘                 └──► L6 HITL/记忆 ┘
  │                                                                              ▲
  └──────────── evalkit 随层交付：样本 → 接缝 → FixtureMCP → 打分器 → n 次运行 ──┘
```

依赖说明：

1. L1 冻结是并行开发的前置条件，L2 / L3 只依赖 L1 契约，可两组并行。
2. L4 依赖 L2 的工具 schema 与 L3 的 `KB_MISS`、`product_candidates`、`calc` 接口；L4 开工还依赖 D1。
3. L5 / L6 都挂在 L4 的 `WorkOrder(DRAFT)` 停点上，L5 管「停下」，L6 管「接着跑」，两者以 `ApprovalTicket` + `RunState` 为接口并行。
4. L7 的 `callbacks` 骨架在第 1-2 周最小链路中已先落地，L7 阶段是补全事件类型、`raw_store`、看板，不是从零开始。

## 6. 路线详细拆解

### 6.1 L0 场景冻结与评测底座

**目标**：把「智能管家」收敛成一个可验收的具体场景，同步建立唯一的效果对照基线。

**涉及模块**：《场景说明书》、`question_taxonomy`（随 `eval/` 入库）、`eval/suites/regression/`、`eval/refs/`、`internal/evalkit/sample`、`internal/evalkit/report`、`cmd/eval`、`Makefile`（`make eval`）。

**功能任务**

1. 冻结 D2：作物、区域、示范农场、目标用户角色（农场主 / 农艺师 / 机手），写成一页《场景说明书》。
2. 冻结问题域：四类问题各挑高频子类，形成 `question_taxonomy`（如植保类 → 病虫害识别 / 防治时机 / 药剂选择 / 用量计算），作为 L3 加载与 L4 分类节点的共同枚举。
3. 定样本 YAML 格式（评测设计方案 §5），专家按格式写样本：先 20 条样例（第 1-2 周），再扩到 100 条（第 5 周前），其中应拒答负样本 ≥ 20 条（知识未覆盖、数据缺失、超出作物范围）。
4. 农艺专家逐条签署标准答案（`author`、`signed_at` 必填），形成可追责的标注记录。
5. 搭 `evalkit` 骨架：`internal/evalkit/sample` 加载与静态校验（必填项、套件配比）、`cmd/eval validate`、单样本多次运行的调度（`--n`，默认 3）、`report` 生成 Markdown 报告。
6. 准备一个「空实现」agent 桩（只返回拒答），用于在主链路就绪前验证 `make eval` 回放与报告链路。
7. Eino 检查点验证 spike（第 1 周）：用实际 Eino 版本确认 `CheckPointStore` 只在中断时写入，结论写入 L6 设计（母文档 §4.6.2-12）。

**交付物**：《场景说明书》、`question_taxonomy`、样本 YAML 格式说明、20 → 100 条样本、`cmd/eval` 骨架、空实现报告一份。

**验收**

1. 《场景说明书》与 `question_taxonomy` 经业务与农艺双方书面确认。
2. 100 条评测集入库，应拒答样本 ≥ 20 条，全部有专家署名，全部通过 `cmd/eval validate` 静态校验。
3. `make eval` 可一键加载并回放评测集，对空实现生成报告（不要求分数）。

**技术实现**

① 流程

1. `make eval` → `cmd/eval run --suite regression` → `evalkit/sample.Load`：解析 `eval/suites/**.yaml`，静态校验必填项、`author / signed_at`、套件配比（应拒答 ≥ 20%），失败即退出并列出样本 ID。
2. `evalkit` 调 `app.Build(evalDeps)` 装配被测 agent；L0 阶段注入只返回 `REFUSED` 的空实现桩，L4 起替换为真实主链路，`evalkit` 代码不变。
3. 调度按样本执行，`--n` 次数参数随骨架交付（默认 3，§7.2-7 的本地自测即用它），每个 trial 记录 `sample_id / outcome / latency / error`；L8 补齐 pass^k reducer 与 flaky 统计。
4. `evalkit/report` 汇总为 Markdown，写本地 `eval/out/{exp_id}/report.md`；L7 起同时上传 MinIO bucket `agri-steward-eval`。
5. `cmd/eval validate` 只执行第 1 步，不装配 agent，供样本作者自检。
6. 样本 `tags` 带 `Ln` 编号与 `question_taxonomy` 枚举，报告按 tag 分组统计，供后续各 Ln 只回放本层样本（`--tags L2`）。

② SQL：无（样本、参考解、`question_taxonomy` 均为 `eval/` 下的 YAML 文件）。L1 起评测库按母文档 §4.6.1「评测库」：`agri_eval` 只放 `experiment / trial`，迁移在 `migrations/eval/`；每个 trial 的业务数据写临时 schema `agri_eval_t_{n}`，跑同一套业务 migrations。

③ Redis 键：无。

### 6.2 L1 核心契约与数据模型冻结

**目标**：先钉死数据契约与注入接缝，作为 L2 之后并行开发的前置条件。

**涉及模块**：`internal/model`（`AdviceCard`、`Citation`、`WorkOrder`、`ApprovalTicket`、`RunState`，状态机常量与迁移表）、`internal/service`（`WorkOrderService.Transition` 唯一迁移入口）、`internal/repository`、`internal/agent/harness/runner`（`run_id`）、`internal/app`（`app.Build`）、`migrations/`、`deploy/docker-compose.yml`、契约 JSON Schema 目录、各注入接缝接口定义。

**功能任务**

1. 按母文档 §5.0.5 字段定义 `AdviceCard`、`WorkOrder`、`Citation`、`RunState`、`ApprovalTicket`，全部含 `tenant_id / farm_id`；`RunState` 预留 `compaction_state`。
2. 契约以 JSON Schema 落库，Go 结构体与 schema 由同一来源生成或双向校验，保证代码与文档一致；schema 带版本号。
3. `WorkOrder` 状态机：状态常量 + 合法迁移表，迁移表直接取母文档 §4.6.2-11（本 Ln 冻结，各阶段只用不改），提供唯一迁移入口，非法迁移返回错误；同步画状态机图入文档。
4. 定义注入接缝接口：`Clock`、`ToolProvider`、`DeviceAdapter`、存储（`RunState`、审计、`raw_store`、`WorkOrder`）、`KnowledgeSource`、`MemoryStore`、`UserChannel`、`Approver`；生产代码禁止直接读系统时间、直接连库（加 lint 规则或评审清单项）。
5. 实现 `app.Build(deps)`：`cmd/server` 传生产依赖，`evalkit` 传评测依赖，装配逻辑只有一份。
6. 发起契约评审会（第 2 周），评审通过即冻结；冻结后变更走版本号 + Owner 批准。
7. 最小鉴权（母文档 §4.6.2-1）：静态「用户 → 农场 → 角色」配置 + HS256 token，所有接口走 `/api/v1/farms/{farm_id}/...`，审批人、发起人、raw 读取授权都以解析出的 `Scope` 为准。
8. 地块主数据：`farm_profile` / `field_profile` 随最小链路建表，`field_profile.id` 即全系统 `field_id`；示范农场地块由种子文件导入，L2 起由 `mcp-field` 同步。

**交付物**：五个契约 schema（v1）、状态机图与迁移表、接缝接口包、`app.Build`、契约评审纪要。

**验收**

1. 五个契约以 schema 形式落库，代码与文档一致。
2. 状态机非法迁移有表驱动单测覆盖，非法迁移一律报错而非静默通过。
3. 契约变更走版本号，冻结后变更需 Owner 批准并同步所有下游。
4. `cmd/server` 与 `evalkit` 通过同一个 `app.Build` 装配，只替换依赖实现（evalkit 能用评测依赖装配出主链路骨架）。
5. 无 token、token 验签失败或路径 `farm_id` 不在 `Scope` 内的请求分别返回 401 / 403，有单测覆盖。

**技术实现**

① 流程

1. `docker compose -f deploy/docker-compose.yml up -d` → `cmd/migrate up` 执行 `migrations/0001_init.up.sql`：建 L1 表与第 1-2 周最小链路要用的表（`agent_run`、`run_step`、`run_checkpoint`、`run_event`、`tool_call`、`audit_log`、`raw_object`、`farm_profile`、`field_profile`、`work_order`、`work_order_transition`、`outbox`；后几张的 DDL 写在所属 Ln），其余表随所属 Ln 追加 `000N_*.up.sql`（版本号按合入顺序领取，母文档 §4.6.1 表设计约定 8）；创建 bucket `agri-steward-raw`、`agri-steward-eval`；`cmd/migrate up --target eval` 执行 `migrations/eval/`，建 `agri_eval` 的 `experiment`、`trial`（DDL 见母文档 §4.6.1）。
2. `internal/model`：五个契约结构体与 `contracts/*.schema.json` 双向校验（单测断言）；`model.WorkOrderTransitions` 照抄母文档 §4.6.2-11，`model.CanTransition(from, to)` 只查表。契约字段到列的映射：`approver` 取 `approval_ticket.operator_id`（HIGH 另有 `first_approver_id`），`approval_expires_at` 取 `approval_ticket.expires_at`，均不在 `work_order` 冗余存储；`done_area` 对应 `work_order.done_area_mu`（P1 回填）。
3. 唯一迁移入口 `service.WorkOrderService.Transition(ctx, tx, orderID, to, actor, reason)`：`CanTransition` 校验 → 在调用方事务内 `repository.WorkOrderRepo.UpdateStatus`（`row_ver` 乐观锁）+ `InsertTransition` + `EventStore.AppendTx(tx, ORDER_TRANSITION)`（L7） → 影响行数 0 返回 `ErrConcurrentUpdate`，非法迁移返回 `ErrIllegalTransition`。`Transition` 不取 Redis 锁，锁只由外层 service 持有（母文档 Redis 键约定 4）。
4. 鉴权：`controller` 中间件验 HS256 token（密钥在 `deploy/.env`），按 `configs/auth.yaml`（`user_id → [{tenant_id, farm_id, roles}]`，平台角色为 `(0, 0, platform_admin)`）解析出 `Scope{tenant_id, farm_id, user_id, roles}` 放进 `ctx`；路径 `farm_id` 不在 `Scope` 内返回 403，请求体中的 `farm_id` 一律忽略。
5. 地块主数据：`cmd/migrate seed --file configs/seed/fields.yaml` 按 `uk_ext` upsert 示范农场的 `farm_profile` / `field_profile`；L2 起 server 内定时任务（`lock:scan:field_sync` 互斥，每日一次）调 `mcp-field` 列出地块，按 `ext_field_code` 同步，新地块插入、已有地块更新 `crop / sowing_date / area_mu`。
6. `app.Build(deps)`：`cmd/server` 注入 MySQL / Redis / MinIO 的 `repository` 实现；`evalkit` 注入指向 `agri_eval_t_{n}` 的同一套 MySQL repository、`env=eval-t{n}` 的 Redis 前缀（母文档 Redis 键约定 1）和 `FrozenClock`，不用内存实现；harness 的存储接口定义在 `internal/agent` 内。`RawStore.Put` 写 MinIO（key 按母文档 §4.6.2-8）后插入 `raw_object`；换阿里云 OSS 只改 `endpoint` 和密钥配置。

② SQL

```sql
CREATE TABLE agent_run (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, field_id, user_id,   -- 关联列均 bigint unsigned；user_id 为发起人（隐私主体）
  status varchar(16) NOT NULL, interrupt_type varchar(16) NULL,  -- RUNNING/INTERRUPTED/COMPLETED/FAILED/CANCELED；WAIT_APPROVAL/WAIT_INPUT/SYSTEM_ERROR
  reason_code varchar(32) NULL,          -- 终态原因：APPROVAL_REJECTED / APPROVAL_EXPIRED / CONDITION_CHANGED / RETRY_EXHAUSTED
  graph_version varchar(16) NOT NULL, prompt_version varchar(16) NOT NULL, model_tier varchar(16) NOT NULL,
  knowledge_ref varchar(40) NOT NULL, compaction_state json NULL, event_seq int NOT NULL DEFAULT 0,
  resume_epoch int NOT NULL DEFAULT 0,   -- 每次恢复 / 重入 / 下发前复检 +1，进入时效工具的 call_key
  sys_retry smallint NOT NULL DEFAULT 0, -- SYSTEM_ERROR 重入次数
  superseded_by_run_id bigint unsigned NULL, row_ver int NOT NULL DEFAULT 0, created_at datetime(3), updated_at datetime(3),   -- updated_at 兼作心跳
  KEY idx_scope_status (tenant_id, farm_id, status, updated_at), KEY idx_user (tenant_id, farm_id, user_id, created_at),
  KEY idx_stuck (status, updated_at));   -- idx_stuck worker 专用；下文省略类型的列同此：bigint unsigned / datetime(3)，均 NOT NULL
CREATE TABLE work_order (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, field_id, advice_id, run_id,
  work_type varchar(16) NOT NULL, device_type varchar(16) NOT NULL, payload json NOT NULL,
  time_window_from datetime(3) NOT NULL, time_window_to datetime(3) NOT NULL, status varchar(16) NOT NULL,
  dispatch_mode varchar(8) NOT NULL,     -- DRY_RUN / REAL，P0 固定 DRY_RUN
  dispatch_deadline_at datetime(3) NULL, -- 审批通过时写入：min(time_window_to, now + 下发时效)
  dispatch_result json NULL, done_area_mu decimal(10,2) NULL,   -- done_area_mu P1 回填，P0 恒 NULL
  schema_ver smallint NOT NULL, row_ver int NOT NULL DEFAULT 0, created_at, updated_at,
  UNIQUE KEY uk_advice (tenant_id, farm_id, advice_id), KEY idx_scope_status (tenant_id, farm_id, status, updated_at),
  KEY idx_dispatch_deadline (status, dispatch_deadline_at));   -- worker 专用
CREATE TABLE work_order_transition (    -- 只追加
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, order_id, run_id, from_status varchar(16), to_status varchar(16),
  actor_type varchar(8), actor_id bigint unsigned, reason_code varchar(32) NULL, created_at, KEY idx_order (tenant_id, farm_id, order_id, id));
CREATE TABLE raw_object (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, run_id NULL, kind varchar(16),   -- LLM_IO/TOOL_RAW/SNAPSHOT/CHECKPOINT/EXPORT
  object_key varchar(256), sha256 char(64), size_bytes bigint, created_at,
  UNIQUE KEY uk_key (object_key), KEY idx_run (tenant_id, farm_id, run_id, kind));
CREATE TABLE farm_profile (id ..., tenant_id, farm_id, name varchar(64), region varchar(32),
  timezone varchar(32) NOT NULL DEFAULT 'Asia/Shanghai', main_crops json,
  row_ver int NOT NULL DEFAULT 0, updated_at, UNIQUE KEY uk_farm (tenant_id, farm_id));
CREATE TABLE field_profile (id ...,   -- id 即全系统的 field_id（地块引用 ID）
  tenant_id, farm_id, ext_field_code varchar(64), crop varchar(32), sowing_date date, area_mu decimal(10,2),
  prefs json, row_ver int NOT NULL DEFAULT 0, updated_at, UNIQUE KEY uk_ext (tenant_id, farm_id, ext_field_code));

UPDATE work_order SET status=?, row_ver=row_ver+1, updated_at=? WHERE id=? AND tenant_id=? AND farm_id=? AND status=? AND row_ver=?;   -- 与下句、AppendTx 同事务
INSERT INTO work_order_transition (tenant_id, farm_id, order_id, run_id, from_status, to_status, actor_type, actor_id, reason_code, created_at) VALUES (...);
INSERT INTO field_profile (tenant_id, farm_id, ext_field_code, crop, sowing_date, area_mu, prefs, updated_at) VALUES (...) AS new
  ON DUPLICATE KEY UPDATE crop=new.crop, sowing_date=new.sowing_date, area_mu=new.area_mu, row_ver=field_profile.row_ver+1, updated_at=new.updated_at;   -- 种子导入 / mcp-field 同步
```

③ Redis 键（锁一律 `SET key {token} NX PX`，释放与续期用 Lua 比对 token，母文档 Redis 键约定 4）

| 键 | TTL | 用途 |
|----|-----|------|
| `lock:order:{order_id}` | 10s | 外层 service（`Decide`、`Dispatch`、扫描任务）对同一作业单的写串行化，`row_ver` 兜底；锁内不做外部 IO |
| `lock:scan:{task}` | 60s | server 内扫描任务互斥（`field_sync` 及 L6 / L7 各扫描任务） |

### 6.3 L2 MCP 数据接入层

**目标**：让 Agent 的每个事实都来自工具返回，而不是模型记忆。

**涉及模块**：`mcp-gateway`、`internal/agent/tools/mcp`（gateway 客户端、错误码归一）、`internal/agent/harness/policy`、`internal/repository`（`tool_call`）、`mcp-weather` / `mcp-crop` / `mcp-field` / `mcp-device` 四个 Server、工具配置（`mock` / `real` 切换）、`internal/evalkit/env`（`FixtureMCP`）。

**功能任务**

1. 搭 `mcp-gateway`：工具注册表、schema 声明、鉴权、超时（默认 3s）、重试（幂等只读工具重试 2 次）、结果标准化与错误码归一。
2. `mcp-weather`：实时天气、3-7 日预报、历史同期、灾害预警；统一输出 `风速 / 降雨量 / 温度 / 湿度 / 数据时间 / 数据源`。第 1-2 周先打通真实数据源（依赖 D6），作为最小链路的第一个真实工具。
3. `mcp-crop`：作物品种、生育期与物候判定（播种期 + 积温 + 日期推算当前生育期）、病虫害谱、防治指标阈值。
4. `mcp-field`：地块边界与面积、土壤基础属性、当季种植档案、历史作业记录（近 N 次施药施肥灌溉）。
5. `mcp-device`：可用设备清单、在线状态、续航、作业能力（载药量 / 幅宽 / 作业效率），本阶段允许全 mock。
6. 每个工具 `mock` 与 `real` 双实现，用配置切换；mock 的字段与真实接口一致，并维护一份「mock/real 实现状态清单」交给 Phase 1。
7. 工具返回统一带 `data_freshness` 与 `source`，供 `Citation` 与拒答判断使用。
8. 实现 `FixtureMCP`：按样本 fixture 返回，支持 `error: timeout | empty | malformed` 故障注入与按调用次序返回（`returns: [...]`）；样本未声明的调用返回标准错误码并记录 `unexpected_call`。

**交付物**：`mcp-gateway`、四个 MCP Server（含 schema 自描述）、mock/real 实现状态清单、`FixtureMCP`、三类故障评测样本各 1 条。

**验收**

1. 四个 MCP Server 全部可被 Agent 发现并调用，schema 自描述完整。
2. 超时、返回空、返回异常三类故障各有单测，失败时不产生凭空建议（走拒答路径）。
3. 每次工具调用在 trace 中可见 `tool_call_id / 入参 / 出参摘要 / 耗时 / 数据源`。
4. `mock → real` 切换只改配置，不改业务代码。
5. 三类故障用例各有一条评测样本，由 `FixtureMCP` 故障注入驱动；样本未声明的工具调用被拒绝并记录 `unexpected_call`。

**技术实现**

① 流程

1. `agent/graph` 的 enrich 节点并行发起工具调用 → `agent/harness/policy.Before`（等级校验、参数 schema 校验）→ `agent/tools/mcp.Gateway.Call`。
2. 计算 `call_key = hex(sha256(run_id|step_id|tool|canonical(args)|call_seq|epoch))[:32]`，`epoch` 对 `time_sensitive` 工具取 `agent_run.resume_epoch`、其余取 0，所以恢复重跑和下发前复检时天气、设备状态必然换键重取。去重按母文档 Redis 键约定 3、5：`SET idem:tool:{call_key} PENDING:{token} NX PX 9000`；命中 `DONE:{tool_call_id}` 直接回查该行；命中 `PENDING` 说明同 key 调用在途，等待至超时后按 MySQL 判断；未命中或 Redis 丢失时查 `tool_call` 中该 `call_key` 的 `status='OK'` 行，有则返回，没有就视为没做过，重新调用。
3. 可缓存的只读工具先查 `cache:mcp:{tool}:{args_digest}`；`epoch > 0` 的时效工具调用与 `step_id='pre_dispatch'` 的复检调用一律不读缓存（结果仍回写缓存）。未命中则调 MCP Server（超时 3s，`read` 且幂等的工具重试 2 次），错误码归一为 `TIMEOUT / EMPTY / MALFORMED / UPSTREAM / REJECTED`。
4. `policy.After` → `ToolCallStore.Save`（`agent` 内接口，`repository.ToolCallRepo` 实现）：结果摘要经 `pii.Mask` 后入 `tool_call`（`attempt` 取该 `call_key` 已有最大值 + 1），原始返回经 `RawStore.Put` 写 MinIO；`OK` 时把 Redis 键改写为 `DONE:{tool_call_id}`（TTL 24h），并把同 run、同工具、`epoch` 更小且未作废的时效结果置 `superseded_at`；失败结果照常落行，但按 token 删除 Redis 键，不参与去重。随后发 `TOOL_CALL_END` 事件。
5. 失败结果写入 `RunState`，下游节点据错误码走拒答，不产生建议；`FixtureMCP` 实现同一 `ToolProvider` 接口，在 `app.Build(evalDeps)` 中替换 gateway。

② SQL

```sql
CREATE TABLE tool_call (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY,
  tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL,
  run_id bigint unsigned NOT NULL, step_id varchar(48) NOT NULL, call_key char(32) NOT NULL,
  attempt smallint NOT NULL,                                            -- 同 call_key 第几次真实调用（失败后重做 +1）
  epoch int NOT NULL DEFAULT 0,                                         -- = 计算 call_key 时的 resume_epoch（非时效工具恒 0）
  tool_name varchar(64) NOT NULL, policy_level varchar(8) NOT NULL,     -- read / write
  args_digest char(64) NOT NULL, status varchar(16) NOT NULL,           -- OK / TIMEOUT / EMPTY / ERROR / REJECTED
  error_code varchar(32) NULL, source varchar(64) NULL, data_freshness_sec int NULL,
  time_sensitive tinyint(1) NOT NULL DEFAULT 0,                         -- 天气、设备状态：恢复与复检时换 epoch 重取
  result_summary json NULL, raw_object_id bigint unsigned NULL, latency_ms int NOT NULL,
  superseded_at datetime(3) NULL,                                       -- 被更高 epoch 的结果取代；只由 NULL 置值一次，不另设 row_ver
  created_at datetime(3) NOT NULL,
  UNIQUE KEY uk_call_attempt (tenant_id, farm_id, call_key, attempt),
  KEY idx_run (tenant_id, farm_id, run_id, step_id),
  KEY idx_scope_tool (tenant_id, farm_id, tool_name, created_at));

-- 去重回查：只认成功结果
SELECT id, result_summary, raw_object_id FROM tool_call
 WHERE tenant_id=? AND farm_id=? AND call_key=? AND status='OK' ORDER BY attempt DESC LIMIT 1;
-- 落行：同一 attempt 重复写入不报错，LAST_INSERT_ID() 返回已有行 id
INSERT INTO tool_call (tenant_id, farm_id, run_id, step_id, call_key, attempt, epoch, tool_name, ...)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ...) ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id);
-- 旧 epoch 的时效结果作废
UPDATE tool_call SET superseded_at=? WHERE tenant_id=? AND farm_id=? AND run_id=? AND tool_name=?
   AND time_sensitive=1 AND epoch<? AND superseded_at IS NULL;
```

③ Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `idem:tool:{call_key}` | 占位 `PENDING:{token}` 9s（3s × 3 次）；成功后 `DONE:{tool_call_id}` 24h | 工具调用去重前置，失败或回滚按 token 删除；丢失时以 `tool_call` 的 `OK` 行为准 |
| `cache:mcp:{tool}:{args_digest}` | weather 10min / crop 24h | 只读结果缓存；`mcp-field`、`mcp-device` 不缓存（P1 L6 改为带 `farm_id`、TTL 延长到 `max_staleness` 以支持降级） |

### 6.4 L3 固定知识库与渐进式加载

**目标**：用「写死的文件 + 按需加载」跑通知识层，同时让 `calc` 承担全部数值与药剂候选集合。

**涉及模块**：`knowledge/`（`index.yaml`、`<crop>/`、`common/`，结构见母文档 §5.0.7）、`internal/agent/knowledge`（`progressive_loader`、`KnowledgeSource`）、`internal/agent/calc`、`internal/evalkit/grader/code`（`citation_valid`）、`internal/evalkit/sample`（静态校验扩展）。

**功能任务**

1. 建 `knowledge/` 目录与 `index.yaml` 格式；专家先产出 10 条知识条目样例（第 2 周，验证模板是否好写），再补齐四类问题。
2. 知识条目统一 front-matter：`kb_id / crop / stage / region / category / authority_level / version / author / effective_from`；加载时校验必填项，缺失即拒绝加载。
3. 实现 `progressive_loader`：按 `question_taxonomy` 分类结果 + 情境（作物 / 生育期 / 问题类型）从 `index.yaml` 选片段，单次注入上限可配置（建议 ≤ 5 条）。
4. 实现 `calc`：亩用量 → 总用量、稀释倍数、PHI 推算作业日、施肥量；按「作物 × 防治对象 × 农药登记适用范围 × 区域」从 `pesticide_registry.yaml` 输出 `product_candidates`。每个数值结果带计算过程（输入、公式所属 `kb_id@version`），供 L4 回填与 L5 比对。
5. 加载命中 0 条时返回 `KB_MISS`，不允许回落到模型自身记忆。
6. `KnowledgeSource` 支持按 git 提交号加载指定知识版本（生产读当前版本，评测读样本 `knowledge_ref`）。
7. 评测：实现 `citation_valid` 打分器；样本静态校验补「`must_cite` 在指定知识版本中存在」「`product_in` ⊆ `calc` 候选集合」两条。

**交付物**：首个作物知识条目全集、`index.yaml`、`progressive_loader`、`calc`（含单测）、`citation_valid`、加载结果打印脚本。

**验收**

1. 首个作物四类问题的知识条目齐备，每条有 `authority_level` 与署名专家。
2. 100 条评测集每条的加载结果可打印，人工抽查 20 条确认加载到的知识确实相关。
3. `calc` 用量计算单测与专家手算一致；药剂候选集合与农药登记信息一致，不含未登记于该作物的药剂。
4. 知识未覆盖的问题稳定走 `KB_MISS` → 拒答，不出现编造。
5. `citation_valid` 有表驱动单测；100 条样本全部通过新增的两条静态校验。

**技术实现**

① 流程

1. 启动时 `agent/knowledge.KnowledgeSource.Open(ref)`：按 git 提交号读取 `knowledge/index.yaml` 与条目 front-matter，必填项缺失即返回错误、拒绝加载；结果按 `knowledge_ref` 缓存在进程内（只读）。
2. load_knowledge 节点调 `knowledge.Loader.Load(ctx, taxonomy, situation)`：按 `crop / stage / region / category` 过滤 → 按 `authority_level` 排序 → 截断到配置上限（默认 5 条）；命中 0 条返回 `KB_MISS`，图上直接走拒答分支。
3. candidates 节点调 `agent/calc.Candidates(crop, target, region)`：从 `pesticide_registry.yaml` 取登记范围内药剂；空集合同样走拒答。
4. compute 节点调 `calc.Dosage / Dilution / PHIDate / Fertilizer`，每个结果返回 `{value, unit, trace{inputs, formula: kb_id@version}}`，供 L4 回填与 L5 比对。
5. 生产读当前提交号，评测由样本 `knowledge_ref` 指定；`cmd/eval validate` 新增两条静态校验（`must_cite` 存在于该版本、`product_in` ⊆ `calc` 候选）。
6. 加载结果打印脚本 `cmd/eval kbdump --suite regression`：对每条样本输出命中的 `kb_id@version` 列表，供验收 2 人工抽查。

② SQL：无（知识条目与农药登记表为仓库内文件，按 git 版本管理；引用的落库见 L4 `advice_citation`）。

③ Redis 键：无（知识库在进程内只读缓存，体量小，不经 Redis）。

### 6.5 L4 Agent 主链路与建议生成

**目标**：把编排跑起来 —— 模型负责理解与选择，事实来自工具，依据来自知识，数值来自 `calc`。

**涉及模块**：`internal/agent/llm`（Eino `ChatModel` 薄封装：档位、超时重试、token 计量、脱敏挂载点）、`internal/agent/graph`（compose Graph 主链路、`AdviceCard → WorkOrder(DRAFT)` 转换器、降级分支）、`internal/controller` / `internal/service`（ask 入口）、提示词目录（分层 + 版本号）、模型与提示词配置、`internal/evalkit/grader`（`schema_valid`、`numbers_match_calc`、`product_in_candidates`、`outcome_match`、`must_call` / `budget`）。

**功能任务**

1. 通过 Eino `qwen` 组件接入千问底座（形态依 D1），模型档位与参数（温度、`max_tokens`、超时）写进配置，可一键切换与回滚。
2. 用 `compose` Graph 固定编排 `run(question, context)`：`理解与分类 → 情境补全（MCP）→ 知识加载 → 规则层给出药剂候选集合 → 模型在集合内选方案 → 规则层数值计算 → 组装 AdviceCard → 引用、药剂与数值校验 → 落库`；模型只出现在「理解与分类 / 方案选择 / 解释生成」三个节点。
3. 方案选择节点的模型输出只允许返回候选集合内的标识，不允许输出数值；数值字段一律由 `calc` 回填。
4. 系统提示词分层（角色 / 边界 / 输出格式 / 拒答规则），纳入版本管理，变更可 diff、可回滚；`prompt_version` 写入 `RunState` 与审计。
5. `AdviceCard → WorkOrder(DRAFT)` 转换器：把作业类建议的执行动作映射为设备作业参数；观察类建议不产生 `WorkOrder`。生成草稿即停，不自动推进。
6. 降级路径：LLM 不可用时，对「防治指标已明确 + 知识条目已命中」的场景由规则层直出保守建议并标注 `degraded=true`；无法降级则明确报不可用。
7. 评测：实现本层五个打分器（`numbers_match_calc` 按字段约定精度比较，`outcome_match` 忽略 ID 与时间戳）；evalkit 能对单个样本跑一次完整主链路并出报告。

**交付物**：主链路 Graph、`internal/agent/llm` 封装、提示词 v1、转换器、降级分支、本层打分器与单测、20 条样例样本的回放报告。

**验收**

1. 20 个真实问题端到端跑通，产出结构合法的 `AdviceCard`；由 evalkit 回放并打分，不靠手工检查。
2. 所有数值字段可追溯到 `calc` 的计算过程，模型输出中的裸数值被校验层拒收。
3. 提示词与模型配置变更可通过配置回滚，不需改代码发版。
4. LLM 主动断连演练：链路按降级路径返回，不抛未捕获异常、不产生无依据建议。
5. 本层打分器全部有表驱动单测；20 条样例样本的参考解能通过全部打分器（`cmd/eval validate`）。

**技术实现**

① 流程

1. `POST /api/v1/farms/{farm_id}/ask` → `controller.AskHandler`（鉴权中间件得 `Scope`、参数校验、`X-Request-Id`、`rl:farm:{farm_id}:ask` 限流）→ `service.AskService.Ask`：插入 `agent_run(RUNNING, user_id=Scope.user_id)`，异步启动主链路，立即返回 `run_id`。
2. `agent/harness/runner.Run(run_id)` 持 `lock:run:{run_id}`（token 锁，每 10s 用 Lua 续期并刷新 `agent_run.updated_at` 作为心跳；续期失败即停止执行、不再写库）执行 Eino Graph：`classify[LLM] → enrich[MCP 并行] → load_knowledge → candidates → choose[LLM，只返回候选 ID 枚举] → compute[calc] → explain[LLM，数字用 {{n0}} 占位] → assemble → validate → persist → to_order`。
3. callbacks 在每个节点开始 / 结束时 upsert `run_step`（行别名写法，见下）；`agent/llm` 每次调用先过 `rl:model:{tier}`，超时或不可用时走降级分支（`degraded=1`）或 `REFUSED`。
4. persist 节点经 `AdviceStore`（`agent` 内接口，`repository` 实现）在同一事务写 `advice_card` + `advice_citation` + `episodic_memory(ADVICE)`，三者都靠业务唯一键幂等，整图重跑不产生第二张卡；`measured` 引用必须带 `tool_call_id`。
5. to_order 节点：作业类建议生成 `work_order(DRAFT)`，`dispatch_mode` 按建单时 `enable_robot_dispatch` 取值（P0 固定关 → `DRY_RUN`），`uk_advice` 保证一卡一单；观察类不建单，run 置 `COMPLETED`。

② SQL

```sql
CREATE TABLE run_step (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, run_id, step_id varchar(48), node varchar(32),
  status varchar(16), started_at, ended_at NULL, error_code varchar(32) NULL,   -- STARTED / COMPLETED / FAILED / SKIPPED
  row_ver int NOT NULL DEFAULT 0, UNIQUE KEY uk_run_step (tenant_id, farm_id, run_id, step_id));
CREATE TABLE advice_card (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, field_id, run_id,
  crop varchar(32), phenology_stage varchar(32), category varchar(8), advice_type varchar(8),
  outcome varchar(8), conclusion varchar(512), reasons json, numbers json NULL,   -- outcome: ADVICE / REFUSED（降级只看 degraded）
  product_candidates json NULL, selected_product varchar(64) NULL, next_action varchar(16), refuse_reason varchar(32) NULL,
  degraded tinyint(1) DEFAULT 0, schema_ver smallint, created_at,
  UNIQUE KEY uk_run (tenant_id, farm_id, run_id), KEY idx_scope_field (tenant_id, farm_id, field_id, created_at));   -- 一 run 一卡
CREATE TABLE advice_citation (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, advice_id, seq smallint,
  claim_path varchar(64), type varchar(8),                                          -- kb / measured
  kb_id varchar(96) NULL, kb_version varchar(16) NULL, tool_call_id NULL, collected_at NULL,
  UNIQUE KEY uk_advice_seq (tenant_id, farm_id, advice_id, seq), KEY idx_kb (tenant_id, farm_id, kb_id, kb_version));

-- 开始传 ('STARTED', started_at, NULL, NULL)，结束传 ('COMPLETED' / 'FAILED', 原 started_at, ended_at, error_code)
INSERT INTO run_step (tenant_id, farm_id, run_id, step_id, node, status, started_at, ended_at, error_code)
VALUES (?,?,?,?,?,?,?,?,?) AS new
ON DUPLICATE KEY UPDATE status=new.status, ended_at=new.ended_at, error_code=new.error_code, row_ver=run_step.row_ver+1;
INSERT INTO advice_card (tenant_id, farm_id, field_id, run_id, ...) VALUES (?, ?, ?, ?, ...)
ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id);   -- 重跑返回已有卡，citation 同理按 uk_advice_seq 去重
```

③ Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `lock:run:{run_id}` | 30s，持有期每 10s 用 Lua 比对 token 续期 | 同一 run 只有一个执行者；续期同时刷新 `agent_run.updated_at` |
| `rl:farm:{farm_id}:ask` | 1min | 单农场提问限流 |
| `rl:model:{tier}` | 1min | 按模型档位限流，超限走降级 |

### 6.6 L5 护栏层（审批 / 脱敏 / 输出校验）

**目标**：把两条红线做成代码，而不是做成规范文档。

**涉及模块**：`internal/agent/harness/policy`（`tool_policy` 加载与工具注册表绑定、调用前后钩子）、`tool_policy.yaml`、`policy/risk_matrix.yaml`、`internal/agent/guardrail`（`output_validator`、`pii`）、`internal/service`（审批闸门 `ApprovalService`、`ApprovalTicket`）、`internal/agent/llm`（入模前脱敏挂载）、`internal/evalkit/env`（`RecordingAdapter`）、`internal/evalkit/grader`（`no_model_actuate`、`stop_at_approval`、`pii_clean`、`refusal_correct`、`kb_miss_refuse`）。

**功能任务**

1. `tool_policy.yaml` 三级：`read` 直通 / `write` 记录留痕 / `actuate` 不注册给模型、只允许 `workorder` 状态机在 `APPROVED` 后调用；策略层对来自模型的 `actuate` 调用兜底拒绝并告警。
2. 策略文件与工具注册表启动期校验：新增工具未声明等级则启动失败，不默认放行；启动自检断言模型可见工具集中不含任何 `actuate` 工具。
3. 审批闸门：主链路生成 `WorkOrder(DRAFT)` 后开票并中断，产出 `ApprovalTicket` + `RunState` 快照（交 L6 持久化），检查点落库后票据才对审批人可见，再向用户返回「待审批」。按 `policy/risk_matrix.yaml` 定 `risk_level` 与 `required_role`；`HIGH` 票需第二审批人确认（第二人 ≠ 第一人，同在 `required_role` 内；审批人 ≠ 发起人，母文档 §4.6.2-12），决策流程见 L6。
4. `pii_masking`：手机号、身份证、账号、精确经纬度在四个出口掩码 —— 应用日志、OTel span 属性、模型入参、落库摘要（`run_event` / `tool_call` / `audit_log`）；地块坐标入模时降精度为地块级标识。脱敏规则集中在 `internal/agent/guardrail/pii`，L7 审计与导出复用同一实现。
5. `output_validator`：结构校验（schema）、引用校验（每条结论 ≥ 1 条 `Citation`，`kb_id@version` 有效）、数值校验（逐一比对 `calc` 结果）、药剂校验（`selected_product` ∈ `product_candidates`）。
6. 校验失败处置：不修不补、不重试硬凑，直接转拒答 + 落 `VALIDATION_FAILED` 事件（带失败的校验项与 `run_id`）。
7. 评测：实现 `RecordingAdapter` 与本层五个打分器；补齐设备动作请求、应拒答、含敏感信息输入三类样本。

**交付物**：`tool_policy.yaml` v1、policy 钩子、`output_validator`、`pii_masking`、审批闸门、`RecordingAdapter`、本层打分器与单测、补充样本。

**验收**

1. 新增一个未声明等级的工具，服务启动失败（有明确报错）。
2. 「帮我把这块地打药」类请求 100% 停在待审批，审批前设备 Adapter 无任何调用（`stop_at_approval` 基于 `RecordingAdapter` 判定）；模型可见工具集不含 `actuate` 工具（`no_model_actuate` 判定）。
3. 脱敏用例集全通过，应用日志输出、span 属性与模型入参中无敏感明文（`pii_clean` 同时扫描 trial 的应用日志）。
4. 人为注入一条无引用建议、一条数值篡改建议、一条集合外药剂建议，均被校验层拒收并落事件。
5. 本层打分器全部有表驱动单测，且每个打分器至少一条「应失败」用例，证明它能抓到违规。

**技术实现**

① 流程

1. 启动期 `app.Build` → `agent/harness/policy.Load("policy/tool_policy.yaml")` 与工具注册表逐项比对：未声明等级即返回错误、进程退出；断言模型可见工具集不含 `actuate`。
2. 脱敏按四个出口挂载，统一调用 `agent/guardrail/pii.Mask`：①应用日志由自定义 `slog.Handler` 在输出前处理全部属性与错误链；②OTel 由 `SpanProcessor` 在导出前过滤属性，span 本身只带 ID 与控制字段；③模型入参在 `agent/llm` 组装消息时处理；④落库摘要由 service / agent 在写 `run_event`、`tool_call`、`audit_log` 之前处理（repository 不负责脱敏）。手机号 / 身份证 / 账号掩码，坐标替换为地块引用 ID，日志坐标保留 0.01°。
3. validate 节点调 `guardrail.OutputValidator.Validate(card, calcResult, candidates)`，按 schema → 引用 → 数值 → 药剂顺序校验；任一失败即 `outcome=REFUSED, refuse_reason=VALIDATION_FAILED`，并发 `VALIDATION_FAILED` 事件，不重试。
4. to_order 节点写 `work_order(DRAFT)` 后调 `ApprovalGate.Open`（`agent` 内接口，`app` 装配为 `service.ApprovalService`）：按 `policy/risk_matrix.yaml` 定 `risk_level`、`required_role`、`confirm_required`（P0 中 SPRAY 一律 HIGH、需二次确认）；审批快照先经 `RawStore.Put` 写 MinIO（key 含 `order_id` 与 `ticket_seq`，重跑覆盖同一对象）并计算 `snapshot_digest`；随后**单事务**写 `constraint_check(PRE_APPROVAL, attempt=ticket_seq)`（P0 只展示、不拦截）、插入 `approval_ticket(PREPARING, ticket_kind=ORDER, initiator_id=agent_run.user_id)`、`Transition(DRAFT → PENDING_APPROVAL)`（已是 `PENDING_APPROVAL` 则跳过）、`AppendTx(APPROVAL_OPEN)`。三处写入都靠业务唯一键幂等，整图重跑返回已有票据。
5. runner 以 `WAIT_APPROVAL` 中断 → L6 落检查点（同事务置 run `INTERRUPTED`）→ runner 调 `ApprovalGate.Activate(run_id)`：确认 run 已是 `INTERRUPTED(WAIT_APPROVAL)` 后把票据 `PREPARING → OPEN` 并写 `expires_at`，发 `APPROVAL_PENDING` 事件，向用户返回「待审批」。`PREPARING` 票不进待审批列表、不接受决策；`Activate` 漏执行由 L6 的 `ticket_activate` 扫描补上。

② SQL

```sql
CREATE TABLE constraint_check (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, order_id,   -- 均 bigint unsigned NOT NULL
  phase varchar(16) NOT NULL,           -- PRE_APPROVAL / PRE_DISPATCH
  attempt smallint NOT NULL, rule_id varchar(48) NOT NULL, passed tinyint(1) NOT NULL,   -- attempt：PRE_APPROVAL 取 ticket_seq，PRE_DISPATCH 取复检序号
  observed json NOT NULL,               -- {value, unit, source, collected_at, tool_call_id}
  threshold json NOT NULL,              -- 带 kb_id@version
  created_at datetime(3) NOT NULL,
  UNIQUE KEY uk_check (tenant_id, farm_id, order_id, phase, attempt, rule_id));
CREATE TABLE approval_ticket (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, run_id, order_id,
  ticket_kind varchar(16) NOT NULL DEFAULT 'ORDER',   -- ORDER（P3 增 PLAN），母文档 §4.6.2-6
  ticket_seq smallint NOT NULL,         -- 同一作业单第几张票（P1 复检退回换票 +1）
  risk_level varchar(8) NOT NULL,       -- LOW / HIGH
  required_role json NOT NULL,          -- 可审批角色，如 ["agronomist"]
  confirm_required tinyint(1) NOT NULL DEFAULT 0,   -- HIGH 为 1：需第二审批人确认
  initiator_id bigint unsigned NOT NULL,            -- = agent_run.user_id，不得审批本票
  first_approver_id bigint unsigned NULL,           -- HIGH 票第一审批人
  snapshot_digest char(64) NOT NULL, snapshot_raw_id bigint unsigned NOT NULL,
  status varchar(16) NOT NULL,          -- PREPARING / OPEN / APPROVED / REJECTED / EXPIRED / SUPERSEDED
  operator_id bigint unsigned NULL,     -- 最终决策人（HIGH 为确认人）
  expires_at datetime(3) NULL,          -- PREPARING → OPEN 时写入
  decided_at datetime(3) NULL,
  row_ver int NOT NULL DEFAULT 0, created_at datetime(3) NOT NULL, updated_at datetime(3) NOT NULL,
  UNIQUE KEY uk_order_seq (tenant_id, farm_id, ticket_kind, order_id, ticket_seq),
  KEY idx_scope_status (tenant_id, farm_id, ticket_kind, status, created_at),
  KEY idx_run (tenant_id, farm_id, run_id),
  KEY idx_expire (ticket_kind, status, expires_at));   -- worker 专用

-- 以下四句同一事务（快照对象已在事务前写入）
INSERT IGNORE INTO constraint_check (tenant_id, farm_id, order_id, phase, attempt, rule_id, passed, observed, threshold, created_at) VALUES (...);
INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_kind, ticket_seq, risk_level, required_role, confirm_required,
  initiator_id, snapshot_digest, snapshot_raw_id, status, created_at, updated_at)
VALUES (?, ?, ?, ?, 'ORDER', 1, ?, ?, ?, ?, ?, ?, 'PREPARING', ?, ?) ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id);
-- + Transition(DRAFT → PENDING_APPROVAL) + AppendTx(APPROVAL_OPEN)

-- Activate：检查点落库、run 已中断后才开放
UPDATE approval_ticket t JOIN agent_run r ON r.id=t.run_id AND r.tenant_id=t.tenant_id AND r.farm_id=t.farm_id
   SET t.status='OPEN', t.expires_at=?, t.row_ver=t.row_ver+1, t.updated_at=?
 WHERE t.tenant_id=? AND t.farm_id=? AND t.run_id=? AND t.ticket_kind='ORDER' AND t.status='PREPARING'
   AND r.status='INTERRUPTED' AND r.interrupt_type='WAIT_APPROVAL';
-- 待审批列表
SELECT id, order_id, risk_level, expires_at FROM approval_ticket
 WHERE tenant_id=? AND farm_id=? AND ticket_kind='ORDER' AND status='OPEN' ORDER BY created_at LIMIT 50;
```

③ Redis 键：无（`Open` / `Activate` 只在持 `lock:run` 的执行者内调用，作业单迁移靠 `row_ver`）。

### 6.7 L6 HITL 中断恢复 + 记忆

**目标**：审批与人工介入不能让已完成的工作作废，断点续跑是 MVP 的硬要求。

**涉及模块**：`internal/agent/harness/checkpoint`（实现 Eino checkpoint 存储接口，`RunState` 持久化）、`internal/agent/harness/runner`（`resume(run_id)`、中断类型、`call_key` 幂等）、`internal/service`（审批决策 `ApprovalService.Decide`）、`raw_store`（MinIO 大对象）、`internal/agent/harness/memory`（`profile` / `episodic` / `working`）、`internal/evalkit/env`（脚本审批人、`FrozenClock.Advance()`、`MemoryStore` fixture）、`internal/evalkit/grader`（`state_machine_legal`）。

**功能任务**

1. 检查点只在中断时写入（母文档 §4.6.2-12，L0 spike 验证）：中断时持久化 `completed_step_ids` 与 `context_snapshot`，落库后才开放审批（先持久化再展示）；非中断点崩溃即整图重跑，带副作用的节点（persist、to_order、`ApprovalGate.Open`、episodic）全部靠业务唯一键幂等。快照体积设上限，超限的大对象（工具原始返回等）写 `raw_store`，快照只存引用 ID。
2. `resume(run_id)`：审批通过后从 `pending_approval` 断点继续，按 `completed_step_ids` 跳过已完成步骤，只做收尾（摘要、通知、置 `COMPLETED`），不再下发；下发由审批事务写入的 `outbox(order.dispatch)` 驱动（母文档 §4.6.2-2，见 L8）。
3. 时效性数据（天气、设备状态）在重入重跑与下发前复检时以新 epoch 重取、不读缓存（L2 `call_key`）；下发前复检结论变化则原作业单 `CANCELED(CONDITION_CHANGED)` 并新建 run。哪些工具属于时效性数据，在 `tool_policy.yaml` 或工具注册表中声明，不在代码里写死。
4. 中断类型三种：`WAIT_APPROVAL`、`WAIT_INPUT`、`SYSTEM_ERROR`（可重入，最多 3 次，之后置 `FAILED`）。
5. 审批时效：`ApprovalTicket.expires_at` 到期后票据与作业单转 `EXPIRED`；决策与过期扫描都带 `status + row_ver` 守卫，先提交者生效。驳回、过期时 run 在同一事务置 `COMPLETED` 并写 `reason_code`（Phase 0 只做最小时效，超时策略细化留 Phase 1）。
6. 记忆最小实现：`profile`（农场 + 地块 + 常种作物 + 常用药剂偏好）、`episodic`（近 N 次建议与作业结果）、`working`（当前会话）；注入时按相关性截断；记忆注入提供开关。dry-run 单不写作业记忆、不参与 PHI 推算与作业指标（母文档 §4.6.2-11）。
7. 评测：实现脚本审批人（通过 / 驳回 / 超时 / 二次确认）、`FrozenClock.Advance()`、`MemoryStore` fixture 注入、`state_machine_legal`；补齐审批超时转 `EXPIRED`、审批后天气变化（下发前复检）两类样本。

**交付物**：checkpoint 存储实现、`resume` 接口、三层记忆、本层评测环境组件与打分器、补充样本。

**验收**

1. 审批中断 → 恢复用例：恢复成功率 ≥ 95%，已完成步骤零重复执行（工具调用计数断言）。
2. 进程重启后仍可 `resume`（快照落持久化存储，不依赖内存）。
3. 中断后修改 mock 天气，审批通过后下发前复检使用新数据（时效工具 `tool_call.epoch` 递增、未读缓存）；条件恶化时原作业单 `CANCELED(CONDITION_CHANGED)` 并新建 run（fixture 按调用次序返回实现）。
4. 记忆注入可开关，关闭后链路仍正常工作。
5. 审批超时样本中，作业单经 `FrozenClock.Advance()` 后转 `EXPIRED`，全程状态迁移通过 `state_machine_legal`。
6. 「到期瞬间审批」「Resume 失败」「票据停在 `PREPARING`」「run 心跳丢失」四类用例中，票据、作业单、run 最终状态一致，无悬挂；`HIGH` 票同一人两次通过、发起人自审均被拒绝。

**技术实现**

① 流程

1. Graph 编译时挂 `compose.WithCheckPointStore(store)`，`store` 为 `agent/harness/checkpoint` 接口、`repository.CheckpointRepo` 实现；`checkpoint_id` 取 `run_id` 字符串，只在中断时 Set：同一事务插入 `run_checkpoint`（≤ 256KB 写 `payload`，超限先写 MinIO 再存 `raw_object_id`）并把 run `RUNNING → INTERRUPTED(interrupt_type)`；只保留最近 3 份。
2. `POST /api/v1/farms/{farm_id}/approvals/{ticket_id}/decide`（`X-Request-Id`，body：`decision=APPROVE|REJECT`、`snapshot_digest`、`comment`）→ `controller`（`Scope`）→ `service.ApprovalService.Decide`：
   - 去重：`SET idem:approve:{request_id} PENDING:{token} NX PX 10000`；命中 `DONE:{decision_id}` 返回首次结果，命中 `PENDING` 返回 409 `REQUEST_IN_FLIGHT`，未命中或丢失时查 `uk_request`。
   - 持 `lock:order:{order_id}`，依次校验：票据 `OPEN`（否则 `TICKET_CLOSED`）→ `expires_at > now`（否则同事务票据与作业单 `EXPIRED`、run 收尾，返回 `TICKET_EXPIRED`）→ `snapshot_digest` 一致（`SNAPSHOT_STALE`）→ `Scope.roles ∩ required_role` 非空且 `user_id ≠ initiator_id`（`ROLE_INSUFFICIENT`）。拒绝也写 `audit_log(APPROVAL, result=DENIED)`。
   - `HIGH` 票的 `APPROVE`：`first_approver_id` 为空时只记第一审批人（`approval_decision.decision=APPROVE`），票据仍 `OPEN`，返回 `awaiting_confirm=true`；不为空时要求 `user_id ≠ first_approver_id`（`SAME_APPROVER`），记 `CONFIRM` 并生效。`LOW` 票一次 `APPROVE` 即生效。
   - 生效（同一事务）：`approval_decision` → 票据 `OPEN → APPROVED` → `Transition(PENDING_APPROVAL → APPROVED)` 并写 `dispatch_deadline_at` → `INSERT outbox(topic=order.dispatch, dedup_key=order.dispatch:{order_id}:{ticket_id}:1)`（母文档 §4.6.2-4）→ `AppendTx(APPROVAL_DECIDED)`。
   - `REJECT`（同一事务）：`approval_decision` → 票据 `REJECTED` → `Transition(→ REJECTED)` → run `INTERRUPTED → COMPLETED(reason_code=APPROVAL_REJECTED)` → `episodic_memory(REJECTED)` → `AppendTx(APPROVAL_DECIDED)`。
   - 提交后 Redis 键改写为 `DONE:{decision_id}`（24h）；回滚按 token 删除。
3. 生效提交后异步调 `agent/harness/runner.Resume(run_id)`：持 `lock:run:{run_id}` → 校验 run 为 `INTERRUPTED(WAIT_APPROVAL)`、票据 `APPROVED`、`graph_version` 一致 → run 置 `RUNNING` 且 `resume_epoch + 1`（`row_ver` 守卫）→ Eino resume，已完成节点跳过，只执行收尾节点（摘要、`RUN_RESUMED` / `RUN_COMPLETED` 事件）→ `COMPLETED`。Resume 不调设备、不做复检，失败由 `run_finalize` 扫描补偿。
4. server 内扫描任务（每 30s，`lock:scan:{task}` 互斥，跨农场查询走 `repository.SystemScope`）：
   - `approval_expire`：`OPEN` 且过 `expires_at` 的票据，逐张持 `lock:order` 同事务票据 `EXPIRED`、`Transition(PENDING_APPROVAL → EXPIRED)`、run `COMPLETED(APPROVAL_EXPIRED)`、`AppendTx`；票据 UPDATE 影响行数 0（已被决策）即跳过。`APPROVED` 且过 `dispatch_deadline_at` 的作业单同样转 `EXPIRED`。
   - `ticket_activate`：`PREPARING` 超过 30s 的票据，run 已 `INTERRUPTED(WAIT_APPROVAL)` 则执行 L5 的 `Activate`；run 仍 `RUNNING` 的交给 `stuck_run`。
   - `run_finalize`：run 仍 `INTERRUPTED(WAIT_APPROVAL)` 而票据已 `APPROVED` 超过 60s，重新触发 `Resume`。
   - `stuck_run`：`RUNNING` 且 `updated_at` 超过 90s 未刷新（3 个续期周期无心跳）且 `lock:run:{run_id}` 不存在 → `INTERRUPTED(SYSTEM_ERROR)`、`sys_retry + 1`；`SYSTEM_ERROR` 且 `sys_retry ≤ 3` 的 run 由本任务调 `Resume` 重入（无检查点即整图重跑，`resume_epoch + 1`）；超过 3 次置 `FAILED(RETRY_EXHAUSTED)`，其 `DRAFT` 作业单同事务 `CANCELED`。
5. 记忆：`agent/harness/memory` 组装上下文时读 `farm_profile` / `field_profile` + 近 N 条 `episodic_memory`（开关关闭则跳过）。`ADVICE` 在 L4 persist 同事务写入，`REJECTED` 在驳回同事务写入，均 `INSERT IGNORE`；`WORK_DONE` 只在 `REAL` 单 `DONE / PARTIALLY_DONE` 时写（P1 起），dry-run 单不写；PHI 推算与作业指标只取 `dispatch_mode='REAL'` 的作业单。

② SQL

```sql
CREATE TABLE run_checkpoint (id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, run_id, seq int,
  payload mediumblob NULL, raw_object_id NULL, size_bytes int, created_at, UNIQUE KEY uk_run_seq (tenant_id, farm_id, run_id, seq));
CREATE TABLE approval_decision (       -- 只追加
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id, ticket_id, request_id varchar(64),
  decision varchar(16),                -- APPROVE / CONFIRM / REJECT
  operator_id, operator_role varchar(16), snapshot_digest char(64), comment varchar(512) NULL, created_at,
  UNIQUE KEY uk_request (request_id), KEY idx_ticket (tenant_id, farm_id, ticket_id, id));
CREATE TABLE episodic_memory (id ..., tenant_id, farm_id, field_id, kind varchar(16),   -- ADVICE / WORK_DONE / REJECTED（dry-run 不写）
  ref_id, summary varchar(512), occurred_at,
  UNIQUE KEY uk_kind_ref (tenant_id, farm_id, kind, ref_id), KEY idx_field_time (tenant_id, farm_id, field_id, occurred_at));
-- farm_profile / field_profile 见 L1

-- 中断落检查点（同一事务）
INSERT INTO run_checkpoint (tenant_id, farm_id, run_id, seq, payload, raw_object_id, size_bytes, created_at) VALUES (...);
UPDATE agent_run SET status='INTERRUPTED', interrupt_type=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='RUNNING' AND row_ver=?;
-- 决策生效 / 驳回：带过期守卫，与过期扫描互斥（影响行数 0 → 重读票据按 TICKET_CLOSED / TICKET_EXPIRED 返回）
UPDATE approval_ticket SET status=?, operator_id=?, decided_at=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND ticket_kind='ORDER' AND status='OPEN' AND expires_at > ? AND snapshot_digest=? AND row_ver=?;
-- HIGH 第一审批人
UPDATE approval_ticket SET first_approver_id=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='OPEN' AND first_approver_id IS NULL AND expires_at > ? AND row_ver=?;
INSERT INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, next_run_at, created_at, updated_at)
VALUES (?, ?, 'order.dispatch', ?, CONCAT('order.dispatch:', ?, ':', ?, ':1'), ?, 'PENDING', ?, ?, ?);
-- 驳回 / 过期时 run 收尾
UPDATE agent_run SET status='COMPLETED', interrupt_type=NULL, reason_code=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='INTERRUPTED' AND interrupt_type='WAIT_APPROVAL';
-- 过期扫描（worker 专用索引 idx_expire）
SELECT id, tenant_id, farm_id, order_id, run_id, row_ver FROM approval_ticket
 WHERE ticket_kind='ORDER' AND status='OPEN' AND expires_at < ? LIMIT 100;
UPDATE approval_ticket SET status='EXPIRED', row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='OPEN' AND row_ver=?;
SELECT id, tenant_id, farm_id, row_ver FROM work_order WHERE status='APPROVED' AND dispatch_deadline_at < ? LIMIT 100;
-- 卡住 run 扫描（idx_stuck）：命中后再确认 lock:run 不存在
SELECT id, tenant_id, farm_id, row_ver FROM agent_run WHERE status='RUNNING' AND updated_at < ? LIMIT 100;
UPDATE agent_run SET status='INTERRUPTED', interrupt_type='SYSTEM_ERROR', sys_retry=sys_retry+1, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='RUNNING' AND updated_at < ? AND row_ver=?;
-- 心跳（lock:run 续期时；不改状态，不升 row_ver）
UPDATE agent_run SET updated_at=? WHERE id=? AND tenant_id=? AND farm_id=? AND status='RUNNING';
```

③ Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `idem:approve:{request_id}` | 占位 `PENDING:{token}` 10s；提交后 `DONE:{decision_id}` 24h | 审批决策去重前置，回滚按 token 删除；丢失由 `uk_request` 兜底 |
| `lock:scan:{task}` | 60s | 沿用 L1，本 Ln 新增任务 `approval_expire` / `ticket_activate` / `run_finalize` / `stuck_run` |

### 6.8 L7 审计日志与可观测链路

**目标**：任何一条建议、一次审批、一次 dry-run，事后都能原样还原。

**涉及模块**：`internal/agent/harness/callbacks`（审计、OTel trace、成本埋点）、`audit_log` 表、`raw_store`（MinIO + `raw_object`，bucket 私有、预签名读取、读取留审计）、`internal/service`（`AuditService`）、统一事件流、指标与最小看板、`internal/evalkit/report`（transcript 导出与 `run_id` 链接）、评测独立存储。

**功能任务**

1. `audit_log` append-only：`audit_id / run_id / actor / action / target / payload_digest / result / created_at`；覆盖提问、工具调用、知识加载、模型调用、校验结果、审批决策、dry-run 执行七类事件，外加作业单迁移与 raw 读取。图外的审批、迁移、下发、raw 读取由 service 在业务事务内写审计，与业务同生同灭。
2. 审计表只落摘要或掩码值（复用 L5 `pii`）；回放所需原文写 `raw_store`，按 `run_id` 关联，读取需授权并留审计记录。
3. trace 三级贯通 `run_id → step_id → tool_call_id`，含 LLM 调用 token 数与耗时；trace 与指标只记 ID 与控制字段，内容按 ID 回查 `raw_store`。
4. 指标埋点：会话完成率、工具成功率与 P95、拒答率、校验失败率、审批等待时长、恢复成功率、单会话 token 成本；作业类指标按 `dispatch_mode` 拆分，dry-run 不计入作业指标。
5. 最小看板：一屏看当日趋势；`run_id` 一键检索完整链路（检索能力第 1-2 周随最小链路先上线）。
6. 评测：统一事件流导出为 JSONL transcript（「根节点 - Agent 步骤 - 原子步骤」三层）；报告每个 trial 带 `run_id` 链接；评测数据写 trial schema，不混入生产看板。
7. 维护 `run_event.event_type` 枚举表（母文档 §4.6.2-7，大写下划线，新增先登记），`audit_log` / `run_event` 分区维护任务（母文档 §4.6.1 表设计约定 7）。

**交付物**：`audit_log` 与 `raw_store`、callbacks 全量事件、指标与看板、transcript 导出、对账记录。

**验收**

1. 任取一个历史 `run_id`，1 分钟内还原「问了什么 → 调了什么 → 依据是什么 → 建议是什么 → 谁审批 → 执行结果」。
2. 审计完整率 100%（抽样 50 个 run，无缺事件）。
3. trace 覆盖率 100%，无孤儿 step。
4. 看板指标与日志统计口径一致（人工对账一次并留记录）。
5. 经授权可从 `raw_store` 取回任一 `run_id` 的完整模型输入输出；未授权读取被拒绝并留审计记录。
6. 任一评测 trial 可从报告跳转到 run 详情；评测数据不出现在生产看板指标中。

**技术实现**

① 流程

1. `EventStore.AppendTx(tx, evt)`（`agent` 内接口，`repository` 实现）是唯一写入口：在调用方事务内 `agent_run.event_seq + 1` 取 seq、插入 `run_event`、同步写 `audit_log` 投影；提交后 `PUBLISH sse:run:{run_id}`。`agent/harness/callbacks` 只订阅 Eino 节点、模型、工具回调（图内事件），自开短事务调用它；`Transition`、`Decide`、`Dispatch`、`ReadRaw` 在各自业务事务内调用它。
2. 事件 payload 与审计 `summary` 只含控制字段与 `pii.Mask` 后的摘要（≤ 8KB）；模型输入输出、工具原文经 `RawStore.Put` 写 MinIO，`audit_log.raw_object_id` 关联。
3. `GET /api/v1/farms/{farm_id}/runs/{run_id}/raw/{raw_id}` → `service.AuditService.ReadRaw`：按 `Scope.roles` 校验（P0 允许 `farm_manager / agronomist / platform_admin`）→ 生成 5 分钟有效的 MinIO 预签名 URL；成功时审计与签发同事务，拒绝时单独一条事务写 `audit_log(action=RAW_READ, result=DENIED)`。本地 MinIO 依赖 bucket 私有 + 预签名访问。
4. OTel span 以 `run_id → step_id → tool_call_id` 为属性，只带 ID、耗时、token 数，导出前经 L5 的 `SpanProcessor` 过滤；指标由 callbacks 埋点，看板按 `run_id` 跳转 run 详情（按 `run_id` 查 `run_event` + `audit_log`）。
5. 评测：`evalkit/report` 把 `run_event` 导出为 JSONL transcript，写 `agri-steward-eval` bucket；trial 的业务数据在 `agri_eval_t_{n}`，结果写 `agri_eval.trial`，生产看板不读。
6. `event_type` 枚举（P0）：`RUN_STARTED / STEP_STARTED / STEP_ENDED / TOOL_CALL_END / KB_LOADED / LLM_CALL_END / VALIDATION_FAILED / DEGRADED / ADVICE_READY / APPROVAL_OPEN / APPROVAL_PENDING / APPROVAL_DECIDED / ORDER_TRANSITION / DISPATCHED / RUN_INTERRUPTED / RUN_RESUMED / RUN_COMPLETED / RUN_FAILED`，定义在 `internal/model` 常量，写入时校验；`audit_log.action` 由 `event_type` 映射（如 `APPROVAL_*` → `APPROVAL`），非审计类事件（`STEP_*` 等）不投影。后续阶段已登记的增量（实现到对应阶段时加入常量）：
   - `event_type`：P2 `RULE_SHADOW_DIFF / COMPACTION_APPLIED / COMPACTION_FAILED`；P3 `RETRIEVAL_HIT / RETRIEVAL_FALLBACK / VISION_RESULT / EXPERT_CASE_OPENED / ALERT_EMITTED / ALERT_SUPPRESSED / SCHEDULE_CONFLICT / PLAN_REVISED / ALTERNATIVE_SELECTED`。
   - `audit_log.action`：P1 `FLAG_CHANGE / CROSS_FARM_DENIED / DISPATCH_HELD / RECEIPT_UNMATCHED / KILL_SWITCH_DRILL / RETENTION_PURGE / SENSITIVE.{op}`；P4 `KB_PUBLISH / GOV_DECIDE / BUDGET_DEGRADE / BUDGET_RESTORE / EXPORT_DOWNLOAD / EGRESS_DENIED`。
   - `raw_object.kind`：P1 `DEVICE_CMD / RECEIPT_RAW`；P2 `COMPACTED_MSGS`；P3 `IMAGE`；P4 `FORENSIC`。
   - `work_order_transition.reason_code`：P1 `RECHECK_FAILED / RECONCILED / RETRY_EXHAUSTED`；P3 `SCHEDULE_CHANGED / PLAN_REVISED / PLAN_FAILED / PLAN_REJECTED / PLAN_EXPIRED`。
   - `outbox.topic`：P1 `order.cancel / field.history.append / notify.send / audit.export`；P3 `expert.resolved`；P4 `gov.execute`。
7. 分区维护：迁移时为 `audit_log`、`run_event` 预建 12 个月分区；server 内 `partition-maintainer` 任务（`lock:scan:partition`，每月 25 日，迁移账号执行）`REORGANIZE PARTITION pmax` 预建后两个月，`pmax` 非空即告警。

② SQL

```sql
CREATE TABLE run_event (                -- 只追加，按月分区
  id bigint unsigned AUTO_INCREMENT, tenant_id, farm_id, run_id,   -- 均 bigint unsigned NOT NULL
  seq int NOT NULL, step_id varchar(48) NULL, tool_call_id bigint unsigned NULL,
  event_type varchar(32) NOT NULL,      -- 大写，取值见流程 6
  actor varchar(8) NOT NULL,            -- user / system / agent
  payload json NOT NULL, created_at datetime(3) NOT NULL,
  PRIMARY KEY (id, created_at),
  UNIQUE KEY uk_run_seq (tenant_id, farm_id, run_id, seq, created_at))   -- 分区表唯一键须含分区列；seq 唯一性由 event_seq 原子自增保证
  PARTITION BY RANGE COLUMNS(created_at) (
    PARTITION p202611 VALUES LESS THAN ('2026-12-01'), /* … 迁移时预建 12 个月 … */ PARTITION pmax VALUES LESS THAN (MAXVALUE));
CREATE TABLE audit_log (                -- 只追加，按月分区
  id bigint unsigned AUTO_INCREMENT, created_at datetime(3) NOT NULL, tenant_id, farm_id, run_id bigint unsigned NULL,
  actor_type varchar(8) NOT NULL, actor_id bigint unsigned NOT NULL,
  action varchar(32) NOT NULL,          -- ASK / TOOL_CALL / KB_LOAD / LLM_CALL / VALIDATE / APPROVAL / ORDER_TRANSITION / DISPATCH / RAW_READ
  target varchar(64) NULL, payload_digest char(64) NOT NULL, summary json NULL, result varchar(16) NOT NULL,
  raw_object_id bigint unsigned NULL,
  PRIMARY KEY (id, created_at), KEY idx_scope_time (tenant_id, farm_id, created_at), KEY idx_run (tenant_id, farm_id, run_id))
  PARTITION BY RANGE COLUMNS(created_at) (
    PARTITION p202611 VALUES LESS THAN ('2026-12-01'), /* … 迁移时预建 12 个月 … */ PARTITION pmax VALUES LESS THAN (MAXVALUE));

UPDATE agent_run SET event_seq = LAST_INSERT_ID(event_seq + 1)
 WHERE id = ? AND tenant_id = ? AND farm_id = ?;   -- 调用方事务内取 seq；不升 row_ver，避免与状态迁移的乐观锁互相冲突
INSERT INTO run_event (tenant_id, farm_id, run_id, seq, step_id, tool_call_id, event_type, actor, payload, created_at)
VALUES (?, ?, ?, LAST_INSERT_ID(), ?, ?, ?, ?, ?, ?);

-- partition-maintainer（迁移账号）：在 pmax 前切出新月份
ALTER TABLE audit_log REORGANIZE PARTITION pmax INTO (
  PARTITION p202712 VALUES LESS THAN ('2028-01-01'), PARTITION pmax VALUES LESS THAN (MAXVALUE));
SELECT COUNT(*) FROM audit_log PARTITION (pmax);   -- 非 0 即告警
```

③ Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `sse:run:{run_id}` | pub/sub 频道，不占存储，不受 TTL 约束 | 事件提交后推送；断线续传由 L8 `EventService.Stream` 先订阅后补读 |
| `lock:scan:partition` | 沿用 L1 `lock:scan:{task}` | 分区维护任务互斥 |

### 6.9 L8 最小交互界面 + 示范农场冒烟 + Gate 0 收口

**目标**：让业务与决策层亲手点一遍完整链路，并用 evalkit 产出 Phase 0 唯一对照基线。

**涉及模块**：内部 Web 管家页（对话区 / 待审批区 / run 详情区，订阅统一事件流）、`internal/service` 的 workorder 下发器与 server 内 outbox 轮询器、`internal/adapter` mock Adapter（dry-run，只由下发器调用）、`internal/repository/outbox`、`cmd/eval`（n=3 全量与 pass^k reducer）、`internal/evalkit/reduce`、`eval/baselines/`。

**功能任务**

1. Web 页三块：对话区（提问 + 建议卡，`Citation` 可点开查看知识条目原文或实测数据）、待审批区（`WorkOrder` 快照 + `constraint_checks[]` 校验结论 + 通过 / 驳回）、run 详情区（trace + 审计）。
2. dry-run 可视化：展示将下发的设备指令与预期作业参数，页面与审计中明确标注「未触达真实设备」。
3. `evalkit` 补齐多次运行：每样本 n=3、红线 pass^3、质量指标均值；实现 flaky 清单、`unexpected_call` 清单、`infra_error` / `grader_error` 统计。
4. 用 evalkit 跑 100 条 × 3 次，产出 Phase 0 baseline 报告（红线通过率、引用覆盖率、拒答率、结论一致性人工抽检、P95、token 成本），据实测值确定母文档 §3.2 中 P95 等目标线；baseline 命名为 `baseline_p0`，实验 ID 写入 `eval/baselines/`，附配置快照（`git_sha`、`prompt_version`、`model_tier`、`knowledge_ref`）。
5. 评测负责人阅读全部红线失败与 flaky 样本的 transcript 并签字。
6. 示范农场真人演练支撑：准备演练账号、地块数据与问题清单（四类问题各 2 个），演练中提供 `run_id` 记录；演练由田间验证方执行，本团队负责问题修复。
7. 输出《Phase 0 验收报告》与《Phase 1 启动包》（任务分解 + owner 指派 + D4 / D5 决策结论）。

**交付物**：Web 管家页、dry-run 可视化、baseline 报告、《Phase 0 验收报告》、《Phase 1 启动包》。

**验收**

1. Gate 0 七条（§4）全部通过并留证据。
2. 真人演练反馈成文，问题按「必须改 / 可延后」分类，必须改项在 Gate 0 前闭环。
3. baseline 报告入库，成为 Phase 2 唯一对照基线，附实验配置快照。
4. 母文档 §4.7 表中 P0 各行评测交付全部完成；未交付项列入《Phase 1 启动包》并指定负责人。

**技术实现**

① 流程

1. Web 页：`POST /api/v1/farms/{farm_id}/ask` 提问 → `GET /api/v1/farms/{farm_id}/runs/{run_id}/events`（SSE）；待审批区 `GET /api/v1/farms/{farm_id}/approvals?status=OPEN`（只列 `ticket_kind=ORDER` 的 `OPEN` 票，`PREPARING` 不展示），详情带 `snapshot_digest` 与 `constraint_check` 结论；`HIGH` 票第一人通过后显示「待第二人确认」。
2. SSE 由 `service.EventService.Stream` 实现，`controller` 只转发：先 `SUBSCRIBE sse:run:{run_id}` 并缓冲 → 按 `Last-Event-ID` 从 `run_event` 补读 `seq > last` → 先推补读结果，再推缓冲与后续消息，按 `seq` 去重、丢弃 `seq ≤ 已推送` 的消息；推送中发现 `seq` 断档时再补读一次。
3. outbox 轮询器（server 内 goroutine，每 1s；P1 迁到 `cmd/worker`，SQL 不变）：`FOR UPDATE SKIP LOCKED` 取 `PENDING` 且到期的消息，同事务把 `next_run_at` 推后 60s 作租约 → 按 `topic` 分发；`order.dispatch` 交 `service.WorkOrderService.Dispatch`。基础设施错误按 1s / 5s / 30s / 2min / 10min 退避，5 次后 `DEAD` 并告警。
4. `Dispatch` 按母文档 §4.6.2-3 的顺序执行：
   - ① 读工单：已是终态视为重复投递，置 outbox `DONE`；`DISPATCHED` 见下文崩溃续跑；过 `dispatch_deadline_at` 则同事务 `Transition(APPROVED → EXPIRED)`。
   - ② P0 工单全部是 `DRY_RUN`（`enable_robot_dispatch` 固定关），不读开关；③ 设备在线检查 P1 起做。
   - ④ 锁外做 `PRE_DISPATCH` 复检：原 run `resume_epoch + 1`，`step_id=pre_dispatch` 重取时效数据（不读 `cache:mcp`），写 `constraint_check(PRE_DISPATCH)`（P0 只记录、不拦截硬约束），重跑 `calc`；结论变化 → 同事务 `Transition(APPROVED → CANCELED, reason=CONDITION_CHANGED)`、新建 run、旧 run 写 `superseded_by_run_id`。
   - ⑤ 持 `lock:order:{order_id}`，同事务 `Transition(APPROVED → DISPATCHED)` + `AppendTx(DISPATCHED)`（recordIntent）。
   - ⑥ `adapter.MockAdapter.DryRun(payload)` 返回将下发的指令（进程内调用，无外部 IO）。
   - ⑦ recordResult（同事务）：写 `work_order.dispatch_result` → `Transition(DISPATCHED → DONE)`（跳过 `EXECUTING`）→ outbox `DONE` → 审计 `action=DISPATCH`，summary 标注 `dry_run=true`；不写 `episodic_memory`、不回写 `field.history`。
   - 崩溃后重复投递：工单已 `DISPATCHED` 且无 `dispatch_result` 时从 ⑥ 继续（DryRun 无副作用），已 `DONE` 直接置 outbox `DONE`。
5. `cmd/eval run --n 3`：每样本 3 个 trial → `evalkit/reduce` 计算红线 pass^3 与质量指标均值，输出 flaky / `unexpected_call` / `infra_error` / `grader_error` 清单。
6. 报告与配置快照（`git_sha / prompt_version / model_tier / knowledge_ref`）上传 `agri-steward-eval` 的 `report/` 前缀，`eval/baselines/baseline_p0.yaml` 记录实验 ID 与对象 key。

② SQL（`outbox` 在 `0001_init` 中建表，P1 只用不改）

```sql
CREATE TABLE outbox (                   -- 可变状态表
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id, farm_id,          -- 均 bigint unsigned NOT NULL
  topic varchar(64) NOT NULL,           -- P0：order.dispatch；P1 起增 order.cancel / notify.send / field.history.append 等
  aggregate_id bigint unsigned NOT NULL,                                      -- 一般为 order_id
  dedup_key varchar(128) NOT NULL,      -- {topic}:{order_id}:{ticket_id}:{n}（母文档 §4.6.2-4）
  payload json NOT NULL, status varchar(8) NOT NULL DEFAULT 'PENDING',       -- PENDING / DONE / FAILED / DEAD
  attempts smallint NOT NULL DEFAULT 0, next_run_at datetime(3) NOT NULL, last_error varchar(512) NULL,
  row_ver int NOT NULL DEFAULT 0, created_at datetime(3) NOT NULL, updated_at datetime(3) NOT NULL,
  UNIQUE KEY uk_dedup (dedup_key),
  KEY idx_poll (status, next_run_at),   -- worker 专用，跨租户扫描
  KEY idx_scope (tenant_id, farm_id, topic, created_at));

-- 轮询（同一事务：取消息 + 推后租约）
SELECT id FROM outbox WHERE status='PENDING' AND next_run_at<=? ORDER BY next_run_at LIMIT 50 FOR UPDATE SKIP LOCKED;
UPDATE outbox SET next_run_at=? /* now+60s */, row_ver=row_ver+1, updated_at=? WHERE id IN (?);
-- 处理完成 / 失败退避
UPDATE outbox SET status='DONE', row_ver=row_ver+1, updated_at=? WHERE id=? AND status='PENDING';
UPDATE outbox SET status=IF(attempts+1>=5,'DEAD','PENDING'), attempts=attempts+1, next_run_at=?, last_error=?,
       row_ver=row_ver+1, updated_at=? WHERE id=? AND status='PENDING';   -- MySQL 按书写顺序赋值，status 须写在 attempts 前

-- recordResult（与 Transition(DISPATCHED → DONE)、outbox DONE、AppendTx 同一事务）
UPDATE work_order SET dispatch_result=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='DISPATCHED' AND dispatch_result IS NULL AND row_ver=?;
SELECT seq, event_type, payload FROM run_event
 WHERE tenant_id=? AND farm_id=? AND run_id=? AND seq > ? ORDER BY seq;   -- SSE 补读
```

③ Redis 键：沿用 `sse:run:{run_id}`（L7）、`lock:order:{order_id}`（L1）、`cache:mcp`（L2，复检不读）。outbox 轮询靠 `SKIP LOCKED` 互斥，不另设锁。

## 7. 推荐实施节奏

### 7.1 周排期

| 周 | 管家主线 | 评测同期交付 | 决策 / 外部 |
|----|----------|--------------|-------------|
| 第 1 周 | 工程骨架与配置体系，`enable_*` 开关框架（默认全关）；`0001_init` 建最小链路表（L1 技术实现 1）+ 示范农场地块 seed；静态鉴权中间件；L0 场景冻结、Eino 检查点 spike；L1 契约初稿；最小链路 `runner → policy → mcp-weather（mock）→ callbacks（审计与 trace）` | 样本 YAML 格式、`cmd/eval` 骨架（含 `--n`）、`validate` | D2、D3 闭环；D1 形态拍板；启动 D6 授权、D4 接口梳理 |
| 第 2 周 | 最小链路接入 `mcp-weather` 真实工具 + 断点恢复，验证 harness 接口；L1 契约评审与冻结（含迁移表）；`mcp-gateway` 骨架；`knowledge/` 与 `index.yaml` 格式 + 10 条样例条目；`run_id` 检索与最小看板 | 20 条样例样本；trial schema 池 + 空实现跑通 `make eval`；注入接缝接口 + `app.Build` | D6 闭环；第 2 周末 15 分钟进度同步（契约图 + 真实天气 demo + 10 条知识样例） |
| 第 3 周 | L2 其余三个 MCP Server；L3 知识条目补齐、`progressive_loader`、`calc`；L4 开工（模型接入、Graph 骨架） | `FixtureMCP` + 三类故障样本；`citation_valid` + 两条静态校验 | D1 最晚闭环（L4 开工前） |
| 第 4 周 | L4 主链路完成（转换器、降级、提示词 v1）；L5 护栏与 L6 HITL / 记忆并行开工 | L4 五个打分器；20 条样例由 evalkit 回放打分；`RecordingAdapter` | — |
| 第 5 周 | L5、L6 完成；L7 补全事件类型、`raw_store`、指标与看板 | L5 / L6 打分器；100 条样本入库并全过 `validate`；transcript 导出 | — |
| 第 6 周 | L8 Web 页与 dry-run 可视化；演练支撑与问题修复；Gate 0 收口 | n=3 多次运行与 pass^k；首个 baseline；评测负责人读 transcript 签字 | D4 / D5 结论写入《Phase 1 启动包》 |

评测工作量按评测设计方案 §15：L0 约 1 人 × 1.5 周，L5-L8 约 1 人 × 2 周，L1-L2 随层完成不单列；由 Harness RD 中 1 人兼任评测负责人。

### 7.2 并行与合入规则

1. **先冻结后并行**：L1 契约评审通过前，L2 / L3 只允许写内部实现，不允许依赖未冻结字段对外暴露接口。
2. **一个 PR 只做一件事**：PR 标题带 `Ln` 编号，不跨 route 大面积混改。
3. **功能与验收同 PR**：每层的打分器与样本随功能同 PR 或同周合入（母文档 §4.7-6），打分器由写功能的人写、评测负责人评审。
4. **红线代码双人评审**：`internal/agent/guardrail`、`internal/service` 中审批与 workorder 相关文件、`internal/agent/harness/policy`、`tool_policy.yaml`、`pii` 改动必须两人签署。
5. **专家签字**：`knowledge/` 与 `calc` 公式、提示词中拒答规则的改动，农艺专家签字后才可合入。
6. **依赖方向检查**：`internal/agent` 不得 import `controller` / `service`，`internal/agent/harness` 不得 import 业务包，生产代码不得 import `internal/evalkit`；用 `depguard` 在构建期检查，违反即构建失败。
7. **本地自测**：合入前跑 `make eval`（regression 中已入库样本，n=3，`--n` 自 L0 骨架起可用）与单测；本阶段不进 CI 阻断，CI 阻断在 Phase 2 落地。

## 8. 降级与回滚

| 风险策略 | 失败表现 | 降级 / 回滚路径 |
|----------|----------|-----------------|
| 模型档位或参数变更 | 建议质量下降、超时 | 配置回滚到上一档位，不改代码发版（L4 验收 3） |
| 提示词变更 | 拒答率异常、校验失败率上升 | `prompt_version` 回滚到上一版本 |
| LLM 不可用 | 主链路中断 | 规则层直出保守建议并标注 `degraded=true`；不可降级时明确报不可用 |
| MCP 真实数据源异常 | 超时 / 空 / 异常 | 工具配置切回 `mock`（仅演示环境）；生产路径走拒答，不产生凭空建议 |
| 知识条目错误 | 建议依据错误 | `KnowledgeSource` 回退到上一知识提交号；问题条目下线后对应问题走 `KB_MISS` |
| 记忆注入干扰结论 | 结论被历史偏好带偏 | 关闭记忆注入开关，链路照常工作（L6 验收 4） |
| 建议生成整体异常 | 无依据结论或大面积拒答 | `enable_advice` 关闭，Web 页提示服务暂不可用 |
| 设备下发 | — | 本阶段 `enable_robot_dispatch` 固定关闭，只走 mock Adapter dry-run |

## 9. 角色分工

| 角色 | 人数 | 负责 route |
|------|------|------------|
| 项目 Owner / 架构负责人 | 1 | L1 契约冻结、跨组裁决、Gate 0 判定、D 项推动 |
| Agent Harness RD | 2（其一兼评测负责人） | L1 接缝与 `app.Build`、L4、L5、L6、L7；`evalkit` 框架与打分器评审 |
| 数据与物联网 RD | 1-2 | L2 四个 MCP Server、`mcp-gateway`、`FixtureMCP`、mock Adapter |
| 知识工程 RD | 1 | L3 知识目录、`progressive_loader`、`calc` |
| 前端 | 1（可兼） | L8 Web 管家页 |
| 农艺专家 | 1-2（半投入） | L0 场景与样本签署、L3 知识条目撰写与署名、`calc` 公式核对 |
| QA | 1 | 样本工程化、`validate`、报告解读、恢复与降级演练组织 |

## 10. 阶段验收模板（执行后填写）

1. 功能完成情况：`L0 ~ L8` 逐项勾选，未完成项写原因与去向。
2. Gate 0 七条逐项结论与证据链接。
3. 评测结果：baseline 报告链接、红线 pass^3 结果、flaky / `unexpected_call` / `infra_error` / `grader_error` 统计。
4. 指标快照：会话完成率、工具成功率与 P95、拒答率、校验失败率、恢复成功率、单会话 token 成本；据此确定的 P95 目标线。
5. 演练结果：LLM 断连、MCP 三类故障、进程重启恢复、脱敏、注入违规建议拒收。
6. 真人演练反馈（由田间验证方提供）与「必须改 / 可延后」处理情况。
7. 遗留问题、负责人、关闭时间；红线相关问题不允许延期。
8. 是否进入 Phase 1（是 / 否 + 理由）。

## 11. 本阶段风险与应对

| 风险 | 影响 | 应对 |
|------|------|------|
| D1 / D2 / D3 / D6 未按期闭环 | L0、L3、L4 无法开工或只能 mock | 第 1 周决策会逐项拍板；D6 未闭环时 `mcp-weather` 先 mock，但第 2 周末 demo 前必须真实打通 |
| 专家档期不足，知识条目与样本产出慢 | 红线一无地基，L3 / L0 验收卡住 | 只锁一个作物四类问题；第 2 周用 10 条样例验证模板「填空即可」；样本与知识条目同一位专家连续产出 |
| 契约冻结后频繁变更 | 下游返工，并行失效 | Phase 1 才用的状态与字段本阶段一并定义；变更走版本号与 Owner 批准 |
| Eino ADK 中断 / 恢复、checkpoint 接口与预期不符 | L6 返工 | 第 1-2 周最小链路先验证断点恢复；接口签名以实际使用版本为准，封装在 `internal/agent/harness/checkpoint` 内 |
| 打分器自身有 bug | baseline 失真 | 每个打分器表驱动单测含「应失败」用例；参考解必须全过；评测负责人读 transcript |
| mock 与真实接口不一致 | Phase 1 切 real 时返工 | mock 字段以真实接口文档为准；维护 mock/real 实现状态清单 |
| 演示压力导致绕过护栏 | 红线失守 | 红线不参与权衡（母文档 §11-6）；Web 页演示同样走审批闸门 |

## 12. 下一阶段交接（交给 Phase 1）

1. 冻结的五个契约 schema（v1）、状态机图与合法迁移表（含 Phase 1 用到的全部状态）。
2. Phase 0 baseline 报告（`baseline_p0`）与 `eval/baselines/` 中的实验 ID、配置快照；据实测确定的 P95 等目标线。
3. `regression` 套件 100 条样本 + 参考解；`evalkit` 现有打分器清单与单测。
4. mock/real 实现状态清单：每个 MCP 工具与设备 Adapter 当前是 mock 还是 real、真实接口文档位置、缺口。
5. `tool_policy.yaml` v1 与 `actuate` 工具清单（Phase 1 真实 Adapter 按此注册）。
6. `DeviceAdapter` 接口与 mock Adapter 实现；「审批事务写 `outbox(order.dispatch)` → server 内轮询器 → `WorkOrderService.Dispatch` → mock Adapter」已在 dry-run 中贯通，Phase 1 把轮询器迁到 `cmd/worker`、接真实 Adapter，SQL 与下发顺序不变。
7. P0 定版表结构（§6 各 Ln 的 SQL，18 张表）与 `event_type` 枚举表：Phase 1 起对这些表只加列、加索引，不改名、不删列，写入 SQL 的列名以此为准。
8. 《Phase 0 验收报告》：Gate 0 证据、遗留问题与负责人。
9. 《Phase 1 启动包》：Phase 1 任务分解到 route 级、owner 指派、D4（设备接口）与 D5（责任与保险）决策结论、D1 法务正式结论。

## 13. 维护规则

1. 本文随 Phase 0 执行持续更新：route 拆分、周排期、owner 变化当周回写。
2. 与母文档冲突时以母文档为准；本文新增的契约、门禁、范围变更须 Owner 批准并同步回母文档。
3. 术语以母文档 §4.5 统一口径为准，不引入同义词。
4. 任何新增优化策略须同时给出 baseline、candidate、指标、评测集、开关或回滚路径，缺一项不进入排期（母文档 §1-5）。
5. Gate 0 未通过不进入 Phase 1，整改结论写入风险清单（母文档 §11-5）。
6. §6 中 P0 定版表的列名、唯一键变更须 Owner 批准，并在同一 PR 内同步 P1–P4 文档中引用该列的 SQL。
