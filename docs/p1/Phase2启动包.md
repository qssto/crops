# 《Phase 2 启动包》

依据：P1 实施路线 §6.9 功能任务 8（任务分解、owner、Gate 2 目标线确定方式）、§12 下一阶段交接；P2 实施路线 §2、§4 Gate 2、§5 路线总览、§6 各路线交付物、§7.1 周排期、§9 角色分工。

> **模型说明（必读）**：Phase 1 交给本阶段的全部评测（含 `baseline_p1_exit` = exp 102）均在桩模型（`model: stub`）下运行，ONEAPI_* 未提供，**真实模型未验证**。
> stub 下 token 合计为 0，n>1 不提供稳定性信息。P2 的 judge 校准、可信度指标、成本与 P95 门禁都需要真实模型，stub 数值不得用作 Gate 2 目标线。

填写规则：
- 负责人只写角色名（P2 §9 / P1 §9），不写人名。
- 文中不预设任何 Gate 2 数值；P2 文档已写死的数值照抄并注明出处，其余写「依实测确定」。
- 「待核实」表示仓库内找不到足以确认的记录；「待 Owner 定」表示文档未约定、需 Owner 决定。
- 签字栏留空。

## 1. 启动前提与输入状态

### 1.1 P2 §2.2 假定的 Phase 1 产出与现状

| # | P2 §2.2 假定 | 现状 | 证据 | 对 P2 的影响 |
|---|--------------|------|------|--------------|
| 1 | redteam 套件与复检、EXPIRED、PARTIALLY_DONE、跨农场隔离打分器，红线 pass^5 = 1 | 已交付；exp 102 redteam 39/39、regression 131/131 pass^5 = 1.00（stub）；partially_done 两个套件均无适用样本 | `eval/suites/redteam/`、`internal/evalkit/grader/code/`、`eval/out/102/report.md` | L7-a 可直接接入；partially_done 需补可运行样本 |
| 2 | 真实下发链路与 5 个示范农场的线上 run_id、审计日志、MinIO 原文 | **无**：未灰度、无真实下发，只有本地模拟平台 `cmd/devicesim` | `docs/p1/灰度记录.md`（空模板） | L6 阈值（取线上上下文长度分布）、L8 归因与回流、Gate 2 #6 #8 缺数据来源 |
| 3 | 审批意见与驳回原因分类入库 | 功能已交付（`GET /rejections`，五类原因必填），无线上数据；导出无审计、无行数上限 | `internal/app/approval_l5.go`、dev-log「P1-L5 遗留」 | L8 归因第一期无驳回数据 |
| 4 | 告警体系、开关与按农场灰度能力 | 代码已交付；无 Prometheus / Alertmanager 服务端，阈值「待实测」；灰度能力未实际使用 | `deploy/alerts/p1.yaml`、`docs/alerts/P1-alert-list.md` | 线上灰度候选因子前需先补告警服务端 |

### 1.2 启动前须由 Owner 决定的事项

| # | 事项 | 原因 | 决定人 |
|---|------|------|--------|
| 1 | 真实模型凭据（ONEAPI_*）何时提供 | P2 L2 judge 走百炼；Gate 2 #3 #5 #8 需真实模型 | 项目 Owner |
| 2 | `baseline_p1` 冻结哪一次实验 | P2 L0 任务 2 规定「baseline_p1 就是 baseline_p1_exit，不重新生成」，而 exp 102 是 stub。可选：(a) 按文档冻结 exp 102，真实模型结果只从第 5 周 `.r2` 开始；(b) 在 `c261f71` 上用真实模型重登 `baseline_p1_exit` 再冻结。两者影响第 1-5 周 PR 冒烟与夜间对比的意义 | 项目 Owner（评测负责人提出方案） |
| 3 | 无线上数据时 L6 / L8 的输入替代 | P2 L6 任务 1、L8 依赖线上 `audit_log` 与 trace | 项目 Owner |
| 4 | Gate 1 未完全满足条目如何处理 | 见《Phase 1 验收报告》§8；Gate 1 #1 #4 #5 #7 #9 未满足 | 项目 Owner |
| 5 | 专家档期锁定 | P2 §7.1：关键路径是专家标注，需在 Gate 1 前锁定（专家 2 人重投入） | 项目 Owner |
## 2. 任务分解（P2 L0-L8）

