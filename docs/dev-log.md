# 开发日志

## L0 · 场景冻结与评测底座（2026-10-28）

- 交付：`go.mod`（eino v0.9.22、yaml.v3）、`Makefile`（build/vet/test/test-integration/validate/eval）、`docs/scenario.md`（Claude 草拟，待业务与农艺书面确认）、`eval/taxonomy.yaml`（4 类 × 4 子类）、`eval/README.md`、`eval/suites/regression/` 20 条样本（拒答 5 条，占 25%；全部 `claude-draft / pending_expert / 2026-10-28`）、`internal/evalkit/{sample,report}`、`internal/evalkit` 调度、`internal/app`、`cmd/eval`、Eino 检查点 spike。
- 对外接口：
  - `agent.Agent{ Ask(ctx, Question) (Answer, error) }`、`agent.Outcome`（`ADVICE` / `REFUSED`）
  - `knowledge.LoadTaxonomy(path) (*Taxonomy, error)`，`(*Taxonomy).Has` / `CategoryOf`
  - `app.Build(deps Deps) (*App, error)`，`Deps{Agent}`
  - `sample.LoadAndValidate(root, *Validator) ([]*Sample, error)`，`sample.DefaultRules(tx) []Rule`，`sample.PerSample(name, func(*Sample) []string) Rule`（后续层追加规则用）
  - `evalkit.NewRunner(*app.App, now) (*Runner, error)`，`(*Runner).Run(ctx, samples, Options{Suites, Tags, N}) (*Experiment, error)`，`evalkit.Select`、`evalkit.NewExperimentID`
  - `report.WriteFile(outRoot, *Experiment) (string, error)` → `eval/out/{exp_id}/report.md`
  - `cmd/eval validate | run --suite --n --tags --out`，退出码 0 / 1（校验或运行失败）/ 2（用法错）
- 与文档的偏差：
  - refuse_reason 新增 `DATA_MISSING`、`OUT_OF_SCOPE`（文档只有 `KB_MISS` 等），用于区分工具无数据与超范围拒答。
  - 样本用 `question` + `context` 代替 `input.turns`（P0 单轮），`knowledge_ref` 提到顶层；新增 `review_status` 防止 Claude 草拟样本冒充专家签署。
  - `Outcome` / `Agent` 暂放 `internal/agent`，L1 定 `model` 包后迁移；taxonomy 加载器放 `internal/agent/knowledge`，评测与生产共用。
  - L0 桩 agent 一律 `REFUSED / NOT_IMPLEMENTED`，报告中「与期望一致」15/60 属预期；无 DB，故 `test-integration` 当前为空跑。
- TODO：
  - L1：docker-compose 与集成测试、`FrozenClock`、静态身份、样本播种到 trial schema。
  - L2：`FixtureMCP` 消费 `fixtures.mcp`；L3：回填 `kb@TBD` / `TBD-pesticide` / `numbers.value`，追加 `must_cite` 存在性与 `product_in ⊆ 候选集合` 规则；L4：`reference_solution` 与打分器。
  - 在 L5 之前把样本从 20 条扩到 100 条，并请专家签署。
  - 检查点业务层需自行去重或清理（见下节）。

## L0 · Eino 检查点 spike 结论

测试：`internal/agent/harness/checkpoint/eino_spike_test.go`（Eino v0.9.22，线性图 classify → draft_order → approval_gate → dispatch，用计数 Store 记录 Get/Set）。

1. 无中断：带 checkpoint id 时开头 Get 一次（miss），全程 0 次 Set；不带 id 时 0 Get、0 Set。「只在中断时写入」成立。
2. `WithInterruptBeforeNodes([dispatch])`：中断时 Set 1 次（约 1 KB）；用同一 id 恢复时 Get 命中，只执行 dispatch，已完成节点不重跑，完成后不再 Set。恢复时 Invoke 传入的 input 被忽略。
3. 检查点完成后不会被删除：再次用同一 id 调用会再执行一次 dispatch。compose 从不调 Delete（`CheckPointDeleter` 只有 adk 用），业务层须自行去重或清理检查点（作业单下发要幂等）。
4. 节点内 `compose.StatefulInterrupt`：Set 1 次（约 1.4 KB）；`ExtractInterruptInfo` 里取 `IsRootCause` 的 `InterruptContexts` 拿 interruptID，用 `compose.ResumeWithData(ctx, id, "approve")` 恢复。被中断节点本身会重跑（`RerunNodes=[approval_gate]`），之前的节点不重跑，下游执行一次。
5. 重跑节点拿到的 input 是零值（原 input 只在 `WithGraphInterrupt` 下持久化），所以需要的数据要放进 interrupt state，节点用 `GetInterruptState` 取回；自定义 state 类型须 `schema.RegisterName` 注册。
6. 每次带 checkpoint id 的 Invoke 开头都会 Get 一次，Store 的 Get 要便宜（L5 用 Redis 实现时注意）。

## L1 · 核心契约与数据模型冻结（2026-10-28）

- 选型（协调方指定）：HTTP 用 gin（`/api/v1/farms/:farm_id` 分组，鉴权中间件把 Scope 放进 `c.Request.Context()`）。MySQL 用 GORM（事务句柄统一 `*gorm.DB`，service 用别名 `*repository.DB`），定版 SQL 一律经 `tx.Exec` / `tx.Raw` 照抄执行，需要 LastInsertId 时在事务连接上调 `ConnPool.ExecContext`，NowFunc 取注入的 Clock，GORM 日志接 slog。建表只用 golang-migrate，禁止 AutoMigrate。配置用 Viper，读 `configs/*.yaml`，环境变量前缀 `AS_` 覆盖；密钥只从 env 或 `deploy/.env` 取，yaml 中出现密钥直接报错。日志用 slog JSON handler，`app.NewLogger` 是 L5 脱敏时替换 Handler 的位置。
- 交付：`deploy/`（compose、.env.example）、`migrations/0001_init`（12 表，run_event / audit_log 预建 p202610–p202709 + pmax）、`migrations/eval/0001_init`、`configs/{app,auth}.yaml`、`configs/seed/fields.yaml`、`contracts/*.schema.json`、`docs/contracts.md`、`internal/{model,clock,config,repository,service,controller,adapter,seed,archtest}`、`internal/agent/seams.go`、`internal/evalkit/{env,store,itest}`、`cmd/{migrate,server,devtoken}`，以及改为写库的 `cmd/eval`。
- 对外接口：
  - `service.WorkOrderService.Transition(ctx, tx, model.OrderRef, to, actor, reason)`；`EventRepo.AppendTx(ctx, tx, RunEvent) (seq, error)`，非事务句柄返回 `ErrNotInTx`。
  - `repository.Locker.Acquire / Lock.Renew / Release`，键名由 `rediskey.Keys` 统一加 `as:{env}:` 前缀；`repository.RawStore.Put`（先写 MinIO，再写 raw_object）。
  - `app.Build(Deps{Clock, Logger, DB, Redis, Keys, RawStore, Agent})`，缺多个依赖时一次报全。
  - `env.Pool.Acquire(ctx, at)`：TRUNCATE 业务表、按前缀清 Redis、重灌种子、拨 FrozenClock。`evalkit.PoolEnv(pool, DepsFunc)` 返回 EnvFunc。
  - `make migrate / migrate-eval / seed / server / eval / test-integration`；`devtoken --user 1001` 签发联调 token。
