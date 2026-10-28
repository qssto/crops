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
