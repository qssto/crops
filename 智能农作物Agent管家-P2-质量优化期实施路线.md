# 智能农作物 Agent 管家 Phase 2（P2）质量优化期 实施路线（8 周）

> 文档类型：阶段详细实施路线（《分阶段实施路线图》§5.2 的展开）
> 上游文档：《分阶段实施路线图》§3.3、§4.7、§5.2、§7.2；《评测 Harness 设计方案》§7.3、§8、§9.2、§10、§11、§12、§13；《P1 生产可用期实施路线》
> 下游文档：《P3 高级能力期实施路线》（以本阶段冻结的 `baseline_p2` 为起点）
> 读者：Agent Harness RD（含评测负责人）、知识工程 RD、QA、SRE、农艺专家组、项目 Owner
> 状态：草案，Gate 1 通过后、Phase 2 开工前评审

---

## 1. 文档定位

1. 本文回答 Phase 2 的「具体做哪几条路线、每条路线改哪些模块、交付什么、怎么验收、按什么顺序合入」，是团队的执行手册。
2. 母文档 §5.2 给出了 10 项功能与 Gate 2 八条；本文把它们拆成 `L0 → L8` 九条路线，补齐涉及模块、交付物、依赖、周排期与合入规则。
3. 本阶段的唯一主题：把「建议靠不靠谱」变成可度量、可提升、不回退的工程指标。任何策略改动都必须先有评测与指标口径，再动手。
4. 与母文档或《评测 Harness 设计方案》冲突时，以母文档为准，并回改本文。

## 2. 阶段基线与输入

### 2.1 Phase 0 留下的基线

1. `regression` 套件 100 条样本（应拒答 ≥ 20 条，全部专家署名），样本 YAML 与 `cmd/eval validate` 静态校验。
2. 红线代码打分器与轨迹断言全套：`schema_valid / citation_valid / numbers_match_calc / product_in_candidates / refusal_correct / no_model_actuate / stop_at_approval / kb_miss_refuse / pii_clean / state_machine_legal / outcome_match / must_call / budget`。
3. Phase 0 baseline 报告（n=3、pass^3），附配置快照：代码提交号、提示词版本、模型档位、知识版本；本文记为 `baseline_p0`。
4. Gate 0 依实测确定的 P95 目标线。

### 2.2 Phase 1 留下的产出

1. `redteam` 套件与下发前复检、`EXPIRED`、`PARTIALLY_DONE`、跨农场隔离打分器，红线 pass^5 = 1。
2. 真实下发链路与 5 个示范农场的线上 `run_id`、审计日志、MinIO 原文 —— 本阶段归因、回流、会话长度分布的数据来源。
3. 审批意见与驳回原因分类入库（用量不对 / 时机不对 / 药剂不对 / 依据不足 / 其他）。
4. 告警体系（含「建议无引用」P0 级告警）、`enable_advice / enable_robot_dispatch` 开关与按农场灰度能力。

### 2.3 沿用不变的契约与约束

1. 五个契约（`AdviceCard / WorkOrder / Citation / RunState / ApprovalTicket`）不改字段；唯一启用的是 Phase 0 预留的 `RunState.compaction_state`，其内部 schema 走契约版本号与 Owner 审批。
2. 红线一、红线二的实现与打分器只增强不放松；本阶段任何改动不得降低 `citation_valid`、`numbers_match_calc` 的 100%。
3. 不引入 RAG、多模态、自动规划（见 §3.2）。

## 3. 范围边界

### 3.1 本阶段必须完成

1. 评测集扩容到 500+ 条，分层抽样，应拒答 ≥ 20%，陷阱样本独立成组。
2. 双轨标注：专家金标准 + 三个维度的 LLM-judge，校准达标后进门禁；建立 `holdout` 套件。
3. 可信度六指标（引用覆盖率、事实一致率、数值准确率、幻觉率、拒答正确率、过度保守率）进日报，可按 `run_id` 下钻。
4. 规则与模型职责再切分：以回放与驳回归因为依据，把模型隐式承担的确定性判断下沉到规则层。
5. 知识加载策略优化：新增三项加载指标，按问题类型定制策略，可配置、可 A/B。
6. 长对话压缩 `compaction` 上线，关键事实白名单字段 100% 保留。
7. 回归门禁、成本与延迟门禁接入 CI：PR 冒烟阻断合并，夜间全量，发版全套件含 `holdout`。
8. LLM 模拟用户引入多轮样本（只进质量指标，不进红线门禁）。
9. 归因 Top 问题清单按周产出；线上回流自动生成样本草稿。

### 3.2 本阶段明确不做

1. 不引入 RAG 与向量检索：检索质量优化的前提是本阶段先有稳定评测集与指标口径，留 Phase 3 L1。
2. 不引入多模态诊断、主动预警、多机排程、自动规划，留 Phase 3。
3. 不做模型微调、不自建推理集群；模型档位只在 D1 已批准的档位内对比。
4. 不扩作物与区域：评测集扩容只在首个作物、首个区域内加深。
5. 不放松任何红线规则来换取过度保守率的改善；过度保守率只能通过补知识条目、修加载策略、下沉规则来改善。
6. 不接入 Coze Loop 作为门禁依赖；本阶段只评估是否用作实验看板（评测设计方案 §15）。

## 4. 目标与通过标准（Gate 2）

| # | 通过标准 | 判定方式 / 判定人 |
|---|----------|-------------------|
| 1 | 评测集 ≥ 500 条，`作物 × 生育期 × 问题类型 × 难度` 分层覆盖无缺口，应拒答 ≥ 20%，专家签署齐全 | `cmd/eval validate` 覆盖统计报表；农艺负责人签字 |
| 2 | 六个可信度指标上线并进日报；引用覆盖率与数值准确率保持 100% | 日报截图 + `evalkit` 夜间全量报告；QA 核对口径 |
| 3 | 事实一致率与幻觉率（口径见 `L3`）相比 `baseline_p0.r2` 显著且可复现改善：用阶段级对比（`compare --mode phase`，见 `L0`）在 L1 冻结的 500+ 样本清单、同一 `judge_versions` 上配对比较，配对差值置信区间不跨 0，且同一 snapshot 下 baseline 与 candidate 同时段交替各跑 2 次结论一致；两指标同源于 `fact_consistency`，合计算一项证据；依赖第 8 条中 `fact_consistency` 已校准达标；目标线由 Gate 1 后依 baseline 实测确定 | `cmd/eval compare --mode phase` 报告；评测负责人签字 |
| 4 | `make eval` 回归门禁生效，且至少拦下过 1 次真实质量回退 | CI 阻断记录 + 对应 PR 链接 |
| 5 | 成本与延迟门禁生效，单会话成本与 P95 均不劣于 baseline | 门禁报告；SRE 核对线上看板 |
| 6 | 归因 Top 问题清单连续产出 4 周，≥ 10 条知识条目因此被补齐或修正 | 周报归档 + 知识条目变更 PR 列表；农艺负责人签字 |
| 7 | `compaction` 上线：白名单字段在最终组装的 prompt 上逐字段校验 100% 保留，压缩前后结论不漂移，长会话成本下降有实测数据 | `compaction` 专项评测报告 |
| 8 | 三个 LLM-judge 维度与专家加权 Kappa 均达阈值并进门禁（硬条件，不带未达标维度出阶段）；`holdout` 建立；线上回流样本每周有产出 | 校准报告 + 回流周报；评测负责人签字 |

Gate 2 由项目 Owner 依上表证据判定；红线相关项不允许带条件通过。

## 5. 实现路线总览（L0 → L8）

1. `L0`：基线冻结与实验配置快照
2. `L1`：评测集扩容与分层
3. `L2`：双轨标注与 LLM-judge 校准
4. `L3`：可信度指标体系与日报
5. `L4`：规则与模型职责再切分
6. `L5`：知识加载策略优化
7. `L6`：长对话压缩 `compaction`
8. `L7`：回归门禁、成本延迟门禁与 CI（含 LLM 模拟用户）
9. `L8`：归因 Top 问题清单、线上回流与 Gate 2 收口

建议顺序：`L0 → L1 + L2 → L3 → L4 + L5 + L6 → L7 → L8`

其中 `L7` 分两段：红线门禁进 CI（`L7-a`）在第 2 周随 `L0` 接入，只依赖 Phase 0-1 已有的红线打分器；质量退化门禁、成本延迟门禁（`L7-b`）依赖 `L3` 指标口径与 `L2` judge 校准，第 6-7 周接入。`L4 / L5 / L6` 的每个 candidate 都必须过 `L7-a`，Gate 2 前全部过 `L7-b`。

```
            ┌──────────── L0 基线冻结 / Experiment 快照 ────────────┐
            │                                                       │
            ▼                                                       ▼
   L1 评测集扩容 ──► L2 双轨标注 / judge 校准             L7-a 红线门禁进 CI
            │                 │                                     │
            └───────┬─────────┘                                     │
                    ▼                                               │
             L3 可信度六指标 / 日报                                 │
                    │                                               │
       ┌────────────┼────────────┐                                  │
       ▼            ▼            ▼                                  │
  L4 规则下沉   L5 加载策略   L6 compaction  ◄── 每个 candidate 过 ──┘
       │            │            │
       └────────────┼────────────┘
                    ▼
         L7-b 质量 / 成本 / 延迟门禁 + 模拟用户
                    │
                    ▼
     L8 归因清单 + 线上回流 + Gate 2 收口（归因周报自第 2 周起持续产出）
```

## 6. 路线详细拆解

### 6.1 L0 基线冻结与实验配置快照

**目标**：在动任何策略之前，把 Phase 0-1 的成绩钉成可配对比较的基线，并把「一次只变一个因子」变成工具约束而不是口头约定。

**涉及模块**：`internal/evalkit/store/`（`Experiment` 记录）、`eval/baselines/`、`cmd/eval compare`、`internal/app/`（配置快照采集）。

**功能任务**