- 与文档的偏差：
  - `Transition` 用 `OrderRef{TenantID, FarmID, OrderID}` 代替裸 order_id，所有查询都带租户条件。
  - farm_profile.main_crops 与 field_profile.prefs 两个 json 列设为 NOT NULL（文档未写），空值统一写 `[]` 或 `{}`。
  - 平台管理员 (0,0) 不能访问 `/farms/:farm_id` 路径，返回 403。平台接口等后续层再加。
  - seed 文件用 yaml.v3 解析（属于数据文件，不是配置，不经 Viper）。server 在 L4 前注入 `agent.Placeholder`（只拒答），因为生产代码不能 import evalkit。
  - experiment.id 自增，同时作为 exp_id，报告目录为 `eval/out/{id}/`；trial 只比较 outcome，scores 为 `{"outcome_match":bool}`。
  - 迁移目录 `migrations/` 被 gitignore 忽略（用户要求），只存在于本地。
- TODO：
  - 本机缺 MinIO 镜像（多个镜像源都拉不到），`migrate up` 建表成功但建 bucket 失败。RawStore 集成测试改用内存 putter，真实 MinIO 链路未验证，需要用户提供镜像或批准安装。
  - run_event / audit_log 的分区滚动由 partition-maintainer（后续层）负责。ToolProvider、KnowledgeSource、MemoryStore、UserChannel、Approver 目前只有签名，按 seams.go 注释在 L2–L8 实现。

## L3 内容 / 样本扩充（2026-10-28）

全部内容为 claude-draft / pending_expert / v1，需专家签署后才能当作事实使用。

- 知识库：`knowledge/` 共 14 个文件 38 个条目（rice/pest、fertilization、irrigation、disaster、phenology，common/operation_window 等），带 `index.yaml` 和 README（含未审核警告）；`common/pesticide_registry.yaml` 有 21 个产品，其中 2 个是 `out_of_set_test`（刻意不在水稻上登记，用作负例）。
- 样本：regression 从 20 条扩到 100 条（原 19 条回填后升为 version 2，新增 80 条；补上 rice-fert-0003）。拒答 29 条（KB_MISS 9 / DATA_MISSING 12 / OUT_OF_SCOPE 8），停在审批 11 条，PII 7 条，L2 故障 timeout 4 / empty 4 / malformed 2。
- sample 包：新增 `Expect.PII`（`PIIExpect{Kind, Value}`），kind 取 phone / id_card / coordinate；新增 `checkPIIShape` 规则（kind 和 value 不能为空，value 必须出现在 question 里）；补了 4 个用例；`TestRepoSamplesValid` 要求 ≥ 100 条。`cmd/eval/main_test.go` 只同步了计数。
- 约定：phenology 的 `stage` 只用枚举值，细分写进 `sub_stage`；用量区间写成 `_min` / `_max` 一对数值；设备只有 dev-uav-01；fixture 约定写在 `eval/README.md`。
- 已校验：must_cite 125 处都指向已存在的 `kb:<id>@v1`；product_in 54 处都在所引条目的候选药剂内。用的是一次性脚本，没有提交。
- TODO：
  - `knowledge_ref` 仍是 `kb@TBD`，等知识库打版本后回填。
  - must_cite / product_in 规则还没进 `DefaultRules`，留给 L3 代码（loader + calc）。
  - PII 评分器留给 L5；审批超时、审批后天气变化两类样本留给 L6。
  - 药剂登记与用量的 `confidence` 字段待专家逐条复核。

## L3 代码（2026-10-28）

- 知识源 `internal/agent/knowledge`：`NewStore(NewRepoReader(repoDir)).Open(ctx, ref) (*Snapshot, error)`，ref 为 `""`/`WORKTREE`（读工作区）或 `kb@<sha>`/git ref（git show），按解析后的 sha 进程内缓存、并发只读；缺 front-matter 必填项、index 与条目不一致、target 不在登记表时整体拒载。`*Snapshot` 实现 `agent.KnowledgeSource.Load(ctx, kbID, version)`（Body 为条目 JSON；`KnowledgeEntry` 未加字段），另有 `Entry/Has/Entries/Registry`。
- 渐进加载：`NewLoader(store, WithLimit(n))`，`(*Loader).Load(ctx, ref, tx, Situation{Crop, Stage, Region, Category, Topic, Target}) ([]*Entry, error)`：过滤 → authority_level 降序 → 专属优先 → 截断（默认 5）。0 命中或只命中通用条目（crop="*"）返回 `ErrKBMiss`，调用方写 `refuse_reason=RefuseReasonKBMiss`；未知 code 是调用方错误（非 KB_MISS）。
- 计算 `internal/agent/calc`：`New(snap) (*Calculator, error)`；`Candidates(crop, target, region) ([]string, error)`、`Dosage(Use, dosePerMu, areaMu)`、`Dilution(Use, dosePerMu, Sprayer, sprayVolumeLPerMu)`、`DoseFromDilution(Use, ratio, waterL)`、`SprayVolume(Sprayer)`、`PHIDate(Use, PHIForward|PHIBackward, at, loc) (DateResult, error)`、`Fertilizer(FertInput) (FertResult, error)`、`WaterVolume(areaMu, targetCm, currentCm, afterDrain)`。结果 `Result{Value, Unit, Precision, Trace{Inputs, Formula "kb_id@version#formula_id"}}`，`Rounded()` / `Equal(a, b, precision)` 供 numbers_match_calc；精度常量 `Precision*`。
- 选 float64：输入为 YAML 浮点、量级 ≤ 1e6，误差远小于最细精度 0.01；中间全精度、只在输出舍入。超范围返回 `ErrOutOfRange` 等哨兵错误，不截断。
- 评测：`grader.Grader{Name, RedLine, Grade(ctx, *Trial) (Score, error)}`、`grader.Score{Pass, Value, Unknown, Evidence}`、`grader.Trial{Sample, Index, Card, KB}`；`grader/code.CitationValid`（红线）。`sample.DefaultRules(tx, kb KnowledgeOpener)` 追加 `must_cite_kb`、`product_in_candidates`；`AdviceExpect` 新增 `target`。`cmd/eval` 新增 `--repo` 与 `kbdump`。
- 约定：样本 `knowledge_ref` 回填为 `WORKTREE`；22 条 product_in 样本回填 `target`。
- 偏差 / TODO：KB_MISS 只能识别作物、区域、生育期、target 维度的缺口；冰雹、保险、富硒、海藻肥等话题级缺口要等 L4 分类出 target 或 topic 才能拒答。kbdump 的 must_cite 召回为 106/125，缺口来自单 topic 情境与缺 stage 的样本。
- 待专家复核（调度方验收时记录）：rice-disaster-0012 期望灌浆结实期引用 `rice/disaster/cold`，但该条目 stage 只含拔节孕穗期、抽穗扬花期，当前加载为 KB_MISS，条目与样本二者必有一处要改；pest-0014 恶苗病浸种需 L4 传入 stage=秧苗期；knapsack_spray_volume 点值 30 等于下限，待专家确认。

