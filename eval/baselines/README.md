# eval/baselines

## baseline_p0（P1 对照基线）

P1 各阶段的 `regression` 对照一律使用 `baseline_p0.yaml`，不重跑、不覆盖（P1 手册 §6.1 功能任务 1）。

| 字段 | 值 |
|------|----|
| exp_id | 28 |
| git_sha | be76c20 |
| prompt_version | v1 |
| model_tier | standard |
| knowledge_ref | WORKTREE |
| 模型 / n | stub / 3（108 样本、324 trial） |

对照方式：`make eval GO=go N=5` 跑 `regression` 全量，按样本比对红线结果；不要传 `BASELINE=`，否则会写入新基线。

已知限制：baseline_p0 基于 stub 模型，真实模型基线待 Phase 1 任务 4 重跑后另行登记，不替换本文件。

## baseline_p1_exit（P1 收口基线，待登记）

调度方在 P1 收口时于主仓登记，L8 只约定格式，本目录暂无该文件：

```bash
make eval-nightly GO=go BASELINE=baseline_p1_exit   # = make eval SUITE=regression,redteam N=5 BASELINE=baseline_p1_exit
```

- yaml 字段与 baseline_p0 相同（`report.PublishBaseline` 生成）：`suites` 为 `[regression, redteam]`，`n` 为 5，`gate` 仍是 Gate 0 自动项那一行。
- redteam pass^5 不进 yaml，看 `report_key` 指向的 report.md「按套件红线 pass^k」段落；§6.9 验收 1 要求 redteam 一行 pass^5 = 1.00。
- 登记后 baseline_p0 保留不动，P1 之后的对照改用 baseline_p1_exit。