1. `Experiment` 记录补齐评测设计方案 §12.1 全部字段：`git_sha / prompt_version / model_tier / loader_strategy / retrieval_strategy / knowledge_ref / judge_versions / suites / n / token_used / cost / wall_time`；`retrieval_strategy` 本阶段固定为 `progressive`，为 Phase 3 预留。
2. `baseline_p1` 就是 P1 L8 收口时登记的 `baseline_p1_exit` 实验，本阶段不重新生成：L0 补录其配置快照并置 `frozen=1`，供第 1-5 周的 PR 冒烟与夜间对比；`baseline_p0` 保留不动。
3. 基线重建（第 5 周，`L2` 校准完成、`L1` 样本清单与 `holdout` 划定之后）：用当前 judge 在冻结的 500+ 样本清单上重跑 n=5，得到 `baseline_p1.r2`（`L4 / L5 / L6` 单因子对比的对照）与 `baseline_p0.r2`（`baseline_p0` 配置，Gate 2 第 3 条的对照）。`judge_versions` 不同的实验不允许直接比较。
4. `cmd/eval compare` 两种模式：
   - 单因子模式（默认，用于 PR 与发版门禁）：除 `git_sha` 外差异因子 > 1 时报告首行标红「多因子对比，结论不可归因」，结论为 `NOT_ATTRIBUTABLE`，不得用于门禁放行；`knowledge_ref` 或 `judge_versions` 不一致直接拒绝，提示重建 baseline（评测设计方案 §8.4-4）。
   - 阶段级模式（`--mode phase`，只用于 Gate 2 第 3 条）：允许多因子，只给整体结论（改善 / 无显著差异 / 退化），不做因子归因，不写 `gate_verdict`、不用于 PR 门禁；要求两边样本清单哈希一致、`judge_versions` 一致。知识库是本阶段的优化对象（Gate 2 第 6 条），`knowledge_ref` 允许不同：报告首行并列两边 `knowledge_ref` 与条目变更清单，并另跑一列「`baseline_p0` 配置 + P2 末知识快照」作为参考，把改善拆成知识侧与 agent 侧两部分，只出报告不判定。
5. 样本清单锁定：snapshot 记录 `sample_id@version` 列表的哈希；`compare` 只在两边 `(sample_id, sample_version)` 的交集上配对，报告写明交集大小与被排除样本。
6. 噪声基线（A/A）：同一 snapshot 在 `regression` 全量上跑 3 次（n=5），取每个可信度指标两两差值的 95% 分位作为该指标退化阈值的下限，写 `eval/gates/thresholds.yaml`（Owner 签字）；snapshot 变化达到重建条件时重测。
7. 对比时段：单因子 candidate 报告与阶段对比都在同一时段交替跑 baseline 与 candidate（不复用数周前的结果，以排除模型服务端漂移）；PR 冒烟与夜间仍对比入库 baseline。
8. 列出本阶段候选因子登记表（`L4` 下沉项、`L5` 加载策略、`L6` 压缩阈值、提示词版本、模型档位），每项填 baseline / candidate / 指标 / 评测集 / 回滚路径，作为 §8 的输入。

**交付物**：`baseline_p1` 快照补录与冻结；`baseline_p1.r2`、`baseline_p0.r2`；因子差异检查与阶段级对比；A/A 阈值表；候选因子登记表。

**验收**

1. `baseline_p1` 快照字段无空值且 `frozen=1`，可被 `cmd/eval compare` 引用；第 5 周 `baseline_p1.r2`、`baseline_p0.r2` 入库。
2. 构造一次同时改提示词与加载策略的对比，单因子模式报告首行出现多因子警告且门禁判定为「不可放行」；同一对比用 `--mode phase` 只出整体结论、无 `gate_verdict`。
3. A/A 阈值表六个指标齐全，Owner 签字。
4. 候选因子登记表每行五项齐全，缺项的因子不进入排期。

**技术实现**

① 流程

1. `cmd/eval run` 启动时由 `app.Build` 采集配置快照（任务 1 的字段 + 样本清单哈希 `samples_hash`），写入 `experiment.snapshot`；样本清单明细存 `agri-steward-eval/{experiment_id}/manifest.json`；P2 起新实验 `snapshot` 为空时 store 层拒绝写入。每条 trial 写 `sample_version`。
2. `baseline_p1`：按 P1 登记的 `experiment.id` 补写 `snapshot` 后执行 `cmd/eval baseline freeze --name baseline_p1 --exp {id}`：置 `frozen=1`，指针文件 `eval/baselines/baseline_p1.yaml`（名字 → `experiment.id`）。`.r2` 基线同样以 `freeze` 登记。
3. `cmd/eval compare --baseline {名字} --candidate {id} [--mode phase]`：读两份 `snapshot`，忽略 `git_sha` 逐项比对，差异写 `factor_diff`；单因子模式下差异因子 > 1 → `NOT_ATTRIBUTABLE`，`knowledge_ref` / `judge_versions` 不一致 → 拒绝；阶段级模式只校验 `samples_hash` 与 `judge_versions`，结论只写报告。
4. 配对集合取两边 `(sample_id, sample_version)` 交集；质量指标按样本级值配对（flaky 样本按样本级均值参与，另列清单），红线 flaky 按失败。
5. `cmd/eval aa --runs 3`：同一 snapshot 跑 3 次，按指标算两两差值 95% 分位，生成 `eval/gates/thresholds.yaml` 草稿，Owner 以 PR 签字合入。

② SQL（`agri_eval` 库；迁移版本号按合入顺序领取，母文档 §4.6.1 表设计约定 8）

```sql
-- migrations/eval/000N_experiment_p2.up.sql（第 1 周合入）
ALTER TABLE experiment
  ADD COLUMN snapshot    json            NULL,  -- 配置快照（含 samples_hash），P2 起新实验必填
  ADD COLUMN baseline_id bigint unsigned NULL,  -- compare 的对照实验
  ADD COLUMN compare_mode varchar(8)     NULL,  -- FACTOR / PHASE
  ADD COLUMN factor_diff json            NULL,  -- [{name, baseline, candidate}]
  ADD COLUMN frozen      tinyint(1)      NOT NULL DEFAULT 0,
  ADD KEY idx_baseline (baseline_id),
  ALGORITHM=INPLACE;
ALTER TABLE experiment MODIFY COLUMN suite varchar(128) NOT NULL;  -- 发版跑全部套件时逗号拼接超过 32；明细以 snapshot.suites 数组为准
ALTER TABLE trial ADD COLUMN sample_version smallint NOT NULL DEFAULT 0, ALGORITHM=INSTANT;

UPDATE experiment SET frozen = 1 WHERE id = ? AND frozen = 0;
-- store 层所有对 experiment 的更新都带 AND frozen = 0，冻结后不可改

-- 配对交集
SELECT b.sample_id, b.sample_version
FROM (SELECT DISTINCT sample_id, sample_version FROM trial WHERE experiment_id = ?) b
JOIN (SELECT DISTINCT sample_id, sample_version FROM trial WHERE experiment_id = ?) c
  USING (sample_id, sample_version);
```

③ Redis 键：无。

### 6.2 L1 评测集扩容与分层

**目标**：把评测集从 100 条扩到 500+ 条，并保证分层覆盖、负样本与陷阱样本占比，使后续每一项优化都能在足够细的切片上看出变化。

**涉及模块**：`eval/suites/{regression,capability,redteam}/`、`eval/refs/`、`internal/evalkit/sample/`（静态校验与覆盖统计）、`question_taxonomy`。

**功能任务**

1. 建分层矩阵 `作物 × 生育期 × 问题类型 × 难度`（问题类型沿用 `question_taxonomy` 子类），按 Phase 1 线上问题分布确定各格目标条数，空格与低覆盖格列入补写清单。
2. `cmd/eval validate` 增加覆盖统计输出：每格条数、`refuse` 占比、陷阱样本占比，低于目标的格标红。
3. 陷阱样本独立成组，用 `tags` 标记：`相似病虫害混淆 / 超剂量诱导 / 跨作物误用 / 过期知识引用`；每个陷阱样本写明期望拒答或期望的纠正结论。
4. 应拒答负样本保持 ≥ 20%，覆盖三类来源：知识未覆盖、数据缺失、超出作物范围。
5. 新样本默认进 `capability`；连续 3 次夜间全量 pass^5 = 1 的样本毕业进 `regression`（评测设计方案 §10）。
6. 专家按样本模板逐条撰写并署名；QA 负责格式工程化、参考解生成与 `validate` 通过。
7. 多轮样本（`user_sim`）单独计数，不计入 500 条的红线样本口径。

**交付物**：500+ 条样本与参考解；分层覆盖报表；陷阱样本组。

**验收**

1. 样本总数 ≥ 500，全部通过 `cmd/eval validate`（含参考解全过代码打分器）。
2. 覆盖报表无空格；`refuse` 占比 ≥ 20%；四类陷阱样本各自成组且有专家署名。
3. 抽查 30 条新样本，fixture 中无真实农户身份与精确坐标（`pii_clean` 对样本文件扫描通过）。

**技术实现**

① 流程

1. `cmd/eval validate` 遍历 `eval/suites/**`：样本格式校验 → 参考解过全部代码打分器 → `pii_clean` 扫描样本文件。
2. 按 `作物 × 生育期 × 问题类型 × 难度` 计数，与 `eval/coverage/targets.yaml` 中各格目标条数比对，输出覆盖报表（每格条数、`refuse` 占比、陷阱组占比，低于目标标红）。
3. 夜间全量结束后 `cmd/eval graduate` 查最近 3 次夜间全量的 trial 结果，把 `capability` 中当前版本连续 pass^5 = 1 的样本移到 `regression`，自动开 PR 由评测负责人合并；样本修订后旧版本的通过记录不计。
4. 样本仍以 YAML 文件进 git，评测库不另建样本表；`user_sim` 样本在报表中单独计数。
5. `holdout` 样本不从现有公开套件抽取：L1 新增的 400 条里按 `category × difficulty` 分层预留约 20%，由 `holdout` 负责人直接收进私有仓（见 `L2`），从不进入 `eval/suites/` 的公开历史；`redteam` 不抽样。

② SQL（`agri_eval` 库，无新表；毕业判定查询，`?` 为各样本当前 `version`，由 `graduate` 从 YAML 读出后按样本批量传入）

```sql
SELECT t.sample_id
FROM trial t
JOIN (SELECT id FROM experiment
      WHERE trigger_kind = 'nightly' AND status = 'DONE'
      ORDER BY id DESC LIMIT 3) e ON e.id = t.experiment_id
WHERE t.suite = 'capability' AND (t.sample_id, t.sample_version) IN ((?, ?), ...)
GROUP BY t.sample_id
HAVING COUNT(DISTINCT t.experiment_id) = 3 AND SUM(t.passed = 0) = 0;
```

③ Redis 键：无。

### 6.3 L2 双轨标注与 LLM-judge 校准

**目标**：让「事实一致、结论一致、解释可读」这三个代码判不了的维度可以规模化打分，同时以专家标注为金标准，在校准达标前不让 judge 影响门禁。

**涉及模块**：`internal/evalkit/grader/judge/`、`eval/judges/`（提示词与评分标准）、`internal/evalkit/report/`（人工队列）、`eval/suites/holdout/`。

**功能任务**