周次取 P2 §7.1；负责人取 P2 §9。建议顺序：L0 → L1 + L2 → L3 → L4 + L5 + L6 → L7 → L8；L7-a 第 2 周随 L0 接入。

| 路线 | 内容 | 周次 | 负责人（主 / 配合） | 交付物（P2 §6） | 依赖的 P1 产出与现状 |
|------|------|------|---------------------|-----------------|----------------------|
| L0 | 基线冻结与实验配置快照：Experiment 补齐评测设计方案 §12.1 字段；冻结 baseline_p1；第 5 周重建 `.r2`；`compare` 单因子 / `--mode phase`；样本清单哈希；A/A 噪声基线；候选因子登记表 | 第 1 周（`.r2` 第 5 周） | 评测负责人（Harness RD 兼）/ 项目 Owner（A/A 阈值签字） | baseline_p1 快照补录与冻结；baseline_p1.r2、baseline_p0.r2；因子差异检查与阶段级对比；A/A 阈值表；候选因子登记表 | exp 102 已登记（`eval/baselines/baseline_p1_exit.yaml`）。现有 snapshot 只有 git_sha、git_dirty、prompt_version、model_tier、model、knowledge_ref、samples_hash，缺 loader_strategy、retrieval_strategy、judge_versions、token_used、cost、wall_time；`cmd/eval` 现无 compare / baseline freeze / aa 子命令；`eval/gates/` 不存在 |
| L1 | 评测集扩容到 500+ 并分层（作物 × 生育期 × 问题类型 × 难度），陷阱样本组 | 第 1-5 周 | 农艺专家组（撰写与署名）/ QA（工程化与覆盖报表）/ holdout 负责人（划定 holdout） | 500+ 样本与参考解；分层覆盖报表；陷阱样本组 | 现有 regression 137、redteam 39、constraint 40 个 yaml；P1 新增样本多为 claude-draft / pending_expert |
| L2 | 双轨标注与三个 LLM-judge 校准；holdout 套件与访问控制 | 第 1 周初稿，第 2 周实现，第 3 周标注，第 5 周校准报告 | 评测负责人 / 农艺专家组（校准集标注）/ holdout 负责人 | 三个 judge 及评分标准；校准集与校准报告；人工队列；holdout 套件与访问控制 | 需真实模型；无现成 judge |
| L3 | 可信度六指标（引用覆盖率、事实一致率、数值准确率、幻觉率、拒答正确率、过度保守率）口径与日报，可下钻到 run_id | 第 3 周口径表，第 4 周日报 | Agent Harness RD（线上埋点）/ QA（口径对账） | 口径表；reduce 指标实现；日报；下钻链接 | P1 已有引用覆盖率、拒答率（stub）；`docs/metrics.md` 为 P1 运营指标，非可信度口径 |
| L4 | 规则与模型职责再切分（规则下沉、`rule_offload.*` 开关、conflict_blocked 打分器） | 第 4 周盘点，第 5-6 周开发与对比 | 知识工程 RD（calc / guardrail 规则）/ Agent Harness RD（compose Graph、output_validator） | 待下沉清单；逐项规则与单测；指标变化记录；conflict_blocked 打分器与红线样本 | P1 遗留 bareDigitRe 中文数字（`internal/agent/guardrail/validator.go:214`）可作为候选项 |
| L5 | 知识加载策略优化（加载埋点、`configs/loader/v1.yaml`、`enable_loader_v1`） | 第 4 周埋点，第 5-6 周 | 知识工程 RD | 加载埋点与三项指标；按问题类型的 loader v1；离线与线上对比报告 | 线上对比报告依赖线上数据，当前无 |
| L6 | 长对话压缩 compaction（白名单字段、`enable_compaction`） | 第 4 周会话长度分布与阈值，第 5-6 周 | Agent Harness RD | compaction 实现与单测；阈值评审记录；专项评测报告 | 阈值取 Phase 0-1 线上 audit_log 与 trace 的上下文长度分布，当前无线上数据（见 §1.2 第 3 条） |
| L7-a | 红线门禁进 CI（PR 冒烟可阻断） | 第 2 周 | 评测负责人 / SRE（CI 资源与 QPM 配额） | CI 触发与红线门禁判定 | 只依赖 P1 已有红线打分器；`make eval-nightly` 已有 |
| L7-b | 质量退化门禁、成本与延迟门禁、LLM 模拟用户 | 第 6-7 周 | 评测负责人 / QA（门禁用例与报告解读）/ SRE（成本与 P95 看板） | 门禁判定逻辑；配对报告；模拟用户与首批多轮样本 | 依赖 L3 口径、L2 校准、A/A 阈值 |
| L8 | 归因 Top 问题清单（周报自第 2 周起）、线上回流、Gate 2 收口 | 第 1 周迁移，第 2 周首期周报，第 8 周收口 | 知识工程 RD（知识条目补齐工程侧）/ 农艺专家组（回流 expect、条目修正）/ QA（回流草稿流转）/ 项目 Owner（Gate 2 判定） | 归因周报 ≥ 4 期；回流工具与入库样本；《Phase 2 验收报告》；baseline_p2；《Phase 3 启动包》 | 驳回原因与线上 run_id 均无线上数据 |

