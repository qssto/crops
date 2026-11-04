# P2 第 2 批简报：L7-a ∥ L2（两个子 agent 并行，分别提交 P2-L7a / P2-L2）

先读：`docs/agent-brief-common.md`、`docs/briefs/P1-common.md`、`docs/briefs/P2-common.md`；`docs/dev-log.md` 的 `## P2-L0`、`## P2-L1-T` 两节及其子节（尤其「待办」各条）；`eval/README.md` 的 P2 L0 / L1-T 两节。
基线分支：`p2-b2-base`（= main 上 P2-L1 提交 + 本简报）。开工先 `git merge --ff-only p2-b2-base`。
迁移：调度方已写好评测库 `0003_experiment_gate`（L7-a）与 `0004_review_task`（L2），在主仓 `migrations/eval/`（gitignored，照路线 DDL）。开工把主仓 `migrations/` 整个复制到 worktree。本批不加其他迁移，不碰业务库迁移。
命名空间：L7-a 用 `AS_TEST_NS=p2l7a`，L2 用 `AS_TEST_NS=p2l2`。集成测试只写 `{ns}_` 前缀的隔离库（参考 `cmd/eval/l0_integration_test.go` 的 TestMain），不写共享 `agri_eval`。
上下文：你的上下文有限。读文件用 offset / grep，不要整份读大文件；分小步提交（每完成一块就在分支上 commit）。

## 共享文件（只追加，各写各的 case / 段落 / 新文件，调度方合并）
`cmd/eval/main.go`（子命令 switch 各加 case，实现放新文件）、`Makefile`、`docs/dev-log.md`（`## P2-L7a …` / `## P2-L2 …`）、`eval/README.md`、`internal/evalkit/store`（各开新文件，不改对方函数）、`internal/evalkit/evalkit` 的 `Snapshot`（L2 只加 judge_versions 的取值，不改结构）。
两边都会碰 `store` 里的实验状态：L7-a 负责 `status` 新取值 `BUDGET_EXCEEDED` / `VOID` 与 `void_reason`；L2 不改实验状态。

---

## L7-a：红线门禁先行 —— P2 §6.8（655-716 行）功能任务 1-4，技术实现 ① 1-3、SQL
文件归属：`.github/workflows/`（新）、`Makefile` 的 `eval` / CI 相关目标、`cmd/eval` 的 `run` 护栏与 `resume` 子命令、`internal/evalkit/runner*`（并发与预算）、`internal/evalkit/store` 中新文件（void / budget）、`eval/smoke/`（新，冒烟子集清单）。

1. 冒烟子集（任务 1）：从 `regression` 选约 30 条，覆盖 `code.All` 里全部红线打分器（每个红线打分器至少 2 条适用样本）。清单放 `eval/smoke/smoke.yaml`（只列 `sample_id`，不复制样本）；单测校验：清单里的样本都存在且在 regression、覆盖全部红线打分器（红线集合由 `code.All(...).RedLine()` 派生，新红线自动纳入，不手写）。
2. `make eval MODE=smoke|nightly|release`（技术实现 1）：smoke = 冒烟子集 n=3 `--trigger pr`；nightly = `regression,redteam,capability` n=5 `--trigger nightly`；release = 全部套件含 holdout n=5 `--trigger release`（holdout 未挂载时 release 失败关闭，报清楚的错）。不带 MODE 时保持现在的行为（`SUITE` / `N` / `TAGS` / `BASELINE`），`make eval-nightly` 保留但改为调 `MODE=nightly`。现在的 `eval-nightly` 不带 `--trigger nightly`、不含 capability，`cmd/eval graduate` 因此永远 0 个可毕业（L1 评审 P1）；nightly 的 n 不得小于 `store.GraduateTrials`（=5，毕业要求每次 pass^5），加单测。
3. 预算（任务 3、技术实现 2）：`run --token-budget N`（写 `token_budget`；不传 = NULL 不限）。累计 token 超预算即停止派发新 trial、实验 `status=BUDGET_EXCEEDED`。`--trigger pr|nightly|release` 时必须传预算，否则失败关闭。stub 下 token 为 0：测试用注入的 token 计数构造超预算。
4. 作废（任务 3、技术实现 3）：实验结束按 SQL 判定 `infra_error` 占比 > 5% 或存在 `grader_error` → `UPDATE ... status='VOID', void_reason=?`（`INFRA_ERROR_RATE` / `GRADER_ERROR`）。作废规则用注册表（名字 → 判定函数），以后加规则只加注册项。阈值 5% 放配置并写进报告。VOID / BUDGET_EXCEEDED 的实验：`compare` 与 `baseline freeze` 都拒绝（失败关闭，补测试）；报告首行写明作废原因。
5. `cmd/eval resume --exp <id>`（任务 4）：只补跑未完成与 `infra_error` 的 trial（按 `(sample_id, trial_index)` 判断），同一实验 id 续写；实验须未冻结；补跑后重新做作废判定。快照必须与当前配置一致（因子与 samples_hash），不一致拒绝。
6. 并发（任务 4、技术实现 2）：trial 并发 = `floor(QPM × 0.7 / 每 trial 调用数估计)`，至少 1；QPM 放配置（`eval.qpm`，stub 下给默认值）。每个 trial 从现有 schema 池取 `agri_eval_t_{n}`（看 `internal/evalkit/poolenv.go`，池大小与并发对齐），Redis 前缀 `as:eval-t{n}:`，trial 结束按前缀删除。用 `-race` 跑并发单测；并发下 transcript / trial 写入不能串号。如果并发改动风险大，先保证默认并发 1 的行为与现在逐字节一致（报告除时间外不变）。
7. CI（GitHub Actions，`.github/workflows/`）：`pr-smoke.yml`（改动 `internal/**` 或 `knowledge/**` 的 PR：`make eval MODE=smoke`，红线 pass^3 < 1 失败）、`nightly.yml`（cron，`MODE=nightly`，失败不阻断合并）、`release.yml`（手动触发，`MODE=release`）。服务（MySQL / Redis / MinIO）用 workflow services 或 compose，密钥用 secrets，不写进文件。本地无法跑 Actions：用 `actionlint` 不可用就只做 YAML 解析单测 + 本地等价 make 目标验证。
8. P2-L0 留下的两条 L7 待办（见 dev-log）：
   - CI 调 `compare` 必须带 `--fail-on-unreleasable`，并在 CI 里自检：构造一个不可放行的对比确认退出码为 1（可写成 `cmd/eval` 集成测试 + workflow 里的一步）。
   - 提示词改了但 `prompt_version` 没升：加检查（`prompts/` 相对基准有变更而 prompt_version 未变 → 失败），放 `cmd/eval` 子命令或 make 目标，CI 调用。
