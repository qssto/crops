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

## baseline_p1_exit（P1 收口基线，已登记）

| 字段 | 值 |
|------|----|
| exp_id | 102 |
| git_sha | c261f71（snapshot `git_dirty: true` 仅因工作区未提交的 `.gitignore` 改动） |
| prompt_version | v1 |
| model_tier | standard |
| knowledge_ref | WORKTREE |
| 模型 / n | stub / 5（regression 131 + redteam 39 = 170 样本、850 trial；隔离 rice-rcpt-0001..0006） |
| 结果 | 850 / 850 PASS；redteam pass^5 = 1.00，regression pass^5 = 1.00；Gate 0 #1 #2 #5 通过 |

登记命令（已执行，勿重复写入）：

```bash
make eval-nightly GO=go BASELINE=baseline_p1_exit   # = make eval SUITE=regression,redteam N=5 BASELINE=baseline_p1_exit
```

- yaml 字段与 baseline_p0 相同（`report.PublishBaseline` 生成）：`suites` 为 `[regression, redteam]`，`n` 为 5，`gate` 仍是 Gate 0 自动项那一行。
- redteam pass^5 不进 yaml，看 `report_key` 指向的 report.md「按套件红线 pass^k」段落；§6.9 验收 1 要求 redteam 一行 pass^5 = 1.00。
- 登记后 baseline_p0 保留不动，P1 之后的对照改用 baseline_p1_exit。

## baseline_p1（P2 冻结基线，待调度方在共享库执行）

`baseline_p1.yaml` 是冻结指针（名字 → experiment.id + 快照 + manifest 键），由 `baseline freeze` 写，不手改。exp 102 是 P1 实验（`snapshot` 为 NULL、`trial.sample_version` 为 0），用 `baseline_p1_exit.yaml` 补录：

```bash
make migrate-eval GO=go                       # 先把 agri_eval 迁到 0002_experiment_p2
make eval-freeze GO=go NAME=baseline_p1 EXP=102 BACKFILL=eval/baselines/baseline_p1_exit.yaml
```

- 补录字段按事实填：loader `v0`、retrieval `progressive`、judge_versions `{}`、suites / n 取 yaml、token_used 0 与 cost 0（stub）、wall_time 由 started_at / finished_at 算；`sample_version` 取当前样本版本。
- knowledge_ref 不取 yaml 里的 `WORKTREE`，按 exp 102 的 git_sha（`c261f71`）上 `knowledge/` 内容重算为 `kc:` 哈希。exp 102 `git_dirty: true`，当时读的是工作区，提交内容只是近似；本次核对 `c261f71` 与当前 HEAD 的 `knowledge/` 哈希相同（`kc:3b4f180e…eacbe`）。
- 只冻结 DONE 且至少有一条 trial 的实验。
- 前提：按 yaml 的 suites / tags 重选当前样本后 `samples_hash` 必须等于 yaml 里的值，否则拒绝（样本已改版，不能代表 exp 102）。补录与 `frozen = 1` 同一事务；冻结后 experiment 的任何 UPDATE 返回 `ErrFrozen`。
- 样本清单写 MinIO `agri-steward-eval/102/manifest.json`。P2 起每次 `run` 都写快照与 manifest，冻结时不再补录（不带 `--backfill`）。
- 指针文件已存在时 freeze 拒绝（`O_CREATE|O_EXCL` 写，不覆盖已登记基线）。`.r2` 等后续基线复用同一命令。
- 冻结已提交但指针没写成（如写文件失败）时，用同一命令重跑：实验已冻结且库中快照与这次补录算出的一致，只补写指针，stdout 打印 `pointer-only`；快照不一致返回 `ErrFrozenMismatch`，不写指针。
- compare 的 `--baseline` 无论给名字还是 id，都要求该实验在库中 `frozen = 1`；指针文件只是名字到 id 的映射。
- 对比：`make eval-compare GO=go BASELINE=baseline_p1 CANDIDATE=<id>`（单因子，写候选的 factor_diff / baseline_id / compare_mode）；`MODE=phase` 为阶段级，只出报告；`FAIL_ON_UNRELEASABLE=1` 时不可放行退出码 1。报告在 `eval/out/compare/{baseline}-{candidate}.md`。