1. 实现三个 judge：`fact_consistency`、`conclusion_match`、`explanation_quality`，一个维度一个 judge，输出 `{score, evidence, verdict}`，`evidence` 必须引用被评文本原句。
2. 每个维度写 1-3 分评分标准与正反例，存 `eval/judges/<维度>/`；judge 只看 fixture、所引知识条目原文、专家标准结论、最终输出，不看中间推理。
3. 允许 `UNKNOWN`：信息不足即返回，`Score.Unknown=true`，自动进入人工队列。
4. judge 模型与智能体分开配置，且选与智能体不同家族的模型；D1 批准范围内做不到时，校准报告单列同源偏好检测（同一样本 judge 对智能体输出与专家改写答案的打分差）。judge 模型档位与提示词纳入 `judge_versions`，任一变更视为打分器变更，需重新校准。
5. 建校准集：每个维度 ≥ 100 个 trial 由专家标注，按专家分数分层，1/2/3 分每类 ≥ 20 个（不足时从 `capability` 与陷阱组 trial 补抽，不只用通过率高的 `regression`）；1-3 分为有序量表，用二次加权 Kappa，给 bootstrap 95% 区间，以区间下限对照阈值；阈值在本阶段启动时按实测确定（评测设计方案初步参考 0.7）。
6. 双人标注：校准任务中随机 25% 由第二位专家独立标注，专家间加权 Kappa 作为该维度可达上限写进校准报告；judge 返回 `UNKNOWN` 的 trial 不进 Kappa 计算，`UNKNOWN` 占比单列。
7. 未达阈值的维度：judge 分数只进报告；以专家为准回修提示词，重新校准。Gate 2 第 8 条要求三个维度全部达标，未达标即 Gate 2 不通过，不带条件出阶段。
8. 人工队列四类入口落地：`UNKNOWN`、flaky、judge 与代码打分器冲突、通过样本随机 10% 抽检。
9. 建 `holdout`：只用 P2 新写、从未进公开仓库的样本（`L1` 技术实现 5），`redteam` 不抽样，PR 与夜间红线覆盖不变。`holdout` 负责人由 QA 负责人担任，不参与提示词、加载策略与规则下沉的调优；样本内容仅负责人可读（私有仓权限 + `evalkit` 读权限双控）；发版报告中 `holdout` 的翻转与失败样本只给 ID 与打分器名，不给内容。
10. 制定月度复核流程：每月抽 20 个 trial 专家复核，一致性下降即回修。

**交付物**：三个 judge 及评分标准；校准集与校准报告；人工队列；`holdout` 套件与访问控制。

**验收**

1. 每个 judge 有表驱动单测（录制回放，不访问百炼），覆盖 1/2/3 分与 `UNKNOWN`。
2. 校准报告给出三个维度的加权 Kappa 与区间、混淆矩阵、专家间 Kappa、`UNKNOWN` 占比；达标维度标记「可进门禁」，未达标维度门禁配置中为关闭。
3. judge 与 `citation_valid` 结论冲突的 trial 100% 出现在人工队列。
4. 非 `holdout` 负责人账号读取 `holdout` 样本被拒绝；`git log` 检查 `eval/suites/` 公开历史中无任何 `holdout` 样本 ID。

**技术实现**

① 流程

1. `evalkit/grader/judge` 按维度组装材料（fixture、所引知识原文、专家标准结论、最终输出），套 `eval/judges/<维度>/prompt.md` 调 judge 模型，解析 `{score, evidence, verdict}`；`evidence` 不是被评文本原句时记 `grader_error`。
2. 出现 `UNKNOWN`、flaky、与代码打分器冲突、或命中通过样本 10% 随机抽检时，写一条 `review_task(HUMAN_QUEUE)`。
3. 校准：`cmd/eval judge calibrate --dim {维度}` 从校准实验按任务 5 分层抽 ≥ 100 个 trial 生成 `CALIBRATION` 任务（`rater_no=1`），再随机 25% 生成 `rater_no=2` 的双标任务派给另一位专家；专家在评审页打分写 `expert_score`；任务做完后算 judge-专家与专家间的二次加权 Kappa（bootstrap 1000 次）与混淆矩阵，报告存 MinIO `agri-steward-eval/report/calibration/{维度}/{judge_version}.json`，区间下限达标则以 PR 修改 `eval/judges/gate.yaml` 打开该维度。
4. 月度复核：`cmd/eval judge recheck` 每月抽 20 个 trial 生成 `MONTHLY_RECHECK` 任务，按第 3 步算一致性。
5. `holdout` 放独立私有仓，以 git submodule 挂到 `eval/suites/holdout/`；`evalkit` 读取前校验调用者在 `eval/holdout_readers.yaml` 中（只列 `holdout` 负责人与 CI 服务账号）；`holdout` trial 的 transcript 与报告明细写 `agri-steward-eval/report/holdout/` 前缀，仅负责人可读。

② SQL（`agri_eval` 库）

```sql
-- migrations/eval/000N_review_task.up.sql（版本号按合入顺序领取，排在 L7-a 的 experiment_gate 之后）
-- 专家任务队列，专家打分即金标准，不另建标注表
CREATE TABLE review_task (
  id            bigint unsigned NOT NULL AUTO_INCREMENT,
  task_type     varchar(16)  NOT NULL,  -- CALIBRATION / HUMAN_QUEUE / MONTHLY_RECHECK / BACKFLOW_EXPECT
  queue_reason  varchar(16)  NULL,      -- UNKNOWN / FLAKY / CONFLICT / RANDOM_10
  experiment_id bigint unsigned NULL, sample_id varchar(64) NULL, trial_index smallint NULL,
  draft_ref     varchar(128) NULL,      -- BACKFLOW_EXPECT：回流草稿 ID（L8）
  dimension     varchar(32)  NULL,
  judge_version varchar(32)  NULL,
  judge_score   tinyint      NULL,      -- NULL 表示 UNKNOWN
  rater_no      tinyint      NOT NULL DEFAULT 1,  -- 1 主标 / 2 双标
  expert_score  tinyint      NULL,
  assignee_id   bigint unsigned NULL,
  status        varchar(16)  NOT NULL,  -- OPEN / ASSIGNED / DONE / SKIPPED
  ref_key       char(32)     NOT NULL,  -- md5(task_type|experiment_id|sample_id|trial_index|dimension|draft_ref|rater_no)
  created_at    datetime(3)  NOT NULL, done_at datetime(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_ref (ref_key),
  KEY idx_assignee (assignee_id, status),
  KEY idx_calib (task_type, dimension, judge_version, status),
  KEY idx_trial (experiment_id, sample_id, trial_index)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO review_task (...) VALUES (...) ON DUPLICATE KEY UPDATE id = id;  -- 重跑不重复派单

-- judge 与专家的混淆矩阵（排除 UNKNOWN，只取主标）
SELECT judge_score, expert_score, COUNT(*) FROM review_task
WHERE task_type = 'CALIBRATION' AND dimension = ? AND judge_version = ? AND status = 'DONE'
  AND rater_no = 1 AND judge_score IS NOT NULL
GROUP BY judge_score, expert_score;

-- 专家间混淆矩阵（可达上限）
SELECT a.expert_score, b.expert_score, COUNT(*)
FROM review_task a JOIN review_task b
  ON b.experiment_id = a.experiment_id AND b.sample_id = a.sample_id AND b.trial_index = a.trial_index
 AND b.task_type = a.task_type AND b.dimension = a.dimension AND b.judge_version = a.judge_version
 AND b.rater_no = 2 AND b.status = 'DONE'
WHERE a.task_type = 'CALIBRATION' AND a.dimension = ? AND a.judge_version = ? AND a.rater_no = 1 AND a.status = 'DONE'
GROUP BY a.expert_score, b.expert_score;

-- UNKNOWN 占比
SELECT AVG(judge_score IS NULL) FROM review_task
WHERE task_type = 'CALIBRATION' AND dimension = ? AND judge_version = ? AND rater_no = 1;
```

③ Redis 键：无（judge 调用并发由 `evalkit/runner` 按百炼 QPM 控制，见 `L7`）。

### 6.4 L3 可信度指标体系与日报

**目标**：六个可信度指标有唯一口径、唯一计算位置，每天出数，任何一个数字都能下钻到具体样本与 `run_id`。

**涉及模块**：`internal/evalkit/reduce/`、`internal/evalkit/report/`、`internal/agent/harness/callbacks/`（线上指标埋点）、L7 看板。

**功能任务**

1. 写《可信度指标口径表》，每个指标固定：定义、分子、分母、数据来源（离线 `evalkit` / 线上事件）、对应打分器、统计 reducer：
   - 引用覆盖率（母文档 §2 口径）：`outcome=ADVICE` 的卡中，每个结论字段都有有效 `Citation` 的占比 ← `citation_valid` 的「覆盖 + 版本有效」部分；`must_cite ⊆ actual` 单列为「引用正确率」，只出报告；
   - 事实一致率：`fact_consistency=3` 的出卡 trial ÷ judge 结果非 `UNKNOWN` 的出卡 trial；
   - 数值准确率 ← `numbers_match_calc`；
   - 幻觉率：［`expected_behavior` 为拒答而实际既未拒答也未降级的 trial + `fact_consistency=1` 的出卡 trial］÷ 全部有效 trial（剔除 `infra_error` 与 judge `UNKNOWN`）；
   - 拒答正确率、过度保守率 ← `refusal_correct` 的两个分支。
   - 事实一致率与幻觉率互斥不互补：同一 trial 不会同时计入两者分子，2 分只进分母；两者共用同一 judge 判定，Gate 2 第 3 条里只算一项证据。
2. 分母口径统一：`infra_error` 不计入分母、单独统计；judge `UNKNOWN` 从分子分母同时剔除，占比单列，超过 10% 时该指标标「不可判」；`grader_error` 存在时当次结果不出日报（评测设计方案 §6-6、§11.3-4）。
3. 比例类指标用 Wilson 区间，均值类按样本 bootstrap；区间按样本而非 trial 计算。judge 维度在 `L2` 校准达标前只出报告、标「未校准」，不与校准后的数据做差值。
4. 线上侧：从 `advice_card`（`outcome / refuse_reason / degraded`）与 `advice_citation` 计算引用覆盖率、数值准确率（`VALIDATION_FAILED` 拒收占比）、拒答率的线上值；事实一致率线上值按每日抽样 judge + 专家巡检给出，标注「抽样估计」。
5. 日报：离线（最近一次夜间全量）与线上两栏并列，每个指标给点估计、区间、相对 `baseline_p1.r2` 差值；按 `category / difficulty / 陷阱组` 切片。门禁只看 `all` 切片；切片差值只出报告，标红前做 Holm 校正。
6. 每个指标数字可点开：离线跳到样本与 trial `run_id`，线上跳到 `run_id` 详情页。

**交付物**：口径表；`reduce` 指标实现；日报（自动发送）；下钻链接。

**验收**

1. `reduce` 统计函数有已知答案单测（如 n=5、c=4 时 pass^3 = 0.4）。
2. 日报连续 5 个工作日自动产出，六指标齐全；人工对账一次日报与原始 trial 记录，口径一致并留记录。
3. 任取日报中一个异常值，2 次点击内定位到具体 `run_id`。

**技术实现**

① 流程

