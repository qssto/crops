# 评测样本格式（P0）

样本按《评测 Harness 设计方案》§5 裁剪到 P0 实际需要的字段。一个文件一个样本，路径 `eval/suites/{suite}/{id}.yaml`，`suite` 必须与所在目录同名。解析是严格模式，写错字段名会直接报错。

```bash
make validate                      # 等价于 go run ./cmd/eval validate，只做静态校验，不装配 agent
make eval                          # 等价于 go run ./cmd/eval run --suite regression --n 3
make eval TAGS=L2 N=1              # 只回放带 L2 tag 的样本，每样本 1 次
```

## 字段

| 字段 | 必填 | 说明 |
|------|------|------|
| `id` | 是 | 全局唯一，小写 kebab-case，如 `rice-pest-0001` |
| `version` | 是 | 样本修订号，从 1 开始；改了期望就加 1 |
| `suite` | 是 | `regression` / `capability` / `redteam` / `holdout`（P0 只用 regression） |
| `difficulty` | 否 | `easy` / `medium` / `hard`，报告覆盖统计用 |
| `tags` | 是 | 至少一个 `Ln`（`L0`–`L8`，表示该样本回放哪一层的能力）和一个 taxonomy code（`eval/taxonomy.yaml` 中的一级或子类 code）；不允许其他自由 tag |
| `question` | 是 | 用户原话（P0 单轮） |
| `context.farm` / `context.field` | 是 | 合成编码，如 `farm-demo-01` / `field-003`；L1 起评测播种时写入 `field_profile.ext_field_code`，实际 id 为 trial schema 自增 bigint |
| `context.role` | 是 | `farm_manager` / `agronomist` / `operator` |
| `context.user` | 是 | 角色用户编码，如 `u-fm-01`（静态身份配置，L1） |
| `fixtures.clock` | 是 | RFC3339，`FrozenClock` 冻结时刻（L1） |
| `fixtures.mcp` | 否 | `工具名 → [桩返回]`，`FixtureMCP` 使用（L2）。每条可写 `match`（入参匹配）、`return`（单次返回）、`returns`（按调用次序返回）、`error`（`timeout` / `empty` / `malformed` 故障注入）；未声明的调用记 `unexpected_call` |
| `fixtures.memory` | 否 | 记忆注入（L6） |
| `fixtures.approver` | 否 | 脚本审批人：`approve` / `reject` / `timeout`（L5 / L6） |
| `knowledge_ref` | 是 | 知识库 git 提交号 `kb@<sha>`；L3 前填 `kb@TBD` |
| `expect.outcome` | 是 | `ADVICE` / `REFUSED`（与 `advice_card.outcome` 同值） |
| `expect.refuse_reason` | 拒答时必填 | `KB_MISS`（知识未覆盖）/ `DATA_MISSING`（工具无数据、超时、异常）/ `OUT_OF_SCOPE`（超出作物、区域或农场范围） |
| `expect.advice` | 建议时必填 | `category`（一级 taxonomy code，须与 tags 一致）、`advice_type`（`作业类` / `观察类`）、`conclusion`（标准结论，供后续 judge 比对）、`must_cite`（非空，`kb:<path>@<version>`）、`product_in`（可接受药剂 / 肥料）、`numbers[]`（`field` / `value` / `unit` / `precision`） |
| `expect.work_order.final_status` | `stop_at_approval` 时必填 | 期望作业单终态，如 `PENDING_APPROVAL` |
| `expect.trajectory` | 否 | `must_call`（superset 语义）、`must_not_call`（支持 `device.*` 通配）、`stop_at_approval`（必须停在审批）、`budget.max_steps` / `budget.max_tokens` |
| `reference_solution` | 否 | 参考解路径 `refs/<id>.json`（L4 起补，证明样本可解、打分器正确） |
| `author` | 是 | 署名。Claude 草拟的样本一律写 `claude-draft`，不得冒充专家 |
| `review_status` | 是 | `pending_expert` / `signed`；`author: claude-draft` 只能是 `pending_expert`，专家签署时同时改 `author` 为本人署名 |
| `signed_at` | 是 | `YYYY-MM-DD` |
| `notes` | 否 | 样本意图、负样本原因等 |

## 静态校验（`cmd/eval validate`）

规则列表在 `internal/evalkit/sample/rules.go`，失败时列出全部违规并在最后一行输出 `failed sample ids: ...`，退出码 1。

1. `required`：上表必填项、id 格式、clock 格式。
2. `attribution`：`author` / `signed_at` / `review_status` 齐全，`claude-draft` 不得标 `signed`。
3. `enums`：suite、role、outcome、refuse_reason、advice_type、approver、故障类型、作业单状态取值合法；suite 与目录一致。
4. `expect_shape`：outcome 与 `refuse_reason` / `advice` / `work_order` 的搭配一致；建议必须有 `must_cite`。
5. `tags`：只允许 `Ln` 与 taxonomy code，且两类各至少一个；`advice.category` 须出现在 tags 的一级分类中。
6. `unique_id`：id 全局唯一。
7. `refuse_ratio`：每个套件应拒答样本 ≥ 20%。

## 占位与回填

当前 20 条样本全部为 `claude-draft / pending_expert`，结构完整但以下内容是占位，L3 / L4 回填：

- `knowledge_ref: kb@TBD`、`must_cite` 中的 `@TBD` 版本：L3 知识库入库后回填真实提交号与 `kb_id@version`，并由 L3 新增的 `must_cite` 存在性校验约束。
- `product_in: [TBD-pesticide]` / `[TBD-fertilizer]`：L3 `pesticide_registry.yaml` 就绪后回填，由 `product_in ⊆ calc 候选集合` 校验约束。
- `numbers[].value: null`：L3 `calc` 就绪后由专家手算核对并回填。
- 结论文本带「（草拟，待专家确认）」：专家签署时改写。
