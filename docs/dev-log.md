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