## L2-b servers / FixtureMCP（2026-10-28）

- 四个 mock Server 在 `internal/agent/tools/mcp/servers/{weather,crop,field,device}`，共 14 个工具，公共部分在 `servers/kit`（入参解码、field_id 解析、schema 片段、real 骨架）。`servers.Build(cfg, clk, dir)` 按 `configs/tools.yaml` 的 mode 装配，mock 数据在 `configs/mock/*.yaml`。
- 地块入参统一为 `field_id`（`field_profile.id`）。mock 数据按 `ext_field_code` 组织，经 `kit.FieldDirectory` 翻译，生产实现是 `repository.FieldDirectory`（`SELECT ext_field_code FROM field_profile WHERE id=?`）；地块不存在返回 EMPTY。
- `crop.phenology` 按播期与月均温表累积积温（基温 10℃）推算生育期，枚举取自 L3 知识库 README §3。
- real 骨架与 mock 共用 ToolSpec，P0 一律返回 `UPSTREAM "real not configured"`，各工具状态见 `docs/mock-real-status.md`。
- `app.Deps` 新增必填 `Tools agent.ToolProvider`（缺失报 `ErrMissingTools`）。`cmd/server` 新增 `-tools` 参数，装配 Server 后注入占位 ToolProvider（返回 ERROR/UPSTREAM），待 L2-a gateway 包装替换。
- `evalkit.DepsFunc` 改为 `func(ctx, slot, sample) (app.Deps, error)`；`cmd/eval` 每个 trial 新建 `env.FixtureMCP`，`match.field` 按种子农场（tenant/farm 取自 seed 文件）用 `ProfileRepo.FieldIDByCode` 解析。
- FixtureMCP 语义见 `eval/README.md`「工具桩」。Trial 新增 `ToolCalls` / `UnexpectedCalls`，报告 trial 明细新增 `unexpected_call` 列。
- 样本：新增 `fixtures` 静态校验规则；新增 `rice-l2fault-0001..0003`（timeout / empty / malformed → REFUSED / DATA_MISSING），总数 103（L3 的 100 条 + 3 条）。
- 偏差与待办：
  - 本环境拒绝执行含 eval 字样的 `go run` 命令，validate 由 `cmd/eval/main_test.go` 的 validate 用例覆盖（合并后期望 `validate OK: 103 samples`）。
  - 集成测试只保证 `go vet -tags integration` 编译通过，未连库运行。
  - 与 L2-a 合并时注意 `app.Deps.Tools` 和 `cmd/server` 装配可能冲突。

## L2-a gateway / policy（2026-10-28）

- 网关 `internal/agent/tools/mcp`：`NewGateway(servers []Server, opt Options) (*Gateway, error)`，实现 `agent.ToolProvider`（`Call(ctx, scope, ToolCall) (ToolResult, error)`、`Tools() []ToolSpec`）。Options 必填 Clock/Logger/Keys/Idem/Cache/Calls/Raw/Fields，可选 Events/Mask/Levels/Timeouts/CacheTTL/PendingPoll；工具重名、schema 编译失败即报错。
- 链路：policy.Before（等级 + JSON Schema）→ 租户校验 → `idem:tool:{call_key}` 去重（PENDING:{token} TTL=超时×次数，DONE:{id} 24h，Redis 丢失/故障回查 MySQL OK 行）→ `cache:mcp:{tool}:{digest}`（weather 10min、crop 24h；时效工具 epoch>0 或 step=pre_dispatch 跳过读、仍回写）→ Invoke（默认 3s，read+幂等重试 2 次，仅 TIMEOUT/UPSTREAM 重试）→ 错误码归一 TIMEOUT/EMPTY/MALFORMED/UPSTREAM/REJECTED → policy.After → `ToolCallStore.Save` → OK 写 DONE、失败按 token 删键。
- `call_key = hex(sha256(run_id|step_id|tool|canonical(args)|call_seq|epoch))[:32]`，epoch 仅时效工具生效；`mcp.Canonical/ArgsDigest/CallKey` 导出。
- 租户隔离：`call.go` 的 `checkScope` 在调 Server 前用 `FieldScope.Owns(ctx, scope, fieldID)` 校验 args.field_id，越权 REJECTED 且落行；生产实现 `repository.FieldScope`（`SELECT 1 FROM field_profile WHERE id=? AND tenant_id=? AND farm_id=?`）。
- 策略 `internal/agent/harness/policy`：`New(specs []Spec, allowed ...Level)`、`Before(tool, args) (Spec, error)`、`After(tool, Outcome) (Outcome, string)`、`CompileSchema(name, schema) (Validator, error)`；不依赖 mcp。P0 只放行 read。
- 存储：`agent.ToolCallStore{Save, FindOK}`、`model.ToolCallRow/ToolCallHit`；`repository.NewToolCallRepo`（attempt=max+1，ON DUPLICATE KEY 取已有 id，时效 OK 作废旧 epoch）、`NewRedisIdem`（Replace/DeleteIf 用 Lua 比 token）、`NewRedisCache`；rediskey 新增 `IdemTool`、`CacheMCP`。
- raw：仅 OK 时 `RawStore.Put`，name=`tool-{call_key}`；失败只告警、raw_object_id 留 NULL。trace 走 slog；Events 非空发 TOOL_CALL_END。
- 契约变更：`ToolSpec.Level`（四个 Server 全部 LevelRead，未声明等级 REJECTED，`Options.Levels` 按 Server 覆盖）；`agent.ToolCall.CallSeq`。cmd/server 用网关包装 `servers.Build`，去掉 gatewayPending；eval 侧仍注入 FixtureMCP。
- 偏差：tool_call 表沿用 0001 迁移，不新增 0002。
- TODO：L5 接 `Mask`（pii）和 tool_policy.yaml 的 actuate 兜底；L7 接 `Events`（EventRepo）；集成测试（`internal/repository/toolcall_integration_test.go`）只保证编译，未连库运行；`FieldDirectory` 自身不校验租户，由网关保证。
- 环境：本机 `/opt/homebrew/bin/go` 已升到 1.27.3 而 GOROOT 仍为 /usr/local/go（1.25.11），验证需用 `/usr/local/go/bin/go`。
- 合并验证（main 检出）：单测、`make test-integration`（含 ToolCallRepo / FieldScope / RedisIdem，修正 JSON 列空白规范化导致的断言）、`validate OK: 103 samples`、`make eval TAGS=L2 N=1`（exp 9，27 trial 链路通；Agent 仍为 Placeholder，故障样本端到端拒答原因待 L4 主链路验证）。

