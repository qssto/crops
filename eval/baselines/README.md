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