1. 离线：夜间全量结束后 `evalkit/reduce` 先把每个样本 n 次 trial 合成样本级值，再算 Wilson / bootstrap（1000 次，`seed` 进报告），结果写 MinIO `agri-steward-eval/{experiment_id}/metrics.json`；存在 `grader_error` 时不写，日报离线栏显示「本期作废」。
2. 线上：worker 任务 `quality-metric-rollup`（每日，取 `lock:p2job:quality-metric-rollup`）逐农场读 `advice_card`（走新增 `idx_scope_time`）与 `advice_citation`，按农场时区（`farm_profile.timezone`，母文档 §4.6.2-9）切出前一日数据按切片汇总，并导入最近一次夜间全量的 `metrics.json`，写 `quality_metric_daily`（农场行 + 平台汇总行 `tenant_id=0, farm_id=0`）。离线评测、judge、专家结果都不写 `run_event`（母文档 §4.6.2-7）。
3. 线上事实一致率：worker 任务 `online-judge-sample` 对前一日 `outcome=ADVICE` 的卡按 `category` 定额抽样，先从生产配额 `rl:model:{tier}` 取令牌、再受 `rl:judge:{tier}` 上限约束后调 judge（公共部分放 `internal/service/quality/judge`，`evalkit` 包装复用），分数写 `online_judge_result`；`UNKNOWN` 与 1 分置 `NEED_EXPERT` 推专家巡检，专家结论回写同一行；汇总行 `is_estimate=1`。judge 不可用时当日线上值标「未评」。
4. 日报：汇总完成后生成 HTML 存 MinIO `agri-steward-eval/report/daily/{yyyymmdd}.html`（母文档 §4.6.2-8），钉钉推送摘要；报告只含 ID 与聚合值，离线单元格链到 `report.html#sample-{id}`，线上链到 `/runs/{run_id}`。

② SQL（版本号按合入顺序领取，母文档 §4.6.1 表约定 8）

```sql
-- migrations/000N_quality_metric_daily.up.sql
-- 平台表例外（母文档 §4.6.1 表约定 3）：tenant_id=0, farm_id=0 为平台汇总行
CREATE TABLE quality_metric_daily (
  id          bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id   bigint unsigned NOT NULL,
  farm_id     bigint unsigned NOT NULL,
  stat_date   date         NOT NULL,
  source      varchar(8)   NOT NULL,   -- ONLINE / OFFLINE
  metric      varchar(48)  NOT NULL,   -- 口径表中的指标名
  slice_key   varchar(96)  NOT NULL,   -- 'all' 或 'category=植保;difficulty=hard'
  numerator   int NOT NULL, denominator int NOT NULL,
  point       decimal(7,5) NOT NULL, ci_low decimal(7,5) NULL, ci_high decimal(7,5) NULL,
  is_estimate tinyint(1)   NOT NULL DEFAULT 0,  -- 抽样估计
  experiment_id bigint unsigned NULL,  -- OFFLINE 行对应的评测实验
  created_at  datetime(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_metric (tenant_id, farm_id, stat_date, source, metric, slice_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 线上 judge / 专家结果（不进 run_event）
CREATE TABLE online_judge_result (
  id            bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id     bigint unsigned NOT NULL,
  farm_id       bigint unsigned NOT NULL,
  run_id        bigint unsigned NOT NULL,
  advice_id     bigint unsigned NOT NULL,
  stat_date     date         NOT NULL,   -- 农场时区下的业务日
  category      varchar(8)   NOT NULL,
  dimension     varchar(32)  NOT NULL,
  judge_version varchar(32)  NOT NULL,
  judge_score   tinyint      NULL,       -- NULL 表示 UNKNOWN
  expert_score  tinyint      NULL,
  expert_id     bigint unsigned NULL,
  status        varchar(16)  NOT NULL,   -- SCORED / NEED_EXPERT / REVIEWED
  row_ver       int          NOT NULL DEFAULT 0,
  created_at    datetime(3)  NOT NULL, updated_at datetime(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_advice_dim (tenant_id, farm_id, advice_id, dimension, judge_version),
  KEY idx_scope_date (tenant_id, farm_id, stat_date, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

ALTER TABLE advice_card ADD KEY idx_scope_time (tenant_id, farm_id, created_at), ALGORITHM=INPLACE, LOCK=NONE;

INSERT INTO quality_metric_daily (tenant_id, farm_id, stat_date, source, metric, slice_key, numerator, denominator, point, ci_low, ci_high, ...)
VALUES (...) ON DUPLICATE KEY UPDATE numerator = VALUES(numerator), denominator = VALUES(denominator),
  point = VALUES(point), ci_low = VALUES(ci_low), ci_high = VALUES(ci_high);  -- 补跑覆盖同日结果

-- 线上汇总（单农场一日）
SELECT outcome, refuse_reason, degraded, COUNT(*) FROM advice_card
WHERE tenant_id = ? AND farm_id = ? AND created_at >= ? AND created_at < ?
GROUP BY outcome, refuse_reason, degraded;

-- 抽样可复现：按 category 定额
SELECT id, run_id FROM advice_card
WHERE tenant_id = ? AND farm_id = ? AND category = ? AND outcome = 'ADVICE' AND created_at >= ? AND created_at < ?
ORDER BY CRC32(CONCAT(id, ?)) LIMIT ?;

INSERT INTO online_judge_result (...) VALUES (...) ON DUPLICATE KEY UPDATE id = id;  -- 补跑不重复评
UPDATE online_judge_result SET expert_score = ?, expert_id = ?, status = 'REVIEWED', row_ver = row_ver + 1, updated_at = ?
WHERE tenant_id = ? AND farm_id = ? AND id = ? AND row_ver = ?;
```

③ Redis 键（键名省略公共前缀 `as:{env}:`，见母文档 §4.6.1 Redis 约定）

| 键 | TTL | 用途 |
|----|-----|------|
| `rl:judge:{tier}` | 2min（令牌桶，按分钟滚动） | 线上抽样 judge 的 QPM 上限；judge 同时从生产 `rl:model:{tier}` 取令牌，不另占百炼配额 |
| `lock:p2job:{job}` | 10min，执行中续期 | P2 批任务单实例互斥，防止手动补跑与定时任务并发 |

### 6.5 L4 规则与模型职责再切分

**目标**：找出模型仍在隐式承担的确定性判断，逐项下沉到规则层，把模型职责收窄为「情境理解 + 方案选择 + 解释生成」，并用指标证明每一项下沉的效果。

**涉及模块**：`internal/agent/calc/`、`internal/agent/guardrail/`（`output_validator`）、`internal/agent/graph/`（compose Graph 节点）、`knowledge/common/`（`operation_window.md / safety.md`）。

**功能任务**

1. 职责盘点：用 Phase 0-1 的 `run_id` 回放与驳回原因（时机不对 / 药剂不对 / 用量不对）定位模型输出中的确定性判断，形成「待下沉清单」，每项注明证据 `run_id` 与涉及样本。
2. 候选下沉项（以盘点结果为准）：作业窗口筛选、混用禁忌判断、多病虫害并发时的优先级排序。
3. 覆盖率核查：生育期判定、PHI 推算、`product_candidates` 已在 Phase 0 由规则层承担，核查是否仍有路径让模型绕过（如解释文本中出现规则层未给出的时间或药剂）。
4. 每个下沉项分两个 PR：先合入规则实现（进 `calc` 或 `guardrail`）与 `output_validator` 的拒收规则，拒收规则按红线资产走双人签核与回归，合入即常开、不受任何开关与农场覆盖影响（母文档 §4.6.2-10）；再合入 compose Graph 中把规则结果作为模型节点输入约束的注入逻辑。
5. 注入逻辑用开关 `rule_offload.<项名>` 控制，值按母文档 §4.6.2-5 写 `{"mode":"off|shadow|on"}`（默认 `off`）：`on` 注入，`shadow` 只算不注入、做影子比对，`off` 不注入；三种取值下拒收都生效。切到 `on` 即 candidate，按 §8 跑配对对比。
6. 每项记录指标变化：六可信度指标 + 驳回原因分布 + token 成本 + P95，写入候选因子登记表。
7. 混用禁忌（`tank_mix`）属于红线：P2 新增红线打分器 `conflict_blocked` 与红线样本（只覆盖禁忌混用）；P3 L5 的 `conflict_checker` 复用本项 `guardrail` 规则实现并扩展该打分器，不另起一套。

**交付物**：待下沉清单；逐项规则实现与单测；指标变化记录；`conflict_blocked` 打分器与红线样本。

**验收**

1. 每个下沉项有表驱动单测，且至少一条「模型给出与规则冲突的结论时被拒收」的用例；`rule_offload.*` 取 `off` 时该用例仍拒收。
2. 每个下沉项都有一份 `cmd/eval compare` 单因子配对报告，可信度指标无显著退化。
3. 覆盖率核查结论成文：Phase 0 已下沉的三项无绕过路径，或已修复。
4. 关闭任一 `rule_offload.*` 开关，系统回到「只拒收、不注入」，回归与红线全绿。

**技术实现**

① 流程

1. 建议主链路（`internal/agent/graph`）在模型节点前按农场读开关 `rule_offload.<项名>`（P1 `feature_flag`，三态取值与合并规则见母文档 §4.6.2-5，经新增读取函数 `service/flag.Mode` 返回 `off / shadow / on`）。
2. 不论开关取值，`internal/agent/calc` 的规则实现（作业窗口、混用禁忌、优先级排序）都会执行，`internal/agent/guardrail` 的 `output_validator` 按同一结果拒收冲突输出，记 `VALIDATION_FAILED`。
3. `on`：规则结果作为模型节点的输入约束注入。
4. `shadow`：不注入；模型输出后比对两者，在落卡同一事务写 `rule_shadow_diff` 并发 `RULE_SHADOW_DIFF` 事件（需先登记到 P0 L7 枚举），供切到 `on` 前评估差异率。
5. `off`：不注入、不比对，只保留拒收。
6. 离线对比：candidate 实验的 `snapshot` 记录各项开关取值，`cmd/eval compare` 按单因子比对。

② SQL（版本号按合入顺序领取）

```sql
-- migrations/000N_rule_shadow_diff.up.sql
CREATE TABLE rule_shadow_diff (
  id           bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id    bigint unsigned NOT NULL,
  farm_id      bigint unsigned NOT NULL,
  run_id       bigint unsigned NOT NULL,
  advice_id    bigint unsigned NOT NULL DEFAULT 0,  -- 0 表示该轮未出卡
  item         varchar(32)  NOT NULL,  -- operation_window / tank_mix / pest_priority
  agreed       tinyint(1)   NOT NULL,  -- 模型输出与规则结论是否一致
  diff_kind    varchar(32)  NULL,      -- WINDOW_OUTSIDE / FORBIDDEN_MIX / ORDER_MISMATCH
  rule_output  json         NOT NULL,
  model_output json         NOT NULL,  -- 对应部分的掩码摘要
  rule_ver     varchar(16)  NOT NULL,
  created_at   datetime(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_run_item (tenant_id, farm_id, run_id, advice_id, item),
  KEY idx_scope_item (tenant_id, farm_id, item, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO rule_shadow_diff (...) VALUES (...) ON DUPLICATE KEY UPDATE id = id;  -- resume 重放幂等

-- 影子期差异率
SELECT item, AVG(agreed = 0) FROM rule_shadow_diff
WHERE tenant_id = ? AND farm_id = ? AND created_at >= ? GROUP BY item;

-- 保留期清理（P1 保留期配置，默认 180 天；cmd/worker 保留期任务逐农场执行）
DELETE FROM rule_shadow_diff WHERE tenant_id = ? AND farm_id = ? AND item = ? AND created_at < ? LIMIT 5000;
```