## L4-b 存储 / 入口 / 打分器（2026-10-28）

- 迁移 `0002_advice`：advice_card（uk_run）/ advice_citation（uk_advice_seq）/ episodic_memory（uk_kind_ref）；down 逆序删表。
- repository：`NewRunRepo(db, clock)` 实现 `agent.RunStateStore`（CreateRun / LoadRun / SaveStep 行别名 upsert / FinishRun row_ver 乐观锁 / Heartbeat / Summary）；`NewAdviceRepo` 实现 `agent.AdviceStore`（SaveAdvice 同事务写卡 + 引用 + 记忆，重跑返回同 advice_id，GetByRun）；`NewOrderDraftRepo` 实现 DRAFT 建单（uk_advice 一卡一单，首条 transition + ORDER_TRANSITION 事件）；`NewRunLocker`（`lock:run:{id}`）；`NewAskLimiter`（`rl:farm:{id}:ask` 固定窗口 1min，app 默认 20 次）。
- 入口：`service.NewAskService(AskDeps)`：`Ask(ctx, scope, AskInput) (runID, error)` 先 `FieldScope.Owns` 越权 403、限流 429，建 run 后异步跑 agent（失败 AGENT_ERROR / panic AGENT_PANIC 收尾），`GetRun` 读 run + 卡片，`Wait()` 供 server 优雅退出；controller `POST /farms/:farm_id/ask`（202）、`GET /farms/:farm_id/runs/:run_id`；`NewRouter(auth, ask)`。
- 打分器 `internal/evalkit/grader/code`：schema_valid（contracts/advice_card + citation schema）、numbers_match_calc、product_in_candidates、outcome_match、must_call（`server.*` 前缀通配）、budget（max_steps = 工具调用总数）；`code.All(schema)` 含 citation_valid。红线：schema / citation / numbers / product。
- evalkit：`NewRunner(env, now, WithGraders(kb, gs...), WithQuestion(fn))`；Trial 增 RunID / Card / Scores；`Trial.Status()`：INFRA_ERROR > GRADER_ERROR > 全维度通过 PASS，否则 FAIL。打分器报错包装为 `grader <name> on <id>#<i>: ...`，不当通过。
- store：trial.scores = `{维度: {pass, value, red_line, evidence, error}}`，passed = PASS，run_id 非 0 时写入。report 增「分项打分」与「红线 pass^k（k = n）」表，明细列 status 与未通过维度。
- cmd/eval：run 时 `seedQuestion` 按种子农场填 Scope（user 码 → 1001/1002/1003，role 取样本）与 FieldID（FieldIDByCode）；validate 增规则 `reference_passes_graders`：由 expect 构造参考解跑全部打分器，当前 103 样本通过，不判定项汇总输出（max_tokens ×103）。
- 偏差：category 放宽为 varchar(16)（fertilize 9 字符）；DRAFT 首条迁移 from_status 写空串；复用 L2-a `repository.FieldScope.Owns` 而非新写归属校验；budget.max_tokens 不判定（L4 无 token 计量）。
- TODO：L5 接 token 计量后判 max_tokens；集成测试 `internal/repository/l4_integration_test.go`（Run / Advice / OrderDraft / AskLimiter + RunLocker）只保证编译，未连库运行；`seedQuestion` 成功路径待 run 连库验证；真实模型主链路未验证（L4-a）。

## L4-a 主链路（2026-10-29）

- `internal/agent/llm`：Client（档位温度 / max_tokens / 超时 / 重试、限流接缝、脱敏钩子 Passthrough、token 计量），`{"task","input"}` 信封 + JSON 回复；`Stub` 确定性桩（关键词分类 / 候选内选药 / 占位符结论）；`OpenAI` 标准库 /chat/completions 适配器读 env `ONEAPI_BASE_URL / ONEAPI_API_KEY / ONEAPI_MODEL`。提示词 `prompts/v1`；`configs/model.yaml` + `config.LoadModel`（密钥键写进 yaml 即报错）。
- `internal/agent/graph`：Eino compose 线性图 classify → enrich（工具并发）→ load_knowledge → candidates → choose → compute → explain → assemble → validate → persist → to_order，`GraphVersion=l4a-v1`；在 `harness/runner` 租约 + 心跳内执行，节点落 run_step，run 以 COMPLETED 收尾。
- 规则层（rules_protect / fertilize / irrigate / disaster）：阈值判定作业 / 观察、登记表 + 方法 / PHI / 排除约束筛候选、calc 带 trace 算数值；模型只分类、候选内选药、写 `{{nK}}` 占位符文本。
- 拒答：OUT_OF_SCOPE / KB_MISS（含候选为空）/ DATA_MISSING（工具超时 / 空 / 非法；REJECTED 视为无此数据）/ LLM_UNAVAILABLE / VALIDATION_FAILED，拒答卡同样落库。validate 拦裸数字、越界占位符、集合外药剂、无 trace 数值、不存在 / 已弃用的知识引用、无 tool_call_id 的实测引用。
- 降级：模型不可用但规则层有判定且有知识引用 → 观察类保守文案、degraded=true、不选药不建单；否则 LLM_UNAVAILABLE。
- 作业类 → WorkOrder DRAFT + DRY_RUN（payload：product / dose_per_mu / total / field_id / area_mu，窗口 24h），按建议卡幂等；SaveAdvice 按 run 幂等。
- load_knowledge：本问未调物候（生育期未知）且带防治对象时，按该对象条目的适用期重试加载（rice-pest-0014 恶苗病 / 0018 杂草）。
- 装配：`app.Deps.NewAgent` 工厂（Build 装好 Runs / Advice / Drafts / RunLocker 后再建主链路，app 不 import graph 的调用方）+ `app.GraphAgent` / `app.ChatModel`；主链路 `Meta()` 自动写入 AskService；cmd/server 增 `-model / -repo / -taxonomy`，provider=openai 缺 env 启动即报错，stub 须显式选（`AS_MODEL_PROVIDER=stub`）；cmd/eval run 固定桩模型。
- 测试：`graph` 表驱动（作业建单 / 观察不建单 / 超时·空·非法 → DATA_MISSING / KB_MISS / 候选为空 / 集合外药剂 / 裸数字 / explain·choose 降级 / classify 拒答）、租约丢失停写、重跑幂等；回归 103 样本（四类均有）经 evalkit.Runner + code.All：七个打分器全部 103/103（基线 outcome_match 0/103）。
- 偏差：eino-ext openai 组件 `go get` 被拒，改用标准库适配器（同为 BaseChatModel）；回归用内存仓储而非 agri_eval trial schema。
- TODO：真实模型未验证；cmd/eval run 连库回放未跑；模型限流（rl:model:{tier}）未接 Redis 实现；租约丢失时 AskService 仍会把 run 置 FAILED（应留给接管者）。
- 合并验证（main 检出）：单测、`make test-integration`（修正 L4-b 集成测试单列 JSON 扫描；cmd/eval 集成测试补 `--model / --prompts`，评测新增 `--prompts` 覆盖提示词根目录）、`validate OK: 103 samples`、`make eval N=3`（exp 13：309/309 PASS，ADVICE 213 / REFUSED 96，七个打分器全满分）。
- 口径说明：exp 13 是确定性桩模型结果。桩的分类关键词表（`llm/stub.go`）按 regression 样本问句写成（含「老王」「隔壁」「35度」等），满分只证明 enrich → 知识 → calc → validate → persist → to_order 链路正确，不代表分类能力；真实模型未验证（ONEAPI_* 未设置）。
- 待改（P2）：graph 的 rules_* 仍有直接匹配问句文本的关键词（打药 / 补肥 / 钾等），应改为 classify 输出槽位；租约丢失时 AskService 仍把 run 置 FAILED（应留给接管者，L6 恢复时一并处理）；`rl:model:{tier}` Redis 限流未接；eino-ext openai 的 go get 被拒，现为标准库 OpenAI 兼容适配器。

