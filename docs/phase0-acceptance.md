# 《Phase 0 验收报告》（模板）

依据：P0 实施路线 §6.9 功能任务 7、验收 1-4。

填写规则：
- 「____」是自动项占位，由调度方在连库运行后填写。
- 「待签字」「待演练」是人工项，签字或演练完成前保持原样。
- 不得预填签字或演练结论。

## 1. 基本信息

| 项 | 内容 |
|----|------|
| 版本（git_sha） | `be76c20` |
| baseline 实验 | `baseline_p0`，exp_id = 28 |
| 报告对象 | `agri-steward-eval/report/{exp_id}/report.md`、`snapshot.json` |
| 配置快照 | prompt_version v1 / model_tier standard / knowledge_ref WORKTREE |
| 模型 | 桩模型（`model: stub`），真实模型未验证（ONEAPI_* 未设置） |
| 报告日期 | 2026-10-29 |

## 2. Gate 0 七条（证据索引见 `docs/gate0.md`）

| # | 标准 | 结论 | 证据 | 签字 |
|---|------|------|------|------|
| 1 | 建议必带依据或明确拒答 | 通过（pass^3 = 100%，桩模型） | 报告「Gate 0 自动项」 | — |
| 2 | 设备动作 100% 被拦下转审批 | 通过（pass^3 = 100%，桩模型；无 device.* 调用） | 报告 + unexpected_call 清单（只读工具未声明，见 `docs/gate0.md`） | — |
| 3 | 审批中断后 resume 成功率 ≥ 95%，且不重复执行 | 百分比未统计；HITL 集成测试连库通过，单测证明零重复 | 集成测试报告 | — |
| 4 | run 可从审计和 trace 完整还原 | 待签字 | `docs/gate0.md`「#4 抽查记录」 | 待签字 |
| 5 | 脱敏用例全通过 | 通过（pass^3 = 100%，桩模型） | 报告「Gate 0 自动项」 | — |
| 6 | 100 条 × 3 次 baseline | 108 / 324 | `eval/baselines/baseline_p0.yaml` | 评测负责人：待签字 |
| 7 | 示范农场真人端到端演练与书面反馈 | 待演练 | `docs/drill/feedback-*.md` | 田间验证方：待签字 |

## 3. baseline 指标（§6.9 功能任务 4，不设达标线）

| 指标 | 值 | 来源 |
|------|----|------|
| 红线通过率（pass^3） | 100%（样本均值 1.0000） | 报告「baseline 指标」 |
| 引用覆盖率 | 100%（225 / 225） | 同上 |
| 拒答率 | 30.56%（99 / 324） | 同上 |
| 结论一致性（人工抽检） | 待签字 | 评测负责人抽检记录 |
| P95 延迟 | 164.648 ms | 同上（桩模型下不代表真实模型） |
| token 成本 | 0 | 同上（桩模型不计 token） |
| flaky 样本数 | 0 | 报告「flaky 清单」 |
| infra_error / grader_error | 0 / 0 | 报告「infra_error / grader_error」；有 grader_error 时实验作废 |

P95 等目标线（母文档 §3.2）：要用真实模型的实测值来确定。桩模型数值不作为目标线依据，确定时机见《Phase 1 启动包》。

## 4. 真人演练（验收 2）

- 演练记录：待演练
- 必须改项：待演练（Gate 0 前须全部闭环）
- 可延后项：已列入《Phase 1 启动包》第 3 节

## 5. §4.7 P0 评测交付核对（验收 4）

| 层 | 交付 | 状态 |
|----|------|------|
| L0 | 样本格式、静态校验、`cmd/eval` 骨架、100 条样本 | 已交付（代码入库，exp 28 运行通过） |
| L1 | 注入接缝、共用 `app.Build` | 已交付（代码入库，exp 28 运行通过） |
| L2 | FixtureMCP 与故障注入 | 已交付（代码入库，exp 28 运行通过） |
| L3 | `citation_valid`，以及 must_cite / product_in 的加载期校验 | 已交付（代码入库，exp 28 运行通过） |
| L4 | schema_valid / numbers_match_calc / product_in_candidates / outcome_match / must_call / budget | 已交付（代码入库，exp 28 运行通过） |
| L5 | no_model_actuate / stop_at_approval / pii_clean / refusal_correct / kb_miss_refuse / RecordingAdapter | 已交付（代码入库，exp 28 运行通过） |
| L6 | 脚本审批人、FrozenClock.Advance、state_machine_legal | 已交付（代码入库，exp 28 运行通过） |
| L7 | transcript 导出，报告链接到 run_id | 已交付（代码入库，exp 28 运行通过） |
| L8 | n=3 与 pass^k（reduce）、Gate 0 门禁行、baseline_p0 | 已交付（代码入库，exp 28 运行通过） |

未交付项转入《Phase 1 启动包》，并指定负责人。

## 6. 签字

- 项目负责人：待签字　　日期：待签字
- 评测负责人：待签字　　日期：待签字
- 田间验证方：待签字　　日期：待签字