P2 §7.2 合入规则摘要：L0 未完成前 L4 / L5 / L6 不得合入改变模型输入的代码；一个 PR 只含一个因子；candidate 先在开关关闭状态合入；同一时间线上只灰度一个因子。

## 3. Gate 2 目标线确定方式

P2 §4 Gate 2 由项目 Owner 依证据判定，红线相关项不允许带条件通过。下表「数值」列只写 P2 文档已固定的值，其余不预设。

| # | 条目（P2 §4，摘要） | 数值 / 目标线 | 由谁确定 | 依据的 baseline | 何时确定 |
|---|---------------------|---------------|----------|-----------------|----------|
| 1 | 评测集 ≥ 500 条、分层无缺口、应拒答 ≥ 20%、专家签署齐全 | 文档已固定：≥ 500、≥ 20% | 农艺负责人签字（判定）；QA 出覆盖报表 | 不依赖 baseline | 第 5 周样本达标时核对 |
| 2 | 六指标进日报；引用覆盖率与数值准确率保持 100% | 文档已固定：两项 100%；其余四项无目标线（只进日报） | QA 核对口径 | 不依赖 baseline | 第 4 周日报首发 |
| 3 | 事实一致率与幻觉率相比 baseline_p0.r2 显著且可复现改善 | 统计判据已固定：`compare --mode phase` 配对差值置信区间不跨 0，同时段交替各跑 2 次结论一致；「目标线由 Gate 1 后依 baseline 实测确定」，数值未定 | 评测负责人出报告并签字；目标线由谁定文档未写明，**待 Owner 定**（建议评测负责人提出、项目 Owner 批准） | **baseline_p0.r2**（不是 baseline_p1 / baseline_p1.r2）；在 L1 冻结的 500+ 样本清单、同一 judge_versions 上配对 | baseline_p0.r2 第 5 周重建后才有实测值；目标线须在 Gate 1 判定之后、第 8 周阶段对比之前确定；依赖第 8 条 fact_consistency 已校准 |
| 4 | `make eval` 回归门禁生效，且至少拦下 1 次真实回退 | 质量退化阈值取 A/A 实测（同一 snapshot 在 regression 全量跑 3 次 n=5，取两两差值 95% 分位为下限）；引用覆盖率或数值准确率 < 100% 无条件阻断 | 评测负责人生成 `eval/gates/thresholds.yaml` 草稿（`cmd/eval aa --runs 3`）；项目 Owner 以 PR 签字合入 | A/A 用同一 snapshot；PR 冒烟与夜间第 1-5 周对比 **baseline_p1**，第 5 周后单因子对比 **baseline_p1.r2** | 第 1 周首版；snapshot 变化达到重建条件时重测 |
| 5 | 成本与延迟门禁生效，单会话成本与 P95 均不劣于 baseline | 「劣化超阈值」的阈值文档未给数值，**待 Owner 定**；劣化超阈值需专项说明、Owner 签字放行 | 评测负责人（门禁）；SRE 核对线上看板；项目 Owner 放行签字 | 门禁对照入库 baseline：第 1-5 周 baseline_p1，之后 baseline_p1.r2（P2 §8 各策略 baseline 列）；Gate 2 #5 未写明是哪一份，**待核实** | 第 6-7 周 L7-b 接入时；须真实模型（stub 下 token 为 0、P95 不具代表性） |
| 6 | 归因 Top 问题清单连续 4 周，≥ 10 条知识条目补齐或修正 | 文档已固定：4 周、≥ 10 条 | 农艺负责人签字 | 不依赖 baseline | 第 2 周起持续，第 8 周核对 |
| 7 | compaction：白名单字段 100% 保留，结论不漂移，长会话成本下降有实测 | 文档已固定：保留率 100%；成本下降幅度不设数值，只要求实测数据；压缩触发阈值依线上上下文长度分布评审 | Agent Harness RD 提阈值，评审记录入库 | `enable_compaction` 关闭状态（P2 §8） | 第 4 周阈值评审；当前无线上分布数据 |
| 8 | 三个 judge 维度加权 Kappa 达阈值并进门禁；holdout 建立；回流每周有产出 | Kappa 阈值「本阶段启动时按实测确定」，评测设计方案初步参考 0.7，以 bootstrap 95% 区间下限对照 | 评测负责人（校准报告签字） | 分层校准集（每维度 ≥ 100 个 trial，专家标注），不依赖 baseline | 阶段启动时定阈值；第 5 周校准报告；未达标即 Gate 2 不通过 |