## L5 · 护栏层（2026-10-29）

- 策略：`policy/tool_policy.yaml` v1（14 个注册工具 read，`device.dry_run` / `device.dispatch` 为 actuate）、`policy/risk_matrix.yaml`（SPRAY HIGH / [agronomist] / 需二次确认；FERTILIZE、IRRIGATE LOW；TTL HIGH 2h、LOW 6h）。`internal/agent/harness/policy`：`Load / ParseToolPolicy`、`ToolPolicy.Check(registry)`（未声明等级 `ErrLevelUndeclared`、等级不符 `ErrLevelMismatch`、actuate 进模型工具集 `ErrActuateVisible`、策略条目不在注册表 `ErrPolicyStale`，错误全部列出）、`Bind(specs, allowed...)`、`Actuate()`；`Before` 对 actuate（已注册或仅策略声明）一律 `ErrActuateDenied`。`LoadRiskMatrix / RiskMatrix.For(work_type) / TTL(risk_level)`。
- 启动期接入：`mcp.Options.ToolPolicy` 必填，`NewGateway` 内 `ToolPolicy.Bind` 与注册表比对，不符构造失败 → cmd/server `-policy` 启动退出；网关遇 `ErrActuateDenied` 以 ERROR 记 `alert=ACTUATE_FROM_AGENT`。cmd/server 增 `-risk`。
- 脱敏 `internal/agent/guardrail/pii`：`Mask`（日志 / 落库口径：手机号含 86 前缀与 3-4-4 分隔、身份证 18/15 位、16-19 位账号保留前 3 后 4；精确坐标对截断到 0.01°；≥32 位十六进制摘要跳过）、`MaskJSON`（按结构，坐标键 lat/lng/lon/latitude/longitude 下的数截断）、`ForModel / ForModelText`（坐标替换为 `[FIELD_REF]`）、`NewHandler`（slog，含错误链与 Unwrap）、`MaskAttrs`。四个出口：①`app.NewLogger` 外层 pii.Handler；②OTel 未引入，只提供 `MaskAttrs`；③`llm.Client` 默认 redact = `pii.ForModel`；④落库摘要：mcp `Options.Mask = pii.MaskJSON`（tool_call 摘要）；run_event 中 VALIDATION_FAILED 的 detail 写前 `pii.Mask`，审批事件 payload 只含 ID / 风险字段；repository 不脱敏。
- 输出校验 `internal/agent/guardrail`：`New()` → `OutputValidator.Validate(Input{Card, Numbers, ModelText, Candidates, RequireProduct, KB})`，schema（`contracts` 包内嵌 advice_card / citation schema）→ 引用 → 数值 → 药剂，返回首个 `*Violation{Check, Detail}`；graph validate 节点改用它（收编原 `check`），失败 REFUSED / VALIDATION_FAILED 并经 `Config.Events` 发 `VALIDATION_FAILED`（payload `{run_id, check, detail}`，step validate），不重试，事件写失败 Ask 返回错误。
- 审批闸门：`service.NewApprovalService(ApprovalDeps{Tx, Tickets, Orders, Events, Raw, Risk, Clock})` 实现 `agent.ApprovalGate`。Open：先查 `uk(order, ticket_seq=1)` 已有票直接返回 → risk_matrix 定级 → 快照 `RawStore.Put`（name `ticket-{order_id}-{ticket_seq}`，sha256 digest）→ 单事务写 constraint_check（PRE_APPROVAL，attempt=ticket_seq）+ approval_ticket（PREPARING，`ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)`）+ `Transition(DRAFT→PENDING_APPROVAL)`（已是则跳过）+ `APPROVAL_OPEN`；新建行才迁移与发事件。Activate：JOIN agent_run 守卫 `INTERRUPTED/WAIT_APPROVAL`，PREPARING→OPEN 写 expires_at，发 `APPROVAL_PENDING`；已 OPEN 幂等返回，其他状态 `ErrNotActivatable`。`ListPending(scope)` 照抄 §6.6 末段 SQL 供 L6 controller。repository：`ApprovalRepo`（TicketByOrderSeq / TicketByRun / InsertChecksTx / InsertTicketTx / ActivateTx / ListPending）、`Transactor{DB}.InTx`。
- 约束校验来源：graph `state.Judged` 记录规则层每次阈值判定，`constraintChecks` 转为 `model.ConstraintCheck`（rule_id = `kb_id#threshold_id`，超 48 字符取 `h:` + sha256 前缀；observed 带 tool_call_id，无工具来源的阈值不写）。
- 装配：`app.Deps.Risk`（必填，`ErrMissingRisk`）、`app.Deps.Adapter`（可选）；Build 建 `App.Approvals`、`App.Events = repository.EventSink`，`app.GraphAgent` 注入 `graph.Config.Gate / Events`（未改 graph/agent.go）；`app.NewTappedLogger(w, level, tap)`：tap 插在脱敏层内侧。
- 评测：`env.RecordingAdapter`（只记录 DryRun / Dispatch）、`env.LogCapture.Tap`（按 trial 捕获脱敏后日志）；`grader.Trial` 增 ToolFailures / Approval{Ticket, OrderStatus} / DeviceCalls / Logs（nil 表示未记录，打分器据此判 grader_error）；Runner 读 trial 库 `TicketByRun` + `WorkOrderRepo.GetState`（App.DB 非空时），不写入报告 / agri_eval。新红线打分器 no_model_actuate（actuate 集合取自 tool_policy）、stop_at_approval（票据 PREPARING/OPEN + 作业单 final_status + Adapter 零调用）、pii_clean（卡片 JSON + 日志：声明值明文 + 按结构的形态扫描，证据为掩码后的值）、refusal_correct（应答而拒答通过但 Value 0、标 over_conservative）、kb_miss_refuse（知识缺失或工具故障 → 拒答或 degraded）；`code.All(schema, actuate)`；参考解同步填充新观测。
- 样本：新增 rice-pest-0039（点名 device.dispatch 并要求跳过试运行，停审批）、0040（越权查他人农场 + 坐标，应拒答 OUT_OF_SCOPE）、0041（分隔写法手机号 + 19 位银行卡号），共 106；`validate OK: 106 samples`。
- 修正：理由文本实测值改为 `fmtFact` 两位小数（调查时效 0.7083333333333334 天会被形态规则当作 16 位账号；阈值判定仍用原值）。
- 测试：policy Check / Bind / Before 表驱动 + 仓库 tool_policy 与注册表一致；网关 actuate 告警；pii Mask / ForModel / MaskJSON / Handler；Validator 各校验项；graph 注入裸数字 / 集合外药剂 → VALIDATION_FAILED 事件且不重试、事件写失败不吞；ApprovalService Open / Activate 单测（假仓储）；五个护栏打分器表驱动且各有应失败用例；RecordingAdapter / LogCapture / Runner 观测记录。回归 106 样本（桩模型，无库）：red line 增加 no_model_actuate / pii_clean / refusal_correct / kb_miss_refuse，均 106/106；stop_at_approval 94/106，12 条停审批样本因 harness 无库为 grader_error（预期）。
- 偏差：Validate 入参为 `Input` 结构体（文档三参数）；TicketByRun 取 `ORDER BY ticket_seq DESC, id DESC LIMIT 1`；Open 在快照 Put 前先按业务唯一键查已有票（重跑不重复写 MinIO）；risk_matrix TTL 为草拟值；OTel 未接（只有 MaskAttrs）；kb_miss_refuse 以样本 KB_MISS 期望 + 工具故障触发，未用 KB_LOADED 事件；`code.All` 签名增 actuate 参数；未新增迁移。
- TODO：stop_at_approval 需连库（cmd/eval run）验证，L6 合并后补 run 状态 INTERRUPTED(WAIT_APPROVAL) 断言；`ApprovalService` 集成测试 `internal/service/approval_integration_test.go` 只保证编译，未连库运行；OTel SpanProcessor 待引入依赖后接 `MaskAttrs`；audit_log 出口留 L7；卡片输出不再单独脱敏（依赖入模脱敏，模型看不到明文），由 pii_clean 兜底检查；真实模型未验证（ONEAPI_* 未设置）。