本表是诊断数据而非只追加表，不进母文档 §4.6.1 只追加清单；清理沿用 P1 保留期任务。

③ Redis 键：无（开关读取沿用 P1 的进程内缓存与 `flags:changed` 失效）。

### 6.6 L5 知识加载策略优化

**目标**：让 `progressive_loader` 加载得「全、准、少」—— 该加载的不漏、加载的大多被引用、无关条目不进上下文，并且策略可配置、可 A/B、可回滚。

**涉及模块**：`internal/agent/knowledge/`（`progressive_loader`）、`knowledge/index.yaml`、加载策略配置 `configs/loader/{版本}.yaml` 与开关 `enable_loader_v1`、`internal/agent/harness/callbacks/`（加载事件埋点）。

**功能任务**

1. 加载事件埋点：每次加载记录 `run_id / 问题类型 / 情境维度 / 加载的 kb_id@version 列表 / 排序依据`；结合 `AdviceCard.citations[]` 计算三个新指标：
   - 加载命中率：样本 `must_cite` 中被加载到的比例；
   - 加载精度：加载条目中真正被 `Citation` 引用的比例，分母排除 `MUST_INCLUDE` 与 `COMMON` 类条目（强制加载的公共条目天然不被引用）；
   - 无关知识注入率：加载条目中与样本标注的情境维度（作物 / 生育期 / 问题类型）不匹配的比例，由 `evalkit` 按样本标注离线计算，不由生产 loader 自评。
2. 三个指标接入 `L3` 日报与 `evalkit` 报告，按问题类型切片。
3. 按问题类型定制加载规则，写进 `configs/loader/{版本}.yaml`：如植保类必加 `common/safety.md` 与 `common/operation_window.md`，施肥类必加对应 `fertilization.md` 公式段。
4. 加载上限（Phase 0 建议 ≤ 5 条）与排序规则（权威等级、生育期匹配度、版本新旧）参数化，进同一配置。
5. A/B：加载策略以版本号区分（`v0` = Phase 0 策略，`v1` = 新策略），离线用 `cmd/eval compare` 单因子对比；线上用布尔开关 `enable_loader_v1` 按农场灰度（母文档 §4.6.2-5）。
6. 高频知识缺失拒答（`refuse_reason=KB_MISS`）与加载命中率低的切片，输出给 `L8` 归因清单，驱动知识条目补齐而不是放宽加载。

**交付物**：加载埋点与三项指标；按问题类型的 `configs/loader/v1.yaml`；离线与线上对比报告。

**验收**

1. 三项加载指标在 `regression` 全量上出数，与人工抽查 20 条样本的结果一致。
2. 新策略相比 `v0`：加载命中率不下降，可信度指标无显著退化（配对差值区间不跨 0 或为正向）。
3. 关闭 `enable_loader_v1` 回到 `v0` 只改配置、不发版，回归全绿。

**技术实现**

① 流程

1. `internal/agent/graph` 进入知识加载步骤时按农场读布尔开关 `enable_loader_v1`（P1 `feature_flag`，`{"on":bool}`，默认关），开则用 `v1`、否则 `v0`，写入 `agent_run.loader_strategy`。
2. `internal/agent/knowledge` 的 `progressive_loader` 按 `configs/loader/{版本}.yaml` 执行：先放问题类型必加条目（`MUST_INCLUDE`），再按权威等级、生育期匹配度、版本新旧排序截断到上限。
3. 加载结果经 harness callbacks 发 `KB_LOADED` 事件，同时批量写 `kb_load_item`（一个条目一行）。
4. `quality-metric-rollup` 关联 `kb_load_item` 与 `advice_citation` 计算线上加载精度（分母排除 `MUST_INCLUDE / COMMON`）；加载命中率与无关知识注入率需要样本标注，只在 `evalkit` 离线计算。三项进 `L3` 日报。
5. 加载命中率低的切片与高频知识缺失拒答（`refuse_reason=KB_MISS`）由 `attribution-weekly` 读入 `L8` 归因清单。

② SQL（版本号按合入顺序领取）

```sql
-- migrations/000N_agent_run_p2.up.sql（L5 / L6 共用）
ALTER TABLE agent_run
  ADD COLUMN loader_strategy   varchar(16) NOT NULL DEFAULT 'v0',
  ADD COLUMN prompt_tokens     int         NOT NULL DEFAULT 0,  -- 本 run 全部 LLM 调用累计，只用于成本统计，不作压缩触发依据
  ADD COLUMN completion_tokens int         NOT NULL DEFAULT 0,
  ADD COLUMN compaction_count  smallint    NOT NULL DEFAULT 0,
  ADD COLUMN prev_run_id       bigint unsigned NULL,            -- L6 会话链：同一会话的上一个 run
  ALGORITHM=INSTANT;

-- migrations/000N_kb_load_item.up.sql：诊断数据，非只追加表，保留期走 P1 保留期配置（默认 180 天）
CREATE TABLE kb_load_item (
  id            bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id     bigint unsigned NOT NULL,
  farm_id       bigint unsigned NOT NULL,
  run_id        bigint unsigned NOT NULL,
  load_seq      smallint     NOT NULL,  -- 同一 run 内第几次加载
  rank_no       smallint     NOT NULL,  -- 排序名次，从 0 开始
  question_type varchar(32)  NOT NULL,
  kb_id         varchar(96)  NOT NULL,  -- 知识条目路径
  kb_version    varchar(16)  NOT NULL,
  pick_reason   varchar(16)  NOT NULL,  -- MUST_INCLUDE / RANKED / COMMON
  strategy      varchar(16)  NOT NULL,
  created_at    datetime(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_run_load (tenant_id, farm_id, run_id, load_seq, rank_no),
  KEY idx_scope_time (tenant_id, farm_id, created_at),
  KEY idx_kb (tenant_id, farm_id, kb_id, kb_version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO kb_load_item (...) VALUES (...), (...) ON DUPLICATE KEY UPDATE id = id;  -- resume 重放幂等

-- 线上加载精度（单农场一日）
SELECT COUNT(DISTINCT CASE WHEN c.id IS NOT NULL THEN k.id END) / COUNT(DISTINCT k.id) FROM kb_load_item k
LEFT JOIN advice_card a ON a.tenant_id = k.tenant_id AND a.farm_id = k.farm_id AND a.run_id = k.run_id
LEFT JOIN advice_citation c ON c.tenant_id = a.tenant_id AND c.farm_id = a.farm_id AND c.advice_id = a.id
  AND c.kb_id = k.kb_id AND c.kb_version = k.kb_version
WHERE k.tenant_id = ? AND k.farm_id = ? AND k.created_at >= ? AND k.created_at < ? AND k.pick_reason = 'RANKED';

-- 保留期清理（cmd/worker 保留期任务逐农场执行）
DELETE FROM kb_load_item WHERE tenant_id = ? AND farm_id = ? AND created_at < ? LIMIT 5000;
```

③ Redis 键：无。

### 6.7 L6 长对话压缩 `compaction`

**目标**：会话超过 token 阈值时自动压缩历史，降低长会话成本，同时保证地块、作物、生育期、用户口述禁忌、审批与驳回状态、否决药剂、时效实测数据、已确认结论这些关键事实一条不丢、结论不漂移。

**涉及模块**：`internal/agent/harness/`（新增 `compaction` 子包，挂在「上下文组装」步骤，见母文档 §4.6 借鉴设计②）、`RunState.compaction_state`、`internal/agent/harness/memory/`（`working` 层）、开关 `enable_compaction`。

**功能任务**

1. 阈值设定：从 Phase 0-1 线上 `audit_log` 与 trace 统计单次上下文组装的 token 长度分布，取分布依据定阈值并写入配置；阈值与分布图一起入评审记录。
2. 结构化摘要 schema（存 `compaction_state`）：`facts`（见下条）`/ summarized_until_step_id / compaction_version`。
3. 关键事实白名单：`field_id / crop / phenology_stage`；用户口述约束与禁忌（如「这块地不能用某药」「旁边是蜂场 / 鱼塘」，原句保留）；本会话涉及工单的审批与票据状态及驳回原因；已否决药剂；带时效的实测数据（含取数时间）；已确认结论（含结论正文、剂量、药剂、窗口，不只存摘要）；已引用的 `kb_id`。这些字段由代码从业务表与结构化状态提取，**不交给模型改写**；模型只负责压缩自由文本对话，白名单字段禁止被丢弃或改写。
4. 压缩后原文仍可回放：被压缩的原始消息经 `RawStore.Put` 存 MinIO 并登记 `raw_object`，`compaction_state` 只存引用 ID，按 `run_id` 可还原。
5. 会话边界与 `resume` 兼容：同一 run 内多轮 `WAIT_INPUT` 天然共享压缩状态；跨 ask 的长会话由新 ask 带 `prev_run_id` 续接，`compaction_state` 复制到新 run；`superseded_by_run_id` 产生的新 run 同样复制。从带 `compaction_state` 的检查点恢复时，上下文组装使用摘要 + 未压缩部分，行为与未压缩一致。
6. 指标：压缩触发率、白名单字段保留率、压缩前后结论一致率、压缩前后单会话 token 成本。
7. 评测：多轮长会话样本（脚本用户为主，`L7` 模拟用户样本补充），同一样本在 `enable_compaction` 关 / 开下配对运行；另设专项：压缩前后对同一追问的建议由 `numbers_match_calc`、`product_in_candidates`、`outcome_match` 等代码打分器比对。
8. `enable_compaction`（`{"on":bool}`）默认关，离线达标后按农场灰度。

**交付物**：`compaction` 实现与单测；阈值评审记录；专项评测报告。

**验收**

1. 白名单字段保留率 100%：代码打分器在最终组装的 prompt 上逐字段校验，取值与业务表重新读出的值比对（不与 `facts` 自身比对）。
2. 长会话样本压缩前后 `outcome_match` 与 `conclusion_match` 结论一致；专项追问比对无差异；红线打分器全过。
3. 专项报告给出压缩前后单会话 token 成本的实测对比。
4. 从压缩后的检查点重启进程再 `resume`，恢复成功，已完成步骤零重复执行；带 `prev_run_id` 的新 ask 与 superseded 新 run 都能拿到压缩状态。
5. `enable_compaction` 关闭时行为与 Phase 1 完全一致，回归全绿。