baseline 关系：
- `baseline_p1_exit` = exp 102（main `c261f71`，stub，regression,redteam n=5，170 样本 / 850 trial）。
- `baseline_p1`：P2 L0 第 1 周为 exp 102 补录快照并置 `frozen=1`，不重跑（见 §1.2 第 2 条待 Owner 定）。
- `baseline_p1.r2`、`baseline_p0.r2`：第 5 周在 L2 校准完成、L1 样本清单与 holdout 划定之后，用当前 judge 在冻结的 500+ 样本清单上重跑 n=5。`.r2` 用于 L4 / L5 / L6 单因子对比；`baseline_p0.r2` 用于 Gate 2 #3。judge_versions 不同的实验不允许直接比较。
- `baseline_p0` = exp 28 保留不动。
## 4. P1 §12 交接七项

| # | 交接项（P1 §12） | 状态 | 证据位置 | 缺口 | 接收方（P2 角色） |
|---|------------------|------|----------|------|-------------------|
| 1 | 驳回原因分类数据（rejection_reason，按周导出，关联 run_id 与 advice_id） | 功能已交付，无线上数据 | `GET /rejections`（`internal/app/approval_l5.go`）；原因码 DOSAGE / TIMING / PRODUCT / EVIDENCE / OTHER（`internal/model/approval_l5.go`）；L5 提交 `f910dd3` | 无真实审批；导出无审计、无行数上限 | 知识工程 RD、农艺专家组（L8 归因） |
| 2 | 线上 run_id 回放数据（audit_log + MinIO raw_object） | 存储与读取能力已交付，**无线上数据** | raw_store（L2 `f02434e`）；审计哈希链、导出、保留期（L7 `75c524d`） | 未灰度、无示范农场 run_id | QA（L8 回流草稿）、Agent Harness RD（L6 阈值） |
| 3 | redteam 套件与新增打分器（复检、EXPIRED、PARTIALLY_DONE、跨农场隔离） | 已交付；exp 102 redteam pass^5 = 1.00（stub） | `eval/suites/redteam/`（39 条）；`internal/evalkit/grader/code/` recheck.go、expired.go、partiallydone.go、crossfarm.go、injection.go、forbidden.go；L8 提交 `c261f71` | 样本 claude-draft / pending_expert 待专家签字；partially_done 无适用样本（rice-rcpt-0004 隔离） | 评测负责人（L7-a 并入门禁） |
| 4 | 告警基线与《告警清单》、按农场看板、审计校验链与导出 | 代码已交付；阈值草稿「待实测」；告警触发与看板对账 **待演练** | `deploy/alerts/p1.yaml`；`docs/alerts/P1-alert-list.md`；`GET /api/v1/farms/{farm_id}/metrics`（`docs/metrics.md`）；`cmd/auditctl verify` / `anchor-check` | 无 Prometheus / Alertmanager 服务端（prometheus 客户端未获批，用标准库注册表）；OTel 未加 | SRE |
| 5 | baseline_p1_exit → baseline_p1 → baseline_p1.r2 | baseline_p1_exit 已登记：exp 102；后两步属 P2 L0 | `eval/baselines/baseline_p1_exit.yaml`；`eval/out/102/report.md`；yaml 记录的对象键 `report/102/report.md`、`report/102/snapshot.json`（MinIO 中是否存在未核对） | stub 模型；snapshot `git_dirty: true`（工作区仅用户未提交的 `.gitignore` 改动，跟踪代码与 `c261f71` 一致）；快照缺评测设计方案 §12.1 字段 | 评测负责人 |
| 6 | 灰度与回滚记录、降级矩阵演练记录 | **待演练**，均为空模板 | `docs/p1/灰度记录.md`；`docs/p1/一键关停演练记录.md`；`deploy/runbooks/kill-switch.md`；`docs/drill/P1-L6-fault-drill.md` | 三档灰度、预发一键关停、四类故障演练均未做；替代证据为集成测试（TestKillSwitchDrill_Integration 等），主仓本地容器通过 | SRE、QA |
| 7 | 《Phase 1 验收报告》与《Phase 2 启动包》 | 已起草，签字栏空 | `docs/p1/Phase1验收报告.md`；本文件 | Owner 判定与各方签字 | 项目 Owner |