9. 红线：门禁判定属红线代码，每项先写应失败测试。验收 §6.8 L7-a 相关的第 1、3 条逐条回报（第 1 条在本地用「故意破坏 citation_valid 的改动 + make eval MODE=smoke 返回非 0」替代真实 PR，标「待真实 PR」）。
10. 不做：L7-b（发版质量门禁、`cmd/eval gate`、成本延迟、模拟用户）。

---

## L2：双轨标注与 LLM-judge 校准 —— P2 §6.3（232-320 行）功能任务 1-10、技术实现 ① 1-5、SQL
文件归属：`internal/evalkit/grader/judge/`（新）、`eval/judges/<维度>/`（新）、`internal/evalkit/review/`（新包，建议名：人工队列与校准统计）、`internal/evalkit/store` 中 review_task 新文件、`cmd/eval judge calibrate|recheck|queue` 子命令、`eval/holdout_readers.yaml`（新）、holdout submodule 挂载、`internal/evalkit/sample/coverage` 的 holdout 计数改动、评审页（amis JSON 放 `web/amis/`，gitignored 不入库；后端接口按现有 controller / service / repository 分层）。

1. 三个 judge（任务 1-3、技术实现 1）：`fact_consistency`、`conclusion_match`、`explanation_quality`，各一个打分器，实现现有 grader 接口，输出 `{score 1-3, evidence, verdict}`；`evidence` 不是被评文本原句（经 `textnorm` 规范化后子串匹配）→ grader_error；信息不足 → `Score.Unknown=true`。材料只给 fixture、所引知识条目原文、专家标准结论、最终输出，不给中间推理（单测断言材料里没有中间步骤字段）。提示词与 1-3 分评分标准、正反例放 `eval/judges/<维度>/{prompt.md,rubric.md}`。
2. judge 模型（任务 4）：没有真实模型（见 P2-common）。judge 走独立配置项（`eval.judge.model` / `model_tier`，与智能体分开），实现一个录制回放客户端：按 `(维度, 材料哈希)` 读 `eval/judges/<维度>/recordings/*.json`；没有录制时返回 UNKNOWN，不报 infra_error。表驱动单测覆盖 1 / 2 / 3 分与 UNKNOWN（验收 1）。judge 版本 = 提示词 + 评分标准内容哈希 + 模型档位，写进快照 `judge_versions`（L0 已把 judge_versions 列为 Pinned 因子，变更即重建 baseline）。judge 在门禁配置里默认关闭，只进报告。
3. review_task（SQL 照抄，`ON DUPLICATE KEY UPDATE id = id`）：`ref_key` = md5 拼接按路线定义；store 新文件实现插入、派单、专家打分、三条统计查询。
4. 人工队列（任务 8、技术实现 2）：四类入口 UNKNOWN / FLAKY / CONFLICT（judge 与代码打分器结论冲突，至少 `citation_valid`）/ RANDOM_10（通过 trial 的 10% 抽检，带 seed 的 `math/rand/v2`，seed 进报告）。入口判定用注册表。验收 3：judge 与 `citation_valid` 冲突的 trial 100% 入队（测试构造）。
5. 校准（任务 5-7、技术实现 3）：`cmd/eval judge calibrate --dim <维度> --exp <id>` 按专家分数分层抽 ≥ 100 个 trial 生成 CALIBRATION 任务（`rater_no=1`），随机 25% 生成 `rater_no=2`；二次加权 Kappa（标准库自写）+ bootstrap 1000 次 95% 区间、混淆矩阵、专家间 Kappa、UNKNOWN 占比，报告存 MinIO 并在本地 `eval/out/` 出 md。阈值在 `eval/gates/` 下配置（初步参考 0.6，标「待实测」），以区间下限比阈值，达标维度标「可进门禁」。没有专家：用合成专家分数跑通全流程（集成测试插 review_task 行），Kappa 已知答案单测（手算小例）。标「待专家」「待真实模型」。
6. 月度复核（任务 10、技术实现 4）：`cmd/eval judge recheck` 抽 20 个 trial 生成 MONTHLY_RECHECK，按第 5 步算一致性；用注入时钟测月份边界。
7. holdout（任务 9、技术实现 5）：
   - 私有仓 `~/Desktop/crops-holdout`（已有 78 条，HEAD 3464e86）以 git submodule 挂到 `eval/suites/holdout/`。注意：样本在私有仓的 `<分类>/` 目录下、`suite: holdout`；挂载后 `eval/suites/holdout/<分类>/*.yaml` 要能被 `validate` / `run` 正确识别为 holdout 套件（检查 suite 与目录的一致性规则）。`.gitmodules` 的 url 用本地路径；主仓只提交 submodule 指针，**不得把任何 holdout 样本内容写进主仓历史**（验收 4：写一个测试或脚本检查 `git log --all -- eval/suites/` 的公开历史里没有 `rice-ho-` ID）。
   - 不读未授权内容（L1 评审 P1）：现在 `sample.LoadDir` 会走进 `eval/suites/holdout/` 并解析、校验每个文件（pii_clean 违规消息还会带出命中值），挂载后 `validate` / `run` / `graduate` / `kbdump` / graph 样本测试（`internal/agent/graph/harness_test.go` 的 `loadSamples`）都会读到 holdout 内容，覆盖报表的「content not read」也会失真。改为 `LoadDir` 默认跳过 `<root>/holdout`（`fs.SkipDir`），只有授权读者显式要 holdout 时才进入；`coverage/count.go` 里 holdout/ 下 `suite` 字段不是 holdout 的文件会被 `Counted` 与 `countHoldout` 重复计数，一并修。每项先写应失败测试。
   - `eval/holdout_readers.yaml`：只列 holdout 负责人与 CI 服务账号。`evalkit` 读取 holdout 前校验调用者身份（取法自定，例如环境变量 `AS_EVAL_READER`，缺省视为无权限），不在名单里就拒绝读内容（失败关闭，单测：非名单身份读取被拒）。
   - 覆盖报表（P2-L1 待办）：授权读者下把 holdout 样本计入格子（公开侧现缺的 78 格正好由 holdout 补齐，授权后补写清单应为 0）；未授权时维持现状只数文件。报表对 holdout 只出计数，不出 ID 以外的内容。
   - holdout trial 的 transcript 与报告明细写 `agri-steward-eval/report/holdout/` 前缀；公开报告里 holdout 的翻转与失败样本只给 ID。
   - redteam 不抽样；PR / 夜间红线覆盖不变（holdout 只在 release 跑，L7-a 负责 MODE=release）。
8. 测试：judge 表驱动（1/2/3/UNKNOWN、evidence 非原句 → grader_error、材料不含中间推理）、Kappa / bootstrap 已知答案、四类入队、`ON DUPLICATE KEY` 重跑不重复派单（集成）、校准全流程（集成，合成专家分）、holdout 读权限拒绝、holdout 计入覆盖报表、公开历史无 holdout ID。
9. 验收 §6.3 第 1-4 条逐条回报，不能真实达成的标「待专家」「待真实模型」。

## 回报
按 `docs/agent-brief-common.md` 回报格式；另附：worktree 分支提交 SHA、共享文件改动摘要、需调度方在主仓跑的集成测试名、需调度方执行的一次性操作（例如在主仓添加 submodule 的准确命令）。动手前先用 Skill 调 `coding-fundamentals`（或读 `~/.claude/skills/coding-fundamentals/SKILL.md`）。回报控制在 800 字以内。