**技术实现**

① 流程

1. `internal/agent/harness/compaction` 挂在「上下文组装」步骤：`enable_compaction` 开启且本次待组装上下文的估算 token 数超过 `compaction.trigger_tokens` 时触发；压缩后估算值回落，不会每轮重复触发。`agent_run.prompt_tokens` 只做成本统计，不作触发依据。
2. 会话边界：同一 run 内多轮 `WAIT_INPUT` 共享 `RunState`；`POST /api/v1/farms/{farm_id}/ask` 可带 `prev_run_id`，服务端校验其 `tenant_id / farm_id / field_id / user_id` 一致、状态为终态、`updated_at` 在 `compaction.session_idle`（默认 30min）内，通过则在 INSERT 新 `agent_run` 的同一事务复制 `compaction_state` 并写 `prev_run_id`；不满足则开新会话。resume 产生 `superseded_by_run_id` 新 run 时同样复制。
3. 代码提取白名单写 `facts`：`user_constraints[]` 取用户轮中命中 `policy/user_constraint_lexicon.yaml` 的原句；`orders[]` 取本会话工单与 `approval_ticket` 状态、驳回原因（P1 `rejection_reason`）；`vetoed_products[]` 取被驳回或用户否决的药剂；`measured[]` 取 `tool_call` 中 `time_sensitive=1 AND superseded_at IS NULL` 的 `result_summary` 与取数时间；`confirmed_conclusions[]` 取已落卡的结论正文。保留最近 `compaction.keep_recent_turns`（默认 4）轮原文，更早的自由文本交轻量档模型压成 `summary`（≤ `compaction.summary_max_tokens`）。
4. 被压缩的原始消息经 `RawStore.Put(kind=COMPACTED_MSGS)` 写入（key 格式见母文档 §4.6.2-8，`kind` 需登记到 P0 `raw_object.kind`）；`compaction_state.raw_refs` 只存 `raw_object.id`。
5. 断言：对最终组装的 prompt 逐字段校验白名单，取值从源表重新读取，任一字段缺失或不一致即放弃本次压缩，发 `COMPACTION_FAILED`，用完整上下文继续；通过则发 `COMPACTION_APPLIED`（含前后 token 数）。两个事件类型需先登记到 P0 L7 枚举。
6. 持久化：`compaction_state` 是 `RunState` 的一部分，随检查点 payload 写入；`agent_run.compaction_state` 在同一事务更新（即 P0 中断时 INSERT `run_checkpoint` + UPDATE `agent_run` 的那个事务）；run 置终态的 UPDATE 也一并写入，供 `prev_run_id` 续接读取。其余时间只存在内存，与 P0「只在中断时写检查点」一致。
7. `resume(run_id)`：以检查点 payload 中的 `compaction_state` 为准；`compaction_version` 不识别时视为无压缩、用完整上下文；识别则用 `summary` + `summarized_until_step_id` 之后的原文组装。

`compaction_state` 结构（`schemas/compaction_state.v1.json`）：`compaction_version`、`facts{field_id, crop, phenology_stage, user_constraints[{text, turn_step_id}], orders[{order_id, status, ticket_id, ticket_status, reject_reason_code, reject_text}], vetoed_products[{product, source, ref_id}], measured[{tool_call_id, tool_name, result_summary, collected_at}], confirmed_conclusions[{advice_id, conclusion, numbers, selected_product, window}], cited_kb_ids[]}`、`summary{text, tokens, prompt_version}`、`summarized_until_step_id`、`raw_refs[]`、`history[{at, tokens_before, tokens_after}]`。

② SQL（列已在 P0 `agent_run.compaction_state` 预留、L5 的 `000N_agent_run_p2` 加计数列与 `prev_run_id`，无新表）

```sql
-- 中断事务内（与 INSERT run_checkpoint 同一事务）；run 置终态时同样带上 compaction_state
UPDATE agent_run
SET compaction_state = ?, compaction_count = ?, status = ?, row_ver = row_ver + 1, updated_at = ?
WHERE tenant_id = ? AND farm_id = ? AND id = ? AND row_ver = ?;

-- 续接会话：校验上一个 run，并在 INSERT 新 run 时复制
SELECT compaction_state FROM agent_run
WHERE tenant_id = ? AND farm_id = ? AND id = ? AND field_id = ? AND user_id = ?
  AND status IN ('COMPLETED', 'FAILED', 'CANCELED') AND updated_at >= ?;

-- 白名单取数示例：时效实测数据
SELECT id, tool_name, result_summary, created_at FROM tool_call
WHERE tenant_id = ? AND farm_id = ? AND run_id IN (?) AND time_sensitive = 1 AND superseded_at IS NULL;

-- 每次 LLM 调用后累计 token（LLM_CALL_END 事件同时带这两个值）；计数器更新不升 row_ver，与 P0 event_seq 一致
UPDATE agent_run SET prompt_tokens = prompt_tokens + ?, completion_tokens = completion_tokens + ?
WHERE tenant_id = ? AND farm_id = ? AND id = ?;

-- 压缩触发率（日报，走 idx_scope_status）
SELECT AVG(compaction_count > 0) FROM agent_run
WHERE tenant_id = ? AND farm_id = ? AND status IN ('COMPLETED', 'FAILED', 'CANCELED') AND updated_at >= ? AND updated_at < ?;
```

③ Redis 键：无（压缩状态以检查点与 `agent_run` 为准）。

### 6.8 L7 回归门禁、成本延迟门禁与 CI

**目标**：提示词、模型档位、加载策略、规则层、知识版本任一变更都必须过同一道门；红线退化无条件阻断，质量退化有统计依据地阻断，成本延迟劣化必须专项说明。

**涉及模块**：`Makefile`（`make eval`）、`cmd/eval run / compare / resume`、CI 流水线配置、`internal/evalkit/runner/`（并发、限流、预算）、`internal/evalkit/usersim/`（LLM 模拟用户）。

**功能任务**

`L7-a`（第 2 周，红线门禁先行）

1. PR 冒烟：改动 `internal/` 或 `knowledge/` 的 PR 自动跑 `regression` 冒烟子集（约 30 条，覆盖全部红线打分器），n=3，红线 pass^3 < 1 即阻断合并。
2. 每晚全量：`regression + redteam + capability`，n=5，结果入库并出日报；失败次日修复，不阻断合并。
3. 实验护栏：每次实验设 token 总预算，超预算即停并标记未完成；`infra_error` 占比超阈值（初步参考 5%）或存在 `grader_error` 时实验作废，不得用于门禁判定。
4. trial 级并发按百炼 QPM 配置并留 30% 余量；`cmd/eval resume` 只补跑未完成与 `infra_error` 的 trial。

`L7-b`（第 6-7 周，质量与成本延迟门禁）

5. 发版门禁：发版前或提示词 / 模型档位 / 加载策略 / 规则层 / 知识版本任一变更，跑全部套件含 `holdout`，n=5。
6. 质量退化阻断：任一可信度指标在 `all` 切片上相对 baseline 下降超阈值，且配对 bootstrap 差值区间不跨 0；阈值取 `L0` A/A 实测的 `eval/gates/thresholds.yaml`，切片结果只出报告。引用覆盖率或数值准确率 < 100% 无条件阻断。
7. judge 维度：`L2` 校准达标的维度并入门禁；Gate 2 第 8 条要求三个维度全部达标，未达标即 Gate 2 不通过，不带未达标维度出阶段。
8. 成本与延迟门禁：单会话 token 成本、P95、压缩触发率三项进门禁；劣化超阈值需专项说明，Owner 签字放行。
9. 配对报告：按样本列出翻转样本（退化 / 改进）、flaky 清单、`unexpected_call` 清单，门禁结论放第一行。
10. LLM 模拟用户：按评测设计方案 §9.2 实现 `persona / instruction / max_turns`、`###STOP###` 结束；模拟用户模型与智能体、judge 分开配置；新模拟样本先人工读 3 次 trial 对话后入库；含模拟用户的样本 n ≥ 5，只进 `capability` 与质量指标，不进红线门禁。

**交付物**：CI 三类触发；门禁判定逻辑；配对报告；模拟用户与首批多轮样本。

**验收**

1. 提交一个故意破坏 `citation_valid` 的 PR，CI 阻断合并并在报告中给出失败样本与 `run_id`。
2. 构造一次质量退化 candidate（如回退到劣化的提示词版本），发版门禁判定阻断，报告列出翻转样本。
3. 人为制造超阈值 `infra_error`，实验被标记作废，门禁不给结论。
4. 模拟用户样本入库前的人工阅读记录齐全；红线门禁配置中不含任何 `user_sim` 样本。
5. Gate 2 前门禁至少拦下 1 次真实回退，留 PR 与报告链接。

**技术实现**

① 流程

1. CI 按触发类型调用 `make eval MODE=smoke|nightly|release`，即 `cmd/eval run`：PR 冒烟 n=3（冒烟子集），夜间 n=5（`regression + redteam + capability`），发版 n=5（全部套件含 `holdout`）。
2. `evalkit/runner` 按百炼 QPM 留 30% 余量控制 trial 并发；每个 trial 从 schema 池取一个 `agri_eval_t_{n}` 临时 schema 跑业务迁移后执行；累计 token 超 `token_budget`（`NULL` 不限）即停，`status=BUDGET_EXCEEDED`；CI 触发的实验必须设预算。
3. 实验结束检查作废条件：`infra_error` 占比 > 5% 或存在 `grader_error` → `status=VOID`、写 `void_reason`，门禁不给结论；`cmd/eval resume` 只补跑未完成与 `infra_error` 的 trial。
4. `cmd/eval gate` 依次判定：红线 pass^k < 1 → `BLOCK_REDLINE`；引用覆盖率或数值准确率 < 100%，或 `all` 切片可信度指标下降超 `thresholds.yaml` 阈值且配对 bootstrap 区间不跨 0 → `BLOCK_QUALITY`；成本、P95、压缩触发率劣化超阈值 → `NEED_SIGNOFF`；其余 `PASS`。flaky 样本按样本级均值参与配对并在报告单列。judge 整体不可用时跳过 judge 维度，结论上限为 `NEED_SIGNOFF`。`compare_mode=PHASE` 的实验不写 `gate_verdict`。
5. `NEED_SIGNOFF` 放行：Owner 在 PR 中提交 `eval/signoffs/{experiment_id}.md`，CI 校验作者在 CODEOWNERS 中后写 `signed_by`。
6. 模拟用户（`evalkit/usersim`）作为 `UserChannel` 实现注入，与脚本用户同接口；红线门禁配置排除 `tags` 含 `user_sim` 的样本。

② SQL（`agri_eval` 库）