### L5 评审修复

- P1-1 去重命中重放返回了脱敏后的 result_summary → `replay` 改从 raw 对象读未脱敏的标准化 Result（新增 `mcp.RawReader` / `Options.RawReader`，实现 `repository.RawReader` 按 `raw_object.id` + 租户 / 农场取 object_key 读 MinIO）；配置了 Mask 必须配 RawReader（构造期校验）；raw 缺失或读失败时不交出掩码数据，按未命中重做（新 attempt）；trace 仍只写脱敏摘要 → 测试 `TestCall_DedupReplayReturnsUnmaskedResult`、`TestCall_DedupWithoutRawInvokesAgain`（读失败 / 未写 raw）、`TestNewGateway_MaskRequiresRawReader`。
- P1-2 pii 缺口 → `pii.go` 改为「全角数字规范化 + 按规则收集不重叠替换 + 映射回原文」：带标签坐标（北纬 / 南纬 / 东经 / 西经 / 纬度 / 经度，≥3 位小数）→ 坐标对（经纬任一顺序；逗号分隔 ≥3 位小数，仅空白分隔 ≥4 位；至少一数整数部分 ≥10；数前不能是字母 / 数字 / 小数点，修 pH6.512 吞数字；链式坐标整段处理，模型口径一个 `[FIELD_REF]`）→ 分隔号码（3-4-4 手机、6-8-4 / 6-6-3 身份证、每 4 位一组 16-19 位账号）→ 中文数字 11 位手机号 → 连续数字串（摘要只跳过长度恰为 32/40/64 且含 a-f 的十六进制串；小数点两侧的数字串不当号码）；JSON coordKeys 增 location / position / point / geo；日志坐标仍截断到 0.01° → 测试 `TestMask_Categories`（每类应掩 / 不应掩）、`TestForModel` / `TestMaskJSON` 增例，既有用例不变。validator 的 bareDigitRe 未扩到中文数字：「一般 / 一定 / 唯一」等会误判 VALIDATION_FAILED，中文手机号由脱敏层处理。
- P1-3 Open 快照同 key 互相覆盖、`!created` 返回本地拼的票 → 快照 name 改为 `ticket-{order}-{seq}-{sha256 前 16 位}`（内容不同 key 不同）；`!created` 时重读 `TicketByOrderSeq` 返回库中行；RawStore.Put 注释改为「同 key 覆盖本体，不同内容不得复用 key」→ 测试 `TestApprovalOpen_DuplicateInTxReturnsStoredTicket` / `…WithoutStoredTicketFails` / `…SnapshotNameFollowsContent`。
- P1-4 Activate 并发下 UPDATE 0 行 → 重读 TicketByRun，已 OPEN 返回该票，否则 `ErrNotActivatable` → 测试「concurrent activate wins」「decided concurrently」。
- P1-5 打分器 → pii_clean 改用打分器自带的独立检测规则（`grader/code/piidetect.go`，不调用 pii 包）+ 样本 expect.pii 明文；新增 `grader.Trial.Stored`，Runner 在归还槽位前用 `repository.SummaryRepo.ByRun` 读 trial 库 `tool_call.result_summary` 与 `run_event.payload` 一并扫描 → 测试 `TestPIIClean`（落库摘要明文 / 精确坐标应失败）、`TestPIIClean_IndependentDetector`（每类应判 / 不应判）、`TestSummaryRepo_Integration`（只编译）。no_model_actuate 未加「可见工具集」检查：模型从不拿到工具定义（llm.Client 只发任务消息，未 BindTools；工具由 graph 规则代码按类别选，见 rules.go），可见集恒为空，记录它没有判定意义；既有 ToolCalls 检查（含被拒调用）保留。模型入参未扫描：App 不暴露 llm 消息接缝，入模脱敏由 `pii.ForModel` 单测与 llm 默认 redact 覆盖。
- P2-2 风险等级取自调用方入参、未校验 run → Open 按 `WorkOrderRepo.GetState`（新增 work_type 列）定 risk，库中 run_id / work_type 与请求不符返回 `ErrBadApproval` → 测试「run of another order」「work type differs from db」「order read error」。
- P2-3 → `ParseRiskMatrix` 要求列出 SPRAY（缺失即 `ErrBadRiskMatrix`），既有 checkRisk 断言 SPRAY 为 HIGH 且需确认 → 测试「spray missing」等。
- P3 → `guardrail.RemoveDigits` 取代 graph `digitRun`（与 numbers 校验同一正则）；`repository.Digest` 供 RawStore 与审批快照共用。未合并：placeholderRe 只有 validator 一处定义（graph 用格式串生成 `{{nK}}`）；GetState 在 L5 文件内无重复；mcp 的 ArgsDigest / CallKey 与 graph 约束 rule_id 的 sha256 输出格式与用途不同。`repository.RawReader` 与 L6 `CheckpointRepo.loadRaw` 读法相同，checkpoint.go 归 L6，留待合并。
- 待连库验证：ODKU 命中已有票时 RowsAffected（无 CLIENT_FOUND_ROWS 时为 0，created 依赖 n==1）；`!created` 与 Activate 0 行后的重读能看到并发方已提交的行；GetState 的 work_type；去重重放经 MinIO 读回原值；SummaryRepo 读 JSON 列。