## 5. 带入 Phase 2 的遗留

完整清单见《Phase 1 验收报告》§7。下表只列影响 P2 排期或判定的项。

| # | 遗留 | 影响的 P2 路线 / Gate 2 条目 | 负责人 |
|---|------|------------------------------|--------|
| 1 | 真实模型未验证 | L0 baseline 意义、L2、L3、L7-b；Gate 2 #3 #5 #8 | 项目 Owner、评测负责人 |
| 2 | 无线上 run_id、驳回数据 | L5 线上对比、L6 阈值、L8；Gate 2 #6 #7 #8 | 项目 Owner |
| 3 | redteam 与 P1 新增样本待专家签字 | L1（专家档期与新增 400 条样本竞争）、L7-a | 农艺专家组、评测负责人 |
| 4 | REAL 评测设施未定，rice-rcpt-0001..0006 隔离，partially_done 无适用样本 | L7-a 红线覆盖 | 评测负责人 |
| 5 | bareDigitRe 不认中文数字（红线二相关） | L4 候选下沉项；P2 §2.3 第 2 条要求数值准确率保持 100% | 知识工程 RD、Agent Harness RD |
| 6 | unexpected_call 640 行（exp 102，只读工具，无 device.*） | L1 样本 fixture 补齐；L3 口径 | QA |
| 7 | `constraint` 套件不在夜间全量（exp 103 单独跑 n=5 pass^5 = 1.00） | L7-a 是否把 constraint 纳入夜间全量（现 `make eval-nightly` 只含 regression,redteam），**待 Owner 定** | 评测负责人 |
| 8 | 无人机撒施颗粒肥是否建人工单 | 产品决策，P1 dev-log 记为 P2 | 项目 Owner |
| 9 | 告警服务端缺失、阈值待实测 | 线上灰度候选因子（P2 §7.2 第 4 条、§8 降级规则第 1 条） | SRE |
| 10 | 业务库迁移 0005-0011 未执行、`AS_DEVICE_RECEIPT_SECRET` 未提供 | P2 新增迁移（L0 experiment 列、L7 experiment_gate、L8）的合入顺序 | SRE |

## 6. 签字

| 角色 | 签字 | 日期 |
|------|------|------|
| 项目 Owner（启动批准、§1.2 决定事项） | | |
| 评测负责人（Harness RD 兼） | | |
| holdout 负责人（QA 负责人兼） | | |
| 农艺专家组 | | |
| SRE | | |
| 法务合规（D5、法规对照表） | | |
| 田间验证方（线上数据来源） | | |