```sql
-- migrations/eval/000N_experiment_gate.up.sql（版本号按合入顺序领取；第 2 周合入，排在 L2 的 review_task 之前）
ALTER TABLE experiment
  ADD COLUMN void_reason  varchar(32)     NULL,  -- INFRA_ERROR_RATE / GRADER_ERROR / MULTI_FACTOR；status 新增取值 BUDGET_EXCEEDED / VOID
  ADD COLUMN gate_verdict varchar(16)     NULL,  -- PASS / BLOCK_REDLINE / BLOCK_QUALITY / NEED_SIGNOFF / NOT_ATTRIBUTABLE
  ADD COLUMN token_budget bigint          NULL,  -- NULL 表示不限，存量实验不受影响
  ADD COLUMN signed_by    bigint unsigned NULL,
  ALGORITHM=INSTANT;

SELECT SUM(status = 'INFRA_ERROR') / COUNT(*) FROM trial WHERE experiment_id = ?;  -- 作废判定
UPDATE experiment SET status = 'VOID', void_reason = ? WHERE id = ? AND frozen = 0;
```

③ Redis 键：无。评测 trial 的业务 Redis 键按母文档 §4.6.1 Redis 约定取 env=`eval-t{n}`（前缀 `as:eval-t{n}:`，与临时 schema 编号对齐），trial 结束按前缀删除。

### 6.9 L8 归因 Top 问题清单、线上回流与 Gate 2 收口

**目标**：把线上的驳回、改单、差评、拦截变成下一轮迭代的输入，让评测集与知识库持续跟着真实问题更新；最后按 Gate 2 八条收口。

**涉及模块**：`internal/service/approval/`（驳回原因结构化）、`audit_log` 与 MinIO 原文存储、`internal/evalkit/sample/`（回流草稿生成）、`knowledge/`、专家审核流程。

**功能任务**

1. 驳回与改单原因结构化：沿用 Phase 1 的五类原因，每条关联 `run_id / order_id / 涉及 kb_id`；审批页「其他」原因必须填写文字说明。
2. 归因周报（自第 2 周起每周产出）：按原因 × 问题类型 × 涉及知识条目聚合，输出 Top 问题清单；每条问题指派负责人与处置类型（补知识条目 / 改加载策略 / 下沉规则 / 改提示词），处置结果在下一期周报回填。
3. 线上回流：差评、专家判错、护栏拦截、高频知识缺失拒答的 `run_id`，经 `ReadRaw`（带审计）取当时的工具返回原文，连同 `audit_log` 中的 `clock`、`knowledge_ref`，自动生成样本 fixture 草稿。
4. 回流脱敏：农户身份、手机号替换为合成值，坐标粗化到网格，`farm_id / field_id` 换成随机合成码，映射只留业务库；草稿过 `pii_clean` 扫描才可提交。
5. 草稿由专家补写 `expect` 并署名后进 `capability`，稳定后毕业进 `regression`；每周回流量与入库量进周报。
6. Gate 2 收口：按 §4 逐条收集证据，产出《Phase 2 验收报告》，冻结 `baseline_p2`，输出《Phase 3 启动包》。

**交付物**：归因周报（≥ 4 期）；回流工具与入库样本；《Phase 2 验收报告》；`baseline_p2`；《Phase 3 启动包》。

**验收**

1. 归因周报连续 4 周产出，每条 Top 问题有负责人与处置结果。
2. ≥ 10 条知识条目因归因清单被补齐或修正，每条可追溯到对应周报条目。
3. 回流草稿 100% 通过 `pii_clean`；每周有入库样本。
4. Gate 2 八条证据齐全，`baseline_p2` 入库且不可修改。

**技术实现**

① 流程

1. 驳回：审批页提交时 `internal/service/approval` 在同一事务写 `rejection_reason`（含 `kb_ids`，取自 `advice_citation`）；农户采纳 / 差评经 `POST /api/v1/farms/{farm_id}/advices/{advice_id}/feedback` 写 `advice_feedback`（`request_id` 幂等，`comment` 落库前 `pii_mask`）。
2. 归因周报：worker 任务 `attribution-weekly`（每周一，取 `lock:p2job:attribution-weekly`）汇总上周驳回、差评、`VALIDATION_FAILED`、知识缺失拒答（`refuse_reason=KB_MISS`）与专家判错（`online_judge_result` / `review_task`），按 `原因 × 问题类型 × 主 kb_id` 算 `signature`，upsert `attribution_issue`（平台行 `tenant_id=0, farm_id=0`）；已 `RESOLVED / VERIFIED` 的问题再次命中时重开为 `OPEN` 并累加 `reopen_count`；周报 HTML 存 MinIO `agri-steward-eval/report/weekly/{week}.html`（母文档 §4.6.2-8）。
3. 处置：负责人在运营控制台填 `disposition / owner_id / resolution_ref`，带 `row_ver` 乐观锁更新；下一期周报回填处置结果，重开问题单列。
4. 回流：worker 任务 `backflow-collect`（每周）选出差评、专家判错、护栏拦截、高频知识缺失拒答的 run，置 `agent_run.backflow_status=SELECTED`；以服务账号调 `service.AuditService.ReadRaw` 逐条读取工具返回原文（每次读写 `RAW_READ` 审计），与 `audit_log` 中的 `clock`、`knowledge_ref` 生成 fixture 草稿；身份、手机号替换为随机合成值，坐标统一粗化到 0.01° 网格，`farm_id / field_id` 换成随机生成的评测合成码；真实 ID 与合成码的对应只写业务库 `agent_run.backflow_draft_ref`，随 `agent_run` 保留期清理，评测库与评测仓不留映射。草稿过 `pii_clean` 后推到评测仓 `eval/backflow/drafts/`，并为每份草稿建 `review_task(BACKFLOW_EXPECT)`；状态依次推进到 `DRAFTED / PII_FAILED / MERGED / DROPPED`。
5. 收口：`cmd/eval baseline freeze --name baseline_p2` 冻结基线，按 §4 汇总证据出《Phase 2 验收报告》。

② SQL（版本号按合入顺序领取；本迁移第 2 周首期周报前合入）

```sql
-- migrations/000N_feedback_attribution.up.sql
CREATE TABLE advice_feedback (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL,
  advice_id bigint unsigned NOT NULL, run_id bigint unsigned NOT NULL, user_id bigint unsigned NOT NULL,
  verdict    varchar(8)   NOT NULL,  -- ADOPT / DISLIKE
  category   varchar(16)  NULL,      -- DISLIKE 必填，与驳回五类一致
  comment    varchar(512) NULL,
  request_id varchar(64)  NOT NULL,
  created_at datetime(3)  NOT NULL,
  PRIMARY KEY (id), UNIQUE KEY uk_request (request_id),
  KEY idx_scope_time (tenant_id, farm_id, created_at), KEY idx_advice (tenant_id, farm_id, advice_id),
  KEY idx_user (tenant_id, farm_id, user_id, created_at)  -- P4 按 user_id 删除个人数据
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 平台表例外（母文档 §4.6.1 表约定 3）：tenant_id=0, farm_id=0 表示平台级问题；跨周同一问题只一行
CREATE TABLE attribution_issue (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL,
  signature char(32) NOT NULL, first_week char(8) NOT NULL, last_week char(8) NOT NULL,
  reason varchar(16) NOT NULL, question_type varchar(32) NOT NULL, primary_kb_id varchar(96) NULL,
  hit_count int NOT NULL,           -- 最近一周命中次数
  evidence_refs json NOT NULL,      -- 最近 50 条 [{run_id, source, source_id, week}]
  disposition varchar(16) NULL,     -- ADD_KB / FIX_LOADER / OFFLOAD_RULE / FIX_PROMPT / WONTFIX
  owner_id bigint unsigned NULL, status varchar(16) NOT NULL,  -- OPEN / IN_PROGRESS / RESOLVED / VERIFIED
  reopen_count int NOT NULL DEFAULT 0,
  resolution_ref varchar(256) NULL, row_ver int NOT NULL DEFAULT 0,
  created_at datetime(3) NOT NULL, updated_at datetime(3) NOT NULL,
  PRIMARY KEY (id), UNIQUE KEY uk_signature (tenant_id, farm_id, signature),
  KEY idx_status (tenant_id, farm_id, status, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

ALTER TABLE rejection_reason ADD COLUMN kb_ids json NULL, ALGORITHM=INSTANT;
ALTER TABLE agent_run ADD COLUMN backflow_status varchar(16) NULL,  -- SELECTED / DRAFTED / PII_FAILED / MERGED / DROPPED
  ADD COLUMN backflow_draft_ref varchar(64) NULL,                    -- 草稿合成码，映射只留业务库
  ADD KEY idx_backflow (tenant_id, farm_id, backflow_status, updated_at);

-- 复发重开：MySQL 按书写顺序求值 SET，reopen_count 必须写在 status 之前
INSERT INTO attribution_issue (tenant_id, farm_id, signature, first_week, last_week, ..., hit_count, evidence_refs, status, ...)
VALUES (0, 0, ?, ?, ?, ..., ?, ?, 'OPEN', ...) ON DUPLICATE KEY UPDATE
  last_week = VALUES(last_week), hit_count = VALUES(hit_count), evidence_refs = VALUES(evidence_refs),
  reopen_count = reopen_count + IF(status IN ('RESOLVED', 'VERIFIED'), 1, 0),
  status = IF(status IN ('RESOLVED', 'VERIFIED'), 'OPEN', status),
  row_ver = row_ver + 1, updated_at = VALUES(updated_at);
```

③ Redis 键：`lock:p2job:{job}`（见 `L3`，用于 `attribution-weekly`、`backflow-collect`）。

## 7. 推荐实施节奏

### 7.1 周排期

| 周 | 主线 | 并行 | 本周出口 |
|----|------|------|----------|
| 第 1 周 | `L0` 冻结 P1 产出的 `baseline_p1`、因子登记表、A/A 噪声测量 | `L1` 分层矩阵与补写清单，专家开始写样本；`L2` 评分标准初稿；`L8` 迁移合入 | `baseline_p1` 冻结、`thresholds.yaml` 首版 |
| 第 2 周 | `L7-a` 红线门禁进 CI（eval 库 `experiment_gate` 迁移先合入） | `L2` judge 实现；`L8` 首期归因周报 | PR 冒烟可阻断 |
| 第 3 周 | `L2` 校准集标注（先用已有样本的 trial 按分数分层，不足的类别等 `L1` 新样本补齐） | `L1` 样本持续入库；`L3` 口径表 | 口径表评审通过 |
| 第 4 周 | `L3` 日报上线 | `L4` 职责盘点；`L5` 加载埋点；`L6` 会话长度分布与阈值 | 六指标日报首发 |
| 第 5 周 | `L4 / L5 / L6` 开发 | `L1` 样本达 500+、划定 `holdout`；`L2` 校准报告；重建 `baseline_p1.r2` 与 `baseline_p0.r2` | 评测集达标 |
| 第 6 周 | `L4 / L5 / L6` 逐项单因子对比 | `L7-b` 质量门禁与配对报告 | 各 candidate 首份对比报告 |
| 第 7 周 | `L7-b` 发版门禁、成本延迟门禁、模拟用户 | 达标 candidate 按农场灰度 | 发版门禁可阻断 |
| 第 8 周 | `L8` Gate 2 收口、`compare --mode phase` 阶段对比 | 冻结 `baseline_p2`、《Phase 3 启动包》 | Gate 2 判定 |