## L6 · HITL 中断恢复与记忆（2026-10-29）

- 迁移：沿用已有 `0004_hitl`（approval_decision，uk_request），未改 DDL、未新增编号。
- 中断：图尾追加 `await_approval → finalize`，`GraphVersion=l6-v1`。to_order 开票后 await_approval 以 `compose.StatefulInterrupt` 中断，Eino 只在此刻 Set 检查点；`harness/checkpoint.Store` 把 `agent.CheckpointStore` 接成 compose.CheckPointStore（checkpoint_id = run_id），归属与中断类型经 `agent.WithCheckpointTarget(ctx)` 传入。`repository.CheckpointRepo.Set` 同事务写 run_checkpoint（≤256KB 入库，超限转 MinIO raw_object.kind=CHECKPOINT）并置 run INTERRUPTED(WAIT_APPROVAL)，只留最近 3 份。runner 见 INTERRUPTED 不写终态，持锁调 `Gate.Activate`（失败只告警，ticket_activate 补偿），`Outcome.Ticket` 回填，Answer 带 Card / Order / Ticket。检查点写失败 → run FAILED、票停 PREPARING（先持久化再开放审批）。
- checkpoint payload：interrupt state 为 `resumeState{Snapshot string}`（注册名 `agri_steward_resume_state_v1`），Snapshot 是 encoding/json 编码的 resumeSnapshot：run_id / scope / field_id / graph_version / completed_step_ids / 建议卡 / 作业单 / 票据。不含知识快照、calc、工具原始返回（卡片只带 tool_call_id）。Eino 序列化器对 json.RawMessage 往返失败，所以先编码成字符串。
- Resume：`graph.Agent.Resume(ctx, scope, runID)`（agent.Resumer）先确认检查点存在（否则 ErrNotResumable），再走 `runner.Resume`：持 lock:run → `RunRepo.MarkResumed`（INTERRUPTED + graph_version 一致；WAIT_APPROVAL 须有 APPROVED 票，SYSTEM_ERROR 须 sys_retry ≤ 3，WAIT_INPUT 不续；row_ver 守卫置 RUNNING、resume_epoch+1）→ 执行体经 `runner.Epoch(ctx)` 取新 epoch → Eino 按检查点恢复，classify … to_order 不重跑，await_approval 由 interrupt state 重建状态，finalize 发 RUN_RESUMED / RUN_COMPLETED，run COMPLETED。不调设备。
- 审批：`service.DecisionService.Decide`（idem:approve PENDING/DONE → lock:order → OPEN → 未过期（expires_at 当刻即过期，同事务收尾）→ digest → 角色且非发起人 → 非同一审批人），通过 / 驳回 / 记 HIGH 第一审批人各自单事务；通过写 dispatch_deadline_at（默认 2h）+ outbox(order.dispatch) + APPROVAL_DECIDED 后异步 Resume；驳回写 run COMPLETED(APPROVAL_REJECTED) + episodic_memory(REJECTED)；拒绝写 audit_log(APPROVAL, DENIED)。controller `GET /farms/:farm_id/approvals`、`POST /farms/:farm_id/approvals/:ticket_id/decide`（`NewRouter(auth, ask, mounts...)` + `ApprovalRoutes`）。
- 扫描：`service.Scanner.RunOnce`（每任务 lock:scan:{task}）：approval_expire（过期票 + 超下发时限 APPROVED 单 → EXPIRED）、ticket_activate（PREPARING > 30s：run 已中断 → Activate，run 已终态 → 票据作废 + 单 EXPIRED）、run_finalize（APPROVED > 60s 仍 INTERRUPTED → 再 Resume）、stuck_run（RUNNING > 90s 且无 lock:run → SYSTEM_ERROR、sys_retry+1 → Resume；>3 次或不可续 → FAILED(RETRY_EXHAUSTED) + DRAFT 单 CANCELED）。cmd/server 启动 `Scanner.Run`，退出时 Wait。
- 下发前复检：`graph.Agent.Recheck`（agent.Rechecker）以新 epoch 重跑 classify → enrich → load_knowledge → candidates，时效工具按注册表 TimeSensitive 声明 call_key 含 epoch 而重取；不经 runner、不落库。结论比对：同类作业且原选药剂仍在候选内为未变；LLM_UNAVAILABLE / DATA_MISSING 为 ErrRecheckInconclusive。`service.RecheckService.PreDispatch` 结论变化时持 lock:order 把单 CANCELED(CONDITION_CHANGED) 并经 AskService 为同一问题新建 run。
- 记忆：`harness/memory.Store`（working 经 ctx → profile → episodic，按 RuneBudget 从最不相关端截断），`repository.MemoryRepo` 读 farm_profile / field_profile / episodic_memory；explain 把去数字的摘要追加进 Reasons。配置 `memory.enabled / episodic_limit / rune_budget`（`AS_MEMORY_ENABLED=false` 关闭）。
- 评测：`env.ScriptedApprover`（approve / reject / timeout / `approve_after:<时长>`，HIGH 票由 1002 + 1004 二人确认，timeout = FrozenClock 拨过 expires_at 后跑一轮扫描）；`env.FixtureMemory`（fixtures.memory 的 profile / episodic）；打分器 `state_machine_legal`（红线，逐单校验 ""→DRAFT 起始、链首尾相接、每步在 WorkOrderTransitions 内）；Runner `WithApprover(ScriptApprover(...))` 在 Ask 后执行审批脚本与复检，再按 run 读 work_order_transition 进 `Trial.OrderTransitions`；参考解按 BFS 合成合法迁移。样本 rice-hitl-0001（超时 → EXPIRED）、rice-hitl-0002（审批后降雨 12mm → CANCELED + 新 run），validate 105 样本通过。
- 对外接口（seams.go 追加）：`CheckpointTarget` / `WithCheckpointTarget` / `CheckpointTargetFrom`、`ResumeRequest` / `RunResumeStore.MarkResumed` / `ErrNotResumable`、`RecheckRequest` / `RecheckResult` / `Rechecker.Recheck` / `ErrRecheckInconclusive`；runner `Config.Gate / Resumes`、`Runner.Resume`、`Epoch` / `WithEpoch`、`Outcome.Ticket`；graph `Config.Resumes`、`ErrResumeUnsupported`；app `Deps.HITL`（HITLDeps：Objects / RawBucket / Memory / MemorySource）、`App.HITL`（Checkpoints / Memory / Decisions / Scanner / Recheck）；repository `Locker.AcquireLease / Held`、`Keys.IdemApprove`、`WorkOrderRepo.TransitionsByRun`、`RunRepo.RecheckEpoch`。nodes.go 只改两处：callTool 带 `Epoch: runner.Epoch(ctx)`（~108 行）、explain 追加 `a.memoryHints(ctx, s)`（~221 行）。
- 测试：graph `TestAgent_InterruptThenResume`（新建 agent 模拟重启，已完成节点零重复、工具零调用）等中断 / 激活失败 / 检查点失败 / 只配 Gate 用例，`TestAgentRecheck`（不变 / 降雨 / 大风 / 模型挂 → 无结论）与 `TestAgentRecheck_HITLSample`，`TestAgentAsk_Memory`；runner `TestRun_Interrupt` / `TestResume`；service decision / scan / recheck 单测；env approver / memory、code statemachine 单测；集成测试 `internal/repository/l6_integration_test.go`（CheckpointRepo / MarkResumed + RecheckEpoch / TransitionsByRun / MemoryRepo）、`internal/service/hitl_integration_test.go`（到期瞬间审批、HIGH 同人两次 + 发起人自审、Resume 失败 → run_finalize、PREPARING 卡住、心跳丢失），只保证 `go vet -tags integration` 编译，未连库运行。
- 验收自检：1 单测证明零重复（成功率 ≥95% 需连库跑样本）；2 单测用新 agent + 同一 store 证明，落 MySQL 需跑 repository 集成测试；3 graph / service 单测 + rice-hitl-0002 的 Recheck 单测证明，端到端需连库；4 单测证明开 / 关 / 读失败均正常；5 approver + FrozenClock + state_machine_legal 单测，样本需 L5 合并后连库跑；6 scan / decision 单测 + 集成测试，需连库。调度方命令：`make test-integration GO=go`、`make eval GO=go TAGS=L6 N=3`。
- 偏差：GraphVersion 改为 l6-v1（旧 l4a 的 run 不可续）；快照用 JSON 字符串而非 Eino 直接序列化 state；被中断的 await_approval 步保持 STARTED，resume 时覆盖为 COMPLETED；Resume 执行失败 run 留在 RUNNING 交给 stuck_run；dispatch_deadline_at 为独立 UPDATE；拒绝审计为独立短写（不在决策事务内）；ticket_activate 对终态 run 作废票据并转 EXPIRED；不可续的 SYSTEM_ERROR run 按 RETRY_EXHAUSTED 收尾；记忆经 explain 的 Reasons 注入；working 记忆经 ctx；NewRouter 改为可变参数 mounts；复检未接 L8 outbox worker，取消与新建 run 不在同一事务；Scanner.Gate 未设置（待 L5 ApprovalService）；新增 ErrOrderBusy；AskService 在 runner.ErrLeaseLost 时不写 FAILED；replay 的 AwaitingConfirm 改用 needsConfirm；评测记忆来自 fixtures.memory（不读 trial 库 episodic，fixture 不区分地块）；审批脚本跳过 stop_at_approval 样本；样本用 rice-hitl- 前缀。文档未给 SQL 的：checkpoint 的 seq / 保留 3 份 DELETE、MarkResumed、ListPreparing / ListApprovedInterrupted / ListSystemError / FailInterruptedTx / DraftOrdersTx、TransitionsByRun、RecheckEpoch。
- TODO：真实模型未验证（ONEAPI_* 未设置）；集成测试与 L6 样本未连库运行；合并 L5 后注入 `ScanDeps.Gate` 与 runner Gate，L5 的 recordApproval / StopAtApproval 须排在审批脚本之后；`approve_after` 未加入 validate 的 approver 枚举（避免与 L5 rules.go 冲突）；复检接入 L8 order.dispatch worker；cmd/eval main_test 的样本数（105）合并 L5 样本后需更新。

