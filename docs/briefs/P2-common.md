# P2 子 agent 公共简报补充（在 `docs/agent-brief-common.md`、`docs/briefs/P1-common.md` 基础上，冲突以本文为准）

## 文档
- P2 执行手册（只读）：`智能农作物Agent管家-P2-质量优化期实施路线.md`。各 Ln 的「技术实现」流程 / SQL / Redis 键是定版，照抄，不擅自改名。
- 评测设计：`智能农作物Agent管家-评测Harness设计方案.md`（§7.3 judge、§8 多次运行与配对、§9.2 模拟用户、§10 套件、§11 指标、§12 实验记录）。
- P0/P1 已完成的接口与偏差在 `docs/dev-log.md`；P1 收口状态见 `docs/p1/Phase2启动包.md`（尤其 §1、§5 遗留）。

## 已定决策（用户已批准，不要改）
- 模型：ONEAPI 未提供，全部评测用桩模型（stub）。需要真实模型的验收（judge 实际打分、Kappa 达标、成本 / P95 实测）只做到「代码 + 录制回放 / 构造数据测试」，标「待真实模型」。
- 专家：没有专家。新样本作者写 `claude-draft`、`review_status: pending_expert`；专家标注用合成数据跑通流程，标「待专家」。
- 线上数据：没有示范农场线上 run_id。线上侧任务用合成数据集成测试验证，标「待线上数据」。
- 时间跨度（连续 N 周 / 工作日、月度）：用注入时钟生成多期验证逻辑，真实周期标「待运行」。
- 不加任何新第三方依赖；统计（Wilson、bootstrap、加权 Kappa）用标准库自写，随机数用带 seed 的 `math/rand/v2`。
- CI：GitHub Actions（`.github/workflows/`），本地用同样的 make 目标验证。
- holdout：仓库外独立 git 仓 `~/Desktop/crops-holdout`，以 submodule 挂到 `eval/suites/holdout/`（L2 负责）。
- `baseline_p1` = P1 的 `baseline_p1_exit`（exp 102，stub），补录快照后冻结，不重跑。

## 迁移编号（预分配，只用分给你的）
- 评测库 `migrations/eval/`：`0002_experiment_p2`（L0，调度方已写好，在主仓 migrations/eval/）、`0003_experiment_gate`（L7-a）、`0004_review_task`（L2）。
- 业务库 `migrations/`：`0012_quality_metric_daily`（L3）、`0013_rule_shadow_diff`（L4）、`0014_agent_run_p2`（第 4 批基线）、`0015_kb_load_item`（L5）、`0016_feedback_attribution`（L8）。

## 合入规则摘要（P2 §7.2）
- 一个提交 / PR 只含一个因子；L4 / L5 / L6 的 candidate 先以开关关闭状态合入（行为不变、回归全绿）。
- 红线代码（`output_validator` 拒收规则、`calc` 下沉项拒收、`compaction` 白名单、门禁判定）必须有应失败测试，调度方另开审查。
- 不得放松任何红线规则；`citation_valid`、`numbers_match_calc` 必须保持 100%。
- 样本只改元数据（tags、分层字段、suite 目录）时不升 `version`，否则会打断与 baseline 的配对；改了 expect / fixtures / question 才升 version。

## 评测与验证
- `make eval` 由调度方在主仓跑；worktree 里只跑 `cmd/eval` 集成测试里的小规模评测。
- worktree 没有 deploy/.env 时集成测试跑不了，就保证 `go vet -tags integration ./...` 通过，并在回报里列出需调度方在主仓跑的集成测试名。
- 提交前缀：worktree 分支上可以中文 commit（`P2-Ln：…`），不动 main、不 push。