关键路径是专家标注：`L1` 的 400 条新增样本与 `L2` 每维度 ≥ 100 个 trial 的校准标注同时占用专家档期，需在 Gate 1 前锁定（母文档 §8.2 本阶段专家 2 人重投入）。

### 7.2 并行与合入规则

1. `L0` 未完成前，`L4 / L5 / L6` 不得合入任何改变模型输入的代码。
2. 一个 PR 只含一个因子变更；PR 标题带 `Ln` 编号与因子名，如 `[P2-L5] loader_strategy v1: 植保类必加 safety`。
3. `L4 / L5 / L6` 的每个 candidate 先在开关关闭状态合入（行为不变、回归全绿），再单独提「开关打开」的对比报告，Owner 批准后灰度。
4. 同一时间线上只灰度一个因子；前一个因子观察窗结束、指标无退化后再放下一个，避免线上指标变化无法归因。
5. 评测集扩容中的样本只进 `capability`；每次实验在 `snapshot` 记录 `samples_hash`，`compare` 只在两边共有的 `sample_id@version` 上配对，样本修订或毕业不影响已有对比。
6. judge 提示词、评分标准变更走与打分器相同的评审：评测负责人 + 一名农艺专家签字。
7. 红线相关代码（`output_validator`、`calc` 下沉项的拒收规则、`compaction` 白名单）双人评审，沿用母文档 §8.3-4；拒收规则合入即常开（母文档 §4.6.2-10）。
8. 迁移版本号按合入顺序领取（母文档 §4.6.1 表约定 8），文中 `000N_*` 不预设编号。

## 8. 降级与回滚

每个优化策略必须同时给出五项，缺一项不进入排期（母文档 §1-5）：

| 策略 | baseline | candidate | 指标 | 评测集 | 开关 / 回滚路径 |
|------|----------|-----------|------|--------|-----------------|
| 规则下沉（每项一行） | `baseline_p1.r2`（只拒收、不注入） | `rule_offload.<项名>` 取 `{"mode":"on"}` | 六可信度指标、驳回原因分布、P95、token 成本 | `regression` + 对应陷阱组 + `holdout`（发版时） | 关开关即回到只拒收；拒收规则常开不回退 |
| 加载策略 | `v0`（`enable_loader_v1` 关） | `enable_loader_v1` 开（`v1`） | 加载命中率、加载精度、无关知识注入率 + 六可信度指标 | `regression` 全量，按问题类型切片 | 关开关回 `v0`，不发版 |
| 长对话压缩 | `enable_compaction` 关 | `enable_compaction` 开 + 阈值 | 白名单保留率、结论一致率、单会话 token 成本、压缩触发率 | 多轮长会话样本 + `regression` | 关开关；已压缩会话按 MinIO 原文恢复 |
| 提示词版本 | 当前 `prompt_version` | 新版本 | 六可信度指标、`explanation_quality`、token 成本 | 全部套件含 `holdout` | 配置回滚到上一版本（母文档 L4 已具备） |
| 模型档位 | 当前 `model_tier` | D1 批准范围内的其他档位 | 六可信度指标、P95、token 成本 | 全部套件含 `holdout` | 配置回滚 |
| LLM-judge 进门禁 | 只出报告 | 进门禁 | 加权 Kappa（区间下限）、专家间 Kappa、月度复核一致性 | 分层校准集 | 一致性下降即从门禁配置移除，退回只出报告 |

**降级规则**

1. 线上灰度期间，任一可信度指标线上值出现显著退化，或触发「建议无引用」告警，立即关闭对应开关，退回 baseline 配置，再做归因。
2. `compaction` 出现白名单字段缺失（代码断言失败），当次会话放弃压缩、使用完整上下文，并落 `COMPACTION_FAILED` 事件；不允许带缺失摘要继续生成建议。
3. judge 服务不可用时，门禁只按代码打分器判定，judge 维度标记「未评」，不得视为通过；门禁结论上限为 `NEED_SIGNOFF`，不得为 `PASS`。

## 9. 角色分工

1. **项目 Owner**：Gate 2 判定；多因子对比、成本延迟劣化的专项放行签字；`holdout` 访问授权。
2. **评测负责人（Harness RD 兼）**：`L0 / L2 / L7`；judge 校准；每次发版前阅读全部红线失败、非 `holdout` 翻转样本与随机 10 条通过样本的 transcript 并签字。
3. **`holdout` 负责人（QA 负责人兼）**：维护私有 `holdout` 仓与读权限；发版时阅读 `holdout` 翻转样本，对外只给 ID；不参与提示词、加载策略、规则下沉的调优。
3. **Agent Harness RD**：`L6 compaction`、`L3` 线上埋点、`L4` 中 compose Graph 与 `output_validator` 的改动。
4. **知识工程 RD**：`L4` 规则实现（`calc` / `guardrail`）、`L5` 加载策略、`L8` 知识条目补齐的工程侧。
5. **农艺专家组（2 人重投入，关键路径）**：`L1` 样本撰写与署名、`L2` 校准集标注与月度复核、`L8` 回流样本 `expect` 补写、知识条目补齐与修正、陷阱样本设计。
6. **QA**：`L1` 样本工程化与覆盖报表、`L3` 口径对账、`L7` 门禁用例与报告解读、`L8` 回流草稿流转。
7. **SRE**：CI 资源与 QPM 配额、实验 token 预算、线上灰度与开关回滚、成本与 P95 看板核对。

## 10. 阶段验收模板（执行后填写）

1. 路线完成情况：`L0`～`L8` 逐项勾选，未完成项写原因与负责人。
2. Gate 2 八条逐条结论与证据链接（报告、PR、截图）。
3. 指标对照表：`baseline_p0.r2` / `baseline_p1.r2` / 当前，六可信度指标 + 三项加载指标 + 压缩指标 + P95 + 单会话 token 成本，均带置信区间。
4. 因子贡献表：每个已上线因子的单因子配对结果（改进 / 无显著差异 / 退化）与翻转样本数。
5. judge 校准结果：三维度 Kappa、是否进门禁、月度复核记录。
6. 门禁运行记录：阻断次数、真实回退拦截记录、被放行的专项说明。
7. 灰度与回滚演练结果：每个开关一条。
8. 归因与回流：周报期数、Top 问题处置率、知识条目补齐数、回流入库数。
9. 遗留问题、负责人、关闭时间；红线相关问题不允许延期。
10. 是否进入 Phase 3（是 / 否 + 理由）。

## 11. 本阶段风险与应对

| 风险 | 表现 | 应对 |
|------|------|------|
| 专家标注产能不足 | `L1` 扩容与 `L2` 校准同时卡住，Gate 2 第 1、8 条无法达成 | Gate 1 前锁定 2 人档期；校准集先用已有样本的 trial 分层抽取，不足类别再补；双标只抽 25%；样本模板做到填空即可；QA 承担格式工程化 |
| judge 校准不达标 | judge 长期只能出报告，事实一致率无法规模化评估，Gate 2 第 3、8 条无法通过 | 回修评分标准与正反例；拆细维度定义；换不同家族的 judge 模型重校；不设「以专家抽检替代」的兜底，未达标即 Gate 2 不通过 |
| 对评测集过拟合 | `regression` 分数上升而线上指标与 `holdout` 无改善 | `holdout` 只取从未公开的新样本，由不参与调优的 `holdout` 负责人维护；发版门禁必跑 `holdout`；线上回流持续补样本 |
| 多因子同时变化 | 指标变了说不清是谁的功劳或责任 | `L0` 因子差异检查；一个 PR 一个因子；线上同一时间只灰度一个因子 |
| 过度保守率上升 | 为压幻觉率把拒答做得过宽，用户觉得「什么都不答」 | 过度保守率与幻觉率同屏展示、同时进门禁；改善只能靠补知识与规则下沉，不放松红线 |
| 压缩丢失关键事实 | 长会话后建议基于错误的地块或生育期 | 白名单字段由代码提取、不经模型；代码断言失败即放弃压缩；`enable_compaction` 默认关 |
| 评测成本上涨 | 样本扩到 500+、n=5 夜间全量后 token 费用线性增长 | 实验 token 预算；PR 只跑冒烟子集；成本进母文档 §8.4 预算核算 |
| 归因数据质量差 | 驳回原因大量填「其他」，周报无法定位问题 | 「其他」必填文字说明；QA 每周抽查并回分类 |

## 12. 下一阶段交接（交给 Phase 3）

1. **`baseline_p2`**：Gate 2 时的指标快照（六可信度指标、三项加载指标、压缩指标、P95、单会话 token 成本）及配置快照，入库不可修改；Phase 3 L0 以此为不可回退基线。
2. **评测底座**：500+ 条分层评测集、`holdout`、已校准的 judge 及其版本、配对对比与门禁流水线；Phase 3 在此基础上扩到 1000+ 条并新增图像子集、主动预警子集与 RAG 召回打分器。Phase 3 §2.1-3 的「三个 LLM-judge 维度校准达标并进门禁」即本阶段 Gate 2 第 8 条，属硬条件，Gate 2 通过即满足。
3. **加载指标口径**：加载命中率、加载精度、无关知识注入率是 Phase 3 L1「RAG 不劣于固定文件路径」的直接对照口径；`configs/loader/` 的最终版本是 `enable_rag=false` 时的回落路径。
4. **规则层清单**：已下沉的确定性判断清单与对应 `output_validator` 规则，Phase 3 L5 的数值二次校验与方案冲突检测在此基础上扩展；P3 L5 `conflict_checker` 复用 `tank_mix` 规则实现，`conflict_blocked` 打分器沿用 P2 版本扩展。
5. **归因与回流机制**：周报与回流流程常态运行，Phase 4 转为季度复盘输入。
6. **遗留问题清单**：未下沉项、未关闭风险，逐项带负责人进入《Phase 3 启动包》。

## 13. 维护规则

1. 本文随执行更新：每条路线完成后回填交付物链接与实际完成周；排期变化在 §7.1 中标注原计划与实际。
2. 指标口径、阈值、门禁规则的变更只改 §4、§6.4、§6.8 与口径表，并同步母文档 §3.3、§7.2 与评测设计方案 §11.3；三处不一致时以母文档为准。
3. 新增候选因子必须先补进 §8 表格五项，再进排期。
4. 新增开关按 `enable_*` / `rule_offload.*` 命名，登记到开关清单，默认关闭。
5. 红线不参与权衡：任何以「降低过度保守率」或「降成本」为由放松引用校验、数值校验、审批闸门的提案一律否决（母文档 §11-6）。