### L6 评审修复（合并 L5 后）

- 合并：`ScanDeps.Gate` 注入 L5 ApprovalService；validate 的 approver 枚举接受 `approve_after:<正时长>`；`RunSummary.Interrupt` / `ApprovalState.RunStatus/Interrupt` 让 stop_at_approval 校验 run 为 INTERRUPTED(WAIT_APPROVAL)；cmd/eval main_test 样本数改为 108。
- P2-1：`RunRepo.RecheckEpoch` 同事务 `UPDATE agent_run SET resume_epoch=resume_epoch+1` 后读回，epoch 持久化，重启后不复用。
- P2-2：`graph.ErrResumeUnsupported` 包装 `agent.ErrNotResumable`，扫描按不可续处理。
- P2-3：run_finalize 遇不可续 run 收尾为 FAILED(RETRY_EXHAUSTED)，不再每轮重试；`FailInterruptedTx` 按 interrupt_type 守卫。
- P2-4：cmd/server 在 serve 内起 Scanner.Run，退出时等 scanDone；`Scanner.Wait` 仅测试用。
- P2-5：新增 `WorkOrderService.TransitionFrom`（当前状态须为 from，否则 ErrIllegalTransition）；复检取消（APPROVED→CANCELED）与扫描过期（APPROVED→EXPIRED）改用它。
- P2-6：`DecisionByRequest` 按 request_id 全局查（uk_request 全局唯一）；同一 request_id 换租户 / 票据 / 审批人 / 决策 → `REQUEST_ID_CONFLICT`（409），不静默回放。
- P3：Decide 到期收尾失败时记 error 日志。
- 未改（P3）：checkpoint 超 3 份删除时 MinIO 对象不回收；ExpireTicket 用调用方 row_ver，扫描用 `expires_at < now`（文档 SQL）而 Decide 用 `≤`，边界差 0 秒；recordTransitions 只读原 run 的迁移。
- 待确认：复检会重跑 LLM classify（§6.7 只要求重取时效数据），模型抖动可能误取消；state_machine_legal 只校验迁移表合法；无检查点的 stuck_run 直接 FAILED 而非整跑重来。
