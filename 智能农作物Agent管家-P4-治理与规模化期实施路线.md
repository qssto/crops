# 智能农作物 Agent 管家 Phase 4（P4）治理与规模化期 实施路线（持续，首个 12 周建设期 + 季度常态）

> 文档类型：阶段详细实施路线（Phase 4 展开）
> 上游文档：《分阶段实施路线图》§5.4、§6、§7.3-7、§8；《评测 Harness 设计方案》§11.2、§13、§15 Phase 4 行；《P3 高级能力期实施路线》（Gate 3 结论与《Phase 4 启动包》）
> 下游：常态运营（季度复盘、on-call、作物扩展流水线）
> 读者：项目 Owner、常态研发、扩展小组、知识工程 RD、农艺专家、QA、SRE、法务、财务
> 状态：草案，Gate 3 通过后评审

---

## 1. 文档定位

1. 本文把母文档 §5.4.1 七项功能拆成 `L0 → L7` 八条路线，给出每条路线的涉及模块、功能任务、交付物与验收，回答「Phase 4 第一天开始做什么、做到什么程度算完」。
2. Phase 4 没有结束日期。本文分两段：**首个 12 周建设期**把治理能力建出来并过 Gate 4 的建设类条目；**季度常态**按固定节奏运行，Gate 4 中「连续 2 个季度」类条目在常态期判定。
3. 与母文档关系：本文是 §5.4 的展开，不新增阶段目标。与母文档冲突时以母文档为准，并回改本文。
4. 本阶段的核心变化是**交付主体从核心研发转为流程**：新作物由扩展小组按 SOP 接入，知识由专家在工作台变更，故障由 on-call 按 Runbook 处置。核心研发只维护底座与工具。

## 2. 阶段基线与输入

### 2.1 Phase 3 产出（进入 Phase 4 的前提）

1. Gate 3 六条全部通过，《Phase 3 验收报告》与《Phase 4 启动包》入库。
2. 五个能力开关 `enable_rag / enable_vision_diagnose / enable_proactive_alert / enable_multi_device_schedule / enable_auto_plan` 已按灰度结论处于确定状态（全量 / 部分农场 / 关闭），每个开关有回滚演练记录。
3. 指标基线：Phase 3 收口时的全指标快照记为 `baseline_p3`（可信度六项 + 检索精度、视觉置信度分布、预警有效率、计划采纳率 + 成本与 P95），入库不可修改。
4. `evalkit` 已具备 `regression / capability / redteam / holdout` 四套件、图像子集、主动预警子集、RAG 召回打分器，PR / 夜间 / 发版三级门禁生效。
5. 法规映射首版已在 Phase 1 落入 `constraint_checker` 与审批前置条件，并经法务签字。

### 2.2 沿用的契约、开关与指标（本阶段不改语义）

1. 五个核心契约 `AdviceCard / WorkOrder / Citation / RunState / ApprovalTicket` 及 `WorkOrder` 状态机，按 L1 冻结规则走版本号变更。
2. 知识条目 front-matter：`kb_id / crop / stage / region / category / authority_level / version / author / effective_from`。本阶段只**新增**生命周期字段（见 L2），不改既有字段含义。
3. 两条红线及其打分器（`citation_valid / numbers_match_calc / product_in_candidates / no_model_actuate / stop_at_approval / kb_miss_refuse / pii_clean / state_machine_legal`）不放松。
4. 开关：`enable_robot_dispatch / enable_advice`（按农场灰度）与 Phase 3 五个能力开关保持现有语义，取值形态按母文档 §4.6.2-5。唯一的语义增量是 L6 一键关停的 `cancel_inflight=true`：范围内 `DISPATCHED / EXECUTING` 工单由系统批量写 `order.cancel`，替代 P1「值班人逐单 cancel」；默认 `false` 时仍按 P1 逐单处理，Runbook 同步写明。
5. 治理数据源：`audit_log`、`raw_store`、OTel trace、§3.3 指标体系、Phase 2 起的归因 Top 问题清单。

## 3. 范围边界

### 3.1 本阶段必须完成

1. 作物包抽象与《新增作物接入 SOP》，扩展小组按 SOP 独立接入 ≥ 2 个新作物。
2. 知识生命周期全流程线上化（提报 → 评审 → 生效 → 到期复核 → 失效 → 版本回滚）与专家审核工作台；线下改文件路径关闭。
3. 知识污染防护：无署名写入被拒，知识回归集在知识变更时自动运行并阻断。
4. 成本看板与预算治理：按农场 / 作物 / 能力三个维度出数，预算告警，高成本会话归因。
5. 合规与数据安全闭环：出域策略、农户隐私、法规映射持续更新、责任划分与保险方案落地、审计保留期与取证流程。
6. 运维闭环：Runbook 覆盖全部 P0/P1 告警项，on-call 值班，季度四类故障演练。
7. 效果经营机制：季度复盘「指标 → 归因 → 知识与策略迭代」，归因 Top 清单进季度 OKR。

### 3.2 本阶段明确不做

1. 不放松红线：动设备仍必须人审，不引入全自主执行；作物包不允许携带绕过 `output_validator` 或 `constraint_checker` 的策略。
2. 不新增 Phase 3 之外的智能能力（产量预测、经营决策、模型微调、自建推理集群），如需立项另走新阶段。
3. 不做作物包热加载与第三方作物包市场：作物包随版本发布，经评测门禁后生效（母文档 §4.6「不借鉴插件热加载」）。
4. 不重写底座：harness、主链路编排、评测框架只做治理所需的增量改造。
5. 不承担田间验证与下发单量组织，相关结论由业务侧提供，本阶段只提供数据与看板。

## 4. 目标与通过标准（Gate 4）

| # | 通过标准（母文档 §5.4.2） | 判定方式 | 判定人 | 判定时点 |
|---|----------------------------|----------|--------|----------|
| 1 | 按 SOP 独立完成 ≥ 2 个新作物接入，未占用核心研发排期 | L1 作物包验收清单全过 + 排期记录中核心研发工时为 0（工具缺陷修复除外，单列） | Owner | 建设期末 / 首个常态季度 |
| 2 | 专家审核工作台承载全部知识变更，线下改文件路径被关闭 | `knowledge/` 目录写保护生效；抽查一个季度内全部知识变更均有工作台 `review_id` | 架构负责人 + 农艺负责人 | 建设期末 |
| 3 | 成本看板上线，季度成本可预测 | 看板三维度出数；季度预测与实际偏差在首季度实测后约定的范围内 | 财务 + Owner | 第 2 个常态季度末 |
| 4 | 合规清单逐项有结论与责任人，保险方案落地 | L5 合规清单每项有结论、责任人、签字；保险生效文件入库 | 法务 | 建设期末 |
| 5 | 四类故障演练全部完成并留报告；Runbook 覆盖所有 P0/P1 告警项 | 告警项 ↔ Runbook 对照表无空缺（自动校验）；四份演练报告入库 | SRE 负责人 | 建设期末 + 每季度 |
| 6 | 季度复盘机制连续运行 2 个季度 | 两份季度复盘纪要 + 对应 OKR 条目可追溯到归因 Top 清单 | Owner | 第 2 个常态季度末 |

补充约束（不单列 Gate 条目，但任一不满足则 Gate 4 不通过）：

1. 新接入作物在 `regression` 与 `redteam` 套件上红线 pass^5 = 1，`citation_valid / numbers_match_calc` 通过率 100%。
2. 已有作物在新作物接入后指标不劣于 `baseline_p3`（按样本配对比较，配对差值为负且置信区间不跨 0 即判为退化）。

## 5. 实现路线总览（L0 → L7）

1. `L0`：常态化基线与治理范围
2. `L1`：作物包与扩展工厂化
3. `L2`：知识生命周期与专家审核工作台
4. `L3`：知识污染防护与知识回归集
5. `L4`：成本看板与预算治理
6. `L5`：合规与数据安全
7. `L6`：运维闭环（Runbook / on-call / 故障演练）
8. `L7`：效果经营机制与 Gate 4 收口

建议顺序：`L0 → L2 + L3（同组串行，L3 依赖 L2 的变更事件）→ L1（依赖 L2/L3 作为作物包准入通道）`；`L4 / L5 / L6` 在 L0 后即可并行；`L7` 在建设期末启动并转入季度常态。

```
                        L0 常态化基线与治理范围
          ┌───────────────┬──────┴────────┬───────────────┐
          ▼               ▼               ▼               ▼
   L2 知识生命周期   L4 成本看板     L5 合规与数据安全   L6 运维闭环
      + 工作台        与预算治理           │          （Runbook / on-call）
          │               │               └─取证流程──►    ▲
          ▼               │                               │
   L3 知识污染防护 ───────┼──── 知识污染演练依赖 L3 ───────┘
      + 知识回归集         │
          │               │
          ▼               │
   L1 作物包与扩展工厂化   │
          └───────┬───────┘
                  ▼
     L7 效果经营机制 + Gate 4 收口 ──► 季度常态
```

## 6. 路线详细拆解

### 6.1 L0 常态化基线与治理范围

**目标**：把 Phase 3 的终态冻结为 Phase 4 的对照基线，并确定哪些开关、指标、告警进入长期治理。

**涉及模块**：`eval/baselines/`（`baseline_p3` 指针）、`enable_*` 开关配置、§3.3 指标体系、告警配置、《Phase 4 启动包》。

**功能任务**

1. 冻结 `baseline_p3`：配置快照（`git_sha / prompt_version / model_tier / loader_strategy / retrieval_strategy / knowledge_ref / judge_versions`）与全指标结果入库，只读。
2. 开关清单收敛：五个 Phase 3 开关逐个给出结论 ——「已全量，转为常驻配置并保留关停能力」或「保持灰度，指定负责人与复评日期」。不允许出现无人负责的长期灰度开关。
3. 治理指标清单：从 §3.3 与 Phase 3 新增指标中选定进入季度复盘的指标集，每项写明口径、数据源、负责人。
4. 告警清单定级：全部告警项标 P0 / P1 / P2，作为 L6 Runbook 覆盖范围的输入。
5. 承接《Phase 4 启动包》中的遗留问题，逐项指派负责人与关闭时间，红线相关遗留不允许延期（母文档 §6-3）。

**交付物**：`baseline_p3` 快照；开关治理清单；治理指标清单；告警定级表；遗留问题清单。

**验收**

1. `baseline_p3` 可被 `cmd/eval compare` 直接引用，且快照不可修改（写操作被拒）。
2. 开关治理清单中每个开关有结论与负责人；处于灰度的开关有复评日期。
3. 告警定级表覆盖当前全部告警项，无「未定级」。
4. 红线相关遗留问题全部关闭。

**技术实现**

① 流程

1. `cmd/eval baseline freeze --name baseline_p3 --exp <id>`（P2 L0 已有）：校验实验已完成、全部 trial 有打分 → 置 `experiment.frozen=1` → 结果导出 `jsonl.gz` 上传 MinIO `agri-steward-eval/baselines/baseline_p3/` → 生成 `eval/baselines/baseline_p3.yaml`（`experiment_id`、配置指纹、导出文件 `sha256`）提 PR。
2. `cmd/eval compare --baseline baseline_p3` 读 yaml 定位实验，重算导出文件摘要，与 yaml 中 `sha256` 不一致直接失败。
3. 开关注册表 `configs/flags_registry.yaml`：沿用 P3 已建的此文件并扩展字段 `key / owner / state: PERMANENT|GRAY|RETIRED / default / review_at / budget_degradable`；`default` 即母文档 §4.6.2-5「没有 FARM 行时的注册表默认值」。`key` 支持前缀通配，`crop_pack.*`（L1 作物开关，默认关）与 `budget_hold.*`（L4 预算覆盖开关）各登记一条，新接入作物不需要改注册表。`internal/service/flag` 启动时加载，未匹配任何条目的 `flag_key` 读写报错；CI `cmd/opscheck flags` 校验 GRAY 项有 `owner` 且 `review_at` 未过期。
4. 治理指标清单 `ops/governance/metrics.yaml`（`metric / 口径 / source / owner / min_sample`），L7 只计算清单内指标；告警迁入 `ops/alerts/*.yaml`，`level` 必填 `P0/P1/P2`，`cmd/opscheck alerts` 校验无未定级项。
5. 遗留问题建 issue，带 `redline` 标签的在 L0 结束前关闭，看板过滤结果即验收证据。

② SQL（作用于评测库 `agri_eval`，业务库无新表；P2 只在 store 层拒写冻结实验，P4 下沉到数据库触发器兜底）

```sql
-- trial / experiment 各建 BEFORE UPDATE、BEFORE DELETE 触发器，trial 另建 BEFORE INSERT（按 NEW.experiment_id 判断，防冻结后补插），以 trial 为例
CREATE TRIGGER trg_trial_frozen_upd BEFORE UPDATE ON trial FOR EACH ROW
BEGIN
  IF (SELECT frozen FROM experiment WHERE id = OLD.experiment_id) = 1 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'experiment frozen';
  END IF;
END;
```

③ Redis 键：无。

测试：注册表加载表驱动单测（未注册键、通配匹配、GRAY 缺字段）；`freeze` 后对 `trial` 执行 UPDATE 的集成测试，期望 `45000`。

### 6.2 L1 作物包与扩展工厂化

**目标**：新增一个作物从「专家全程陪跑」降到「填模板 + 过验收」，由扩展小组独立完成，核心研发不进排期。

**涉及模块**：`knowledge/<crop>/`、`knowledge/index.yaml`、`knowledge/common/pesticide_registry.yaml`、`internal/agent/calc/`、`internal/agent/knowledge/`、`tool_policy.yaml`、`eval/suites/*/<crop>/`、`question_taxonomy`。

**作物包定义**（母文档 §4.6「扩展包模式」）：一个作物包 = 该作物的知识条目 + `calc` 公式 + 策略。知识条目经 L2 工作台随知识发布在运行时切换，`calc` 参数与策略随应用发布；两条通道的版本一致性由 L2 发布门禁的作物包兼容校验保证，不要求「同版本发布」。`calc_params.yaml` 与 `policy.yaml` 属于红线资产（母文档 §4.6.2-10）。

| 组成 | 内容 | 位置 |
|------|------|------|
| 知识条目 | `phenology / fertilization / irrigation / pest/<pest_name> / disaster`，front-matter 完整 | `knowledge/<crop>/` + `index.yaml` 增量 |
| `calc` 公式 | 用量、稀释倍数、PHI 推算、药剂候选集合的作物级参数；只填参数表，不新写计算代码 | `croppacks/<crop>/calc_params.yaml`（`internal/agent/calc/` 只读参数） |
| 策略 | `question_taxonomy` 增量、加载策略（必加文件）、作业硬约束的作物级阈值 | 作物包 `policy` 文件 |
| 评测样本 | 达到最小规模的 `regression / redteam` 样本 + 参考解 | `eval/suites/*/<crop>/`、`eval/refs/` |

**功能任务**

1. 抽取作物包结构：把首个作物中作物相关的参数从代码中剥离到参数文件，`calc` 只保留通用计算逻辑；新作物不需要改 Go 代码。
2. 作物包清单文件 `crop_pack.yaml`：`crop / pack_version / region[] / kb_ref（接入评测所用的知识版本，仅作记录）/ calc_params_ref / policy_ref / eval_suites[] / signed_by[] / status（draft·review·active·retired）`。
3. 作物包静态校验 `cmd/eval validate --pack <crop>`：front-matter 必填项、`authority_level` 与署名齐全、药剂候选集合 ⊆ `pesticide_registry.yaml` 中该作物登记范围、`calc` 参数与被引用知识条目中的数值一致、四类问题均有知识条目、样本配比（应拒答 ≥ 20%）、参考解通过全部代码打分器。
4. 编写《新增作物接入 SOP》，包含：
   - 知识条目模板（逐文件给出必填段落与示例，做到「填空即可」）；
   - 必填清单（作物生育期表、主要病虫害谱、防治指标阈值、登记药剂、作业窗口阈值）；
   - 评测集最小规模（四类问题各自的样本数下限，首次接入实测后约定）；
   - 验收清单（即本路线验收项的可勾选版本）；
   - 专家签署流程（经 L2 工作台）；
   - 人日预算模板（首个新作物接入后按实测填写）。
5. 作物包上线流程：`draft → review（知识条目经 L2 工作台专家评审；参数与阈值文件经规则 owner + 农艺专家双签）→ 门禁（L3 知识回归集 + 新作物套件）→ active`，按区域与农场灰度，复用 `enable_advice` 的农场维度灰度能力。
6. 扩展小组按 SOP 独立接入 ≥ 2 个新作物，过程中遇到的 SOP 缺口记录为 SOP 修订项，不由核心研发代写。

**交付物**：作物包结构与 `crop_pack.yaml` 规范；`validate --pack`；《新增作物接入 SOP》v1；≥ 2 个 `active` 状态的新作物包；SOP 修订记录与人日实测。

**验收**

1. 首个作物重构为作物包后，在 `baseline_p3` 上回归无退化（配对对比无显著负向翻转，红线 pass^5 = 1）。
2. 新增作物全程无 Go 代码改动（代码 diff 只出现在参数文件、知识文件、样本目录）。
3. `validate --pack` 有表驱动单测，且对缺署名、未登记药剂、样本配比不足三类违规各有一条「应失败」用例。
4. 2 个新作物包均为 `active`，其套件红线 pass^5 = 1；排期记录中核心研发工时为 0（工具缺陷修复单列）。
5. 两次接入的实际人日已回填进 SOP 的人日预算模板。

**技术实现**

① 流程

1. 作物包目录 `croppacks/<crop>/{crop_pack.yaml, calc_params.yaml, policy.yaml}` 随应用发布；`internal/agent/knowledge/croppack` 的 `Registry` 在启动时与每次知识 bundle 切换后加载并交叉校验全部作物包（`registry_subset`、`params_match_kb`）。CI 中任一非法即失败；运行时只把非法的作物标为不可用并发 P0 告警，其他作物照常服务，进程不因单个作物包失败而退出（§8「仅该作物」）。
2. `calc` 去作物化：稀释倍数区间、亩用量上下限、PHI 天数、药剂候选全部移入 `calc_params.yaml`。`internal/agent/calc` 只依赖自己定义的 `ParamSource` 接口，由 `cmd/server` 装配时用适配器把 `Registry` 包成 `ParamSource`，避免 `calc ↔ croppack` 循环依赖。
3. 运行时：`classify` 节点识别 `crop` 后调 `Registry.Get`，把 `crop` 与 `pack_ref`（`{pack_version}@{calc_params 与 policy 文件 sha256 前 8 位}`）写入 `agent_run`，数值可追溯到参数版本；返回 `ErrPackUnavailable`（未接入、`retired` 或交叉校验失败）→ refuse（`KB_MISS`）；农场未开 `crop_pack.<crop>`（FARM 级 `feature_flag`，注册表通配 `crop_pack.*`，默认关）→ refuse（`OUT_OF_SCOPE`）。
4. `cmd/eval validate --pack <crop>`：规则列表（每条规则一个 `Rule` 实现，汇总全部违规不提前退出）：`fm_required`、`signed_by_roster`（署名人为 `kb_expert` 且 `expert_scope.yaml` 覆盖该作物；CI 读 worker 每日导出的名册 `agri-steward-kb/roster/expert_roster.json`，不连生产库）、`registry_subset`、`params_match_kb`（`calc_params.yaml` 的用量区间、PHI 与被引用知识条目中的数值一致，且落在 `pesticide_registry` 登记范围内）、`taxonomy_coverage`、`sample_mix`（应拒答 ≥ 20%）、`ref_graders`。
5. 审核链（母文档 §4.6.2-10）：`CODEOWNERS` 把 `croppacks/**` 设为规则 owner 组 + 农艺专家组双必需评审；任何触及 `croppacks/<crop>/` 的 PR 都触发 `kb-regression`（以当前已发布 `knowledge_ref` 为准）与该作物全部套件（n=3），红线 pass^3 < 1 即阻断。
6. CI `scripts/ci/pack-paths.sh`：带 `crop-pack` 标签的 PR，diff 只允许落在 `croppacks/<crop>/`、`eval/suites/*/<crop>/`、`eval/refs/<crop>/`，否则失败并提示「记为 SOP 缺口」（即验收 2 的证据）；知识条目不经此 PR，走 L2 工作台。
7. 首个作物重构单独 PR：同一 `knowledge_ref` 上跑全部套件（n=5），`calc` 输出逐样本字节级一致。

② SQL：无新表。署名人身份复用 P1 `user_role`（平台级绑定 `tenant_id=0, farm_id=0, role='kb_expert'`）；可签署的作物范围与权威等级放知识仓库 `knowledge/common/expert_scope.yaml`，同样经 L2 工作台评审变更。

```sql
ALTER TABLE agent_run ADD COLUMN crop varchar(32) NULL, ADD COLUMN pack_ref varchar(48) NULL, ALGORITHM=INSTANT;  -- 迁移编号按合入顺序领取
-- 工作台在线校验
SELECT 1 FROM user_role WHERE tenant_id = 0 AND farm_id = 0 AND user_id = ? AND role = 'kb_expert';
-- 名册导出（worker 每日，供 CI 使用）
SELECT user_id FROM user_role WHERE tenant_id = 0 AND farm_id = 0 AND role = 'kb_expert';
```

③ Redis 键：无（作物包随进程加载，不缓存到 Redis）。

测试：每条 `Rule` 表驱动单测，至少含缺署名、未登记药剂、样本配比不足、参数与知识数值不一致四条应失败用例；`calc` 黄金用例在重构前后同时跑；构造一个交叉校验失败的作物包，断言只有该作物走 `KB_MISS`，其他作物样本照常通过。

### 6.3 L2 知识生命周期与专家审核工作台

**目标**：知识条目的每一次变更都在线上留痕、有署名、可回滚，线下直接改 `knowledge/` 文件的路径关闭。

**涉及模块**：`internal/agent/knowledge/`、`knowledge/`（git 仓库作为知识的唯一存储）、专家审核工作台（前端 + 后端服务）、`audit_log`、RAG 索引（`enable_rag` 开启时）。

**知识条目生命周期**（拆成两个状态机：条目状态管「这条知识现在能不能用」，修订状态管「一次改动走到哪一步」）

```
条目 kb_entry.status：  ACTIVE ⇄ REVIEW_DUE（到期 → ；复核确认 ← ）   ACTIVE / REVIEW_DUE → RETIRED（RETIRE 修订合入）
修订 kb_revision.status：DRAFT → IN_REVIEW → APPROVED → MERGED（随下一次发布生效）
                              └→ REJECTED          DRAFT / IN_REVIEW → WITHDRAWN
```

一条 `ACTIVE` 条目正在改版时，条目仍是 `ACTIVE`，改版进度只看修订状态。版本回滚是 `change_type=ROLLBACK` 的修订，内容取目标版本，版本号继续递增。

**功能任务**

1. front-matter 新增复核字段（只增不改）：`reviewed_at / review_due_at / reviewer / superseded_by`；`effective_from` 语义不变。条目状态不写进 front-matter，只在 MySQL 台账中维护，供工作台队列使用。
2. 条目与修订两个状态机的合法迁移表，落成代码常量与单测；非法迁移报错，与 `WorkOrder` 状态机同一实现风格。
3. 专家审核工作台：
   - 提报：按 L1 模板填写条目，提交即生成 `review_id` 与 git 分支；
   - 评审：展示与当前 `ACTIVE` 版本的 diff、引用该条目的样本数与近期线上引用次数、L3 知识回归结果；评审人不得是提报人；
   - 生效：评审通过后由工作台合入主干并打知识版本号，提报人、评审人写入 `author / reviewer`；合入后随下一次发布（知识 ref 与 RAG collection 成对，见技术实现 4）对线上生效；
   - 复核：`review_due_at` 到期自动转 `REVIEW_DUE` 并通知署名专家；内容无需修改时专家点「复核确认」，只更新 `reviewed_at / review_due_at`，版本号不变，不产生样本待办；
   - 失效与回滚：失效需填原因；回滚指定目标版本，经评审后生效。
4. 超期降权：加载器按 bundle 中的 `review_due_at` 与注入的 `Clock` 计算是否超期（不读 MySQL，同一 `knowledge_ref` 在评测中可复现）；超期条目在加载排序中降权（固定文件加载与 RAG 排序均生效），超过宽限期仍未复核则退出候选（宽限期首季度实测后约定，需 Owner 与农艺负责人确认），触发 `KB_MISS` 走拒答，而不是继续使用过期知识。
5. 关闭线下路径：`knowledge/` 主干开启分支保护，仅工作台服务账号可合入；CI 校验每个知识提交带 `review_id`，无则拒绝。
6. 全部生命周期动作写 `audit_log`（`actor / action / target=kb_id@version / result`）。

**交付物**：生命周期字段与状态机；专家审核工作台 v1；分支保护与提交校验；超期降权规则。

**验收**

1. 条目与修订状态机的非法迁移有单测覆盖，一律报错。
2. 绕过工作台直接向 `knowledge/` 主干推送被拒绝（演练留记录）。
3. 抽查一个季度内全部知识变更，100% 带 `review_id`，提报人与评审人不同。
4. 构造一条 `review_due_at` 已过的条目（`FrozenClock` 推进时间）：加载排序中被降权；超期阈值后退出候选，相关样本走 `KB_MISS` → 拒答；「复核确认」后恢复正常排序且 `kb_id@version` 不变。
5. 回滚演练：将一条条目回滚到上一版本，发布后 `current.json` 中的 `knowledge_ref` 与 `collection` 同时切换，固定文件路径与 RAG 路径都不再返回被回滚的版本，相关样本引用的 `kb_id@version` 随之变化。

**技术实现**

① 流程（`controller/admin/kb` → `service/kbstudio` → `repository/kb` + GitLab API；正文以 git 为准，MySQL 只存台账，台账写失败由 `kb-reconcile` 按提交 trailer `Review-Id` 补写）

1. 提报 `POST /api/v1/admin/kb/reviews`：同一 `kb_id` 有未关闭 revision 则拒绝（`KB_REVISION_OPEN`）→ 插 `kb_revision(DRAFT, version=max+1)`，自增 `id` 即 `review_id` → 建分支 `kb/{review_id}` 并提交（trailer `Review-Id / Kb-Author`）→ 同事务写 `kb_review(SUBMIT)`、`audit_log(KB_SUBMIT)`。
2. 提交评审：`lock:kb:{kb_id}` → `CheckKBTransition` → `IN_REVIEW` → outbox `kb.ci.request` 由 worker 触发 CI；回调写 `kb_review(CI_RESULT)`，`head_commit` 不是当前分支头则只记录不生效。评审通过需满足：评审人 ≠ 提报人（`KB_SELF_REVIEW`）、为 `kb_expert`（`KB_NOT_EXPERT`）、`head_commit` = 最近一次通过的 CI（`KB_CI_STALE`）→ `kb_review(APPROVE)`。
3. 合入：`lock:kb:merge` → 追加提交写 `reviewer / reviewed_at / review_due_at` 并重建 `index.yaml` → 带 `expectedHead` 合入 → 事务内 `kb_revision(MERGED)`、`kb_entry` upsert（CREATE 首次插入，状态 `ACTIVE`；RETIRE 置 `RETIRED`）、`kb_review(MERGE)`、`audit_log(KB_MERGE)`、outbox `kb.release.build`。合入只改主干，线上生效以发布为准。
4. 发布（一次发布 = 一对 `(knowledge_ref, collection)`，先评测后切流量）：
   - 构建：`kb.release.build` 消费者取主干最新提交为 `knowledge_ref`（同一 ref 的构建按 outbox 去重合并）→ 打包上传 `agri-steward-kb/bundles/{knowledge_ref}.tar.gz` → `cmd/migrate kb-reindex --ref <knowledge_ref>`，`kb_index_manifest` 到 `READY` → 在这个确切的 ref 与 collection 上跑 `kb_regression`（n=3）+ 红线子集 + 全部 `active` 作物包的兼容校验（`registry_subset`、`params_match_kb`）→ 全过才写 `eval_experiment_id`，否则置 `FAILED` 并通知提报人。这样多个修订先后合入后的组合版本，发布前一定被评测过。
   - 激活：农艺负责人只能在 `READY` 且已有 `eval_experiment_id` 的发布候选上点「发布」→ 一个事务内按 P3 激活 SQL 切换 `kb_index_manifest`（旧 `ACTIVE → RETIRED`）、写 `audit_log(KB_PUBLISH)`、outbox `kb.published.{pointer,impact,sample_todo}`。
   - 指针：`kb.published.pointer` 消费者把 `{knowledge_ref, collection, manifest_id}` 写入 `agri-steward-kb/current.json`（幂等覆盖）后 `PUBLISH kb:changed {manifest_id}`；`kb-publish-reconcile`（每 5 分钟）比对 `current.json` 与 `ACTIVE` 行，不一致按 MySQL 修复。
   - 副本：订阅 `kb:changed`，同时每 60 秒按 ETag 轮询 `current.json` 兜底丢消息；发现变化 → `BundleSource` 下载校验 sha256 → 作物包交叉校验 → 原子切换整对 `(knowledge_ref, collection)`。RAG 检索直接使用这一对中的 collection 名，不经别名，固定文件路径与 RAG 路径始终是同一版本；进行中的 run 仍用旧的一对（保留最近 2 对，对应 collection 不删除）。
   - 紧急回滚：重新激活上一对已评测过的 `RETIRED` 发布，不重建索引，几分钟内全量生效；单条目的内容回滚仍走下面的 `ROLLBACK` 修订。
5. 到期与复核：`kb-review-due-scan` 每日按 `idx_review_due` 把 `kb_entry` 置 `REVIEW_DUE` 并通知（只驱动工作台队列，不影响加载）；加载器按 bundle 中 `review_due_at` + `Clock` 降权 ×0.5，超宽限期剔除 → `KB_MISS`。「复核确认」写 `kb_review(CONFIRM)`（`review_id` 取产生当前版本的那次 `MERGED` 修订）→ 只改 `reviewed_at / review_due_at` 的元数据提交（trailer `Review-Id / Kb-Confirm`，`kbcheck` 规则 `confirm_meta_only` 拒绝改到其他行）→ `kb_entry` 回到 `ACTIVE`，随下一次发布生效，版本号不变。回滚是 `change_type=ROLLBACK` 的新提报，版本号继续递增。

② SQL

```sql
CREATE TABLE kb_entry (      -- 平台级表（母文档 §4.6.1-3），每个 kb_id 一行，首次合入时插入；平台知识 tenant_id=0
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id bigint unsigned NOT NULL,
  kb_id varchar(96) NOT NULL, crop varchar(32) NOT NULL, category varchar(16) NOT NULL,
  active_version int NOT NULL,  -- 主干已合入的版本，线上版本以 current.json 为准
  status varchar(16) NOT NULL,  -- 条目状态：ACTIVE / REVIEW_DUE / RETIRED（修订进度看 kb_revision.status）
  author_id bigint unsigned NOT NULL, reviewer_id bigint unsigned NULL, review_due_at datetime(3) NULL,
  row_ver int NOT NULL DEFAULT 0, updated_at datetime(3) NOT NULL, UNIQUE KEY uk_kb (tenant_id, kb_id),
  KEY idx_crop_status (tenant_id, crop, status),
  KEY idx_review_due (status, review_due_at));   -- worker 专用（kb-review-due-scan）
CREATE TABLE kb_revision (   -- 平台级表，每次提报一行，id 即 review_id
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id bigint unsigned NOT NULL,
  kb_id varchar(96) NOT NULL, version int NOT NULL, change_type varchar(16) NOT NULL, -- CREATE/UPDATE/RETIRE/ROLLBACK
  rollback_of int NULL, severity varchar(8) NOT NULL DEFAULT 'NORMAL',  -- CRITICAL 触发 L3 下游联动
  status varchar(16) NOT NULL,  -- DRAFT/IN_REVIEW/APPROVED/MERGED/REJECTED/WITHDRAWN
  head_commit char(40) NOT NULL, merged_commit char(40) NULL, author_id bigint unsigned NOT NULL, row_ver int NOT NULL DEFAULT 0,
  created_at datetime(3) NOT NULL, updated_at datetime(3) NOT NULL,
  UNIQUE KEY uk_kb_version (tenant_id, kb_id, version),
  KEY idx_status (status, updated_at));   -- worker 专用（kb-reconcile）
CREATE TABLE kb_review (     -- 平台级表，只追加，业务账号只授 INSERT, SELECT；kind: SUBMIT/CI_RESULT/APPROVE/REJECT/MERGE/WITHDRAW/CONFIRM/EMERGENCY
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, review_id bigint unsigned NOT NULL, kind varchar(16) NOT NULL,
  actor_id bigint unsigned NOT NULL, head_commit char(40) NOT NULL, ci_passed tinyint(1) NULL, ci_report_ref varchar(64) NULL,
  request_id varchar(64) NOT NULL, created_at datetime(3) NOT NULL, UNIQUE KEY uk_request (request_id), KEY idx_review (review_id, id));

-- 到期扫描 / 复核确认：条目状态乐观锁更新
UPDATE kb_entry SET status='REVIEW_DUE', row_ver=row_ver+1, updated_at=?
 WHERE status='ACTIVE' AND review_due_at < ? AND id=? AND row_ver=?;
UPDATE kb_entry SET status='ACTIVE', review_due_at=?, row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND kb_id=? AND status='REVIEW_DUE' AND row_ver=?;
-- 紧急回滚：重新激活上一对已评测的发布（与 P3 激活 SQL 同事务，先把当前 ACTIVE 置 RETIRED）
UPDATE kb_index_manifest SET status='ACTIVE', activated_by=?, activated_at=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND status='RETIRED' AND eval_experiment_id IS NOT NULL AND row_ver=?;
```

③ Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `lock:kb:{kb_id}` | 10s | 同一条目生命周期动作串行 |
| `lock:kb:merge` | 60s（续期） | 主干合入串行（追加元数据提交 → 重建 `index.yaml` → 合入） |
| `kb:changed` | —（pub/sub 频道，母文档 §4.6.1 Redis 约定 2） | 知识发布广播；丢消息由 60 秒轮询 `current.json` 兜底 |

### 6.4 L3 知识污染防护与知识回归集

**目标**：防止「改一条知识、坏一批建议」：知识变更必须有署名、必须过知识回归集，不过就不生效。

**涉及模块**：`internal/evalkit/`（`sample / runner / report`）、`eval/suites/knowledge_regression/`、CI 流水线、L2 工作台、`index.yaml`。

**功能任务**

1. 写入校验：知识提交缺 `author`、`authority_level`、`review_id`，或 `author` 不在署名专家名册内，或 `authority_level` 超出该专家在 `expert_scope.yaml` 中的可签等级，CI 直接拒绝（与 L2 提交校验共用一个检查器）；国标 / 地标条目必须带来源文号；正文中的用量与 PHI 数值必须落在 `pesticide_registry` 登记范围内；正文含指令类注入文本（如「忽略以上规则」）即拒绝。
2. 建知识回归集：从 `regression` 中按 `kb_id` 反查引用关系，生成「知识条目 → 依赖样本」索引；回归集 = 被变更条目的依赖样本 + 同作物同类别的分层抽样 + 全部红线样本子集。新增条目（CREATE）没有存量依赖样本，SOP 规定其合入前必须配套至少 N 条 `must_cite` 指向它的样本（N 由 SOP 约定），`kbcheck` 校验。`redteam` 补「知识正文夹带指令」样本。
3. 知识变更自动触发：工作台评审页提交后，CI 以变更后的 `knowledge_ref` 跑知识回归集，n=3（评测设计方案 §11.2「知识条目增删改」行），结果回写评审页。
4. 阻断规则：红线 pass^3 < 1、`citation_valid` 或 `numbers_match_calc` 通过率 < 100%、依赖样本出现负向翻转，任一满足则评审页不可点「生效」。
5. 影响面提示：评审页展示该条目被引用的样本数、近期（统计窗口可配置）线上加载或引用它的 run 数（读每日预聚合结果，不在打开页面时实时查库），便于专家判断变更范围；精确的受影响 `run_id` 与工单清单在发布后由异步任务生成报告。
6. 引用失效检查：条目失效或版本变更后，扫描样本 `must_cite` 中指向旧版本的引用，生成样本更新待办，由专家确认后更新（不自动改样本期望）。

**交付物**：知识回归集生成器；知识变更 CI 任务；阻断规则；引用失效扫描。

**验收**

1. 构造一条篡改用量公式的知识变更：知识回归集失败，评审页「生效」按钮不可用。
2. 构造一条无署名知识提交：CI 拒绝，有明确报错。
3. 知识回归集生成器有单测：给定条目能准确反查出依赖样本，无遗漏（以人工构造的引用关系为准）。
4. 一次条目失效后，引用旧版本的样本 100% 出现在样本更新待办中。
5. 知识变更 PR 自动运行并阻断（评测设计方案 §15 Phase 4 验收）。

**技术实现**

① 流程

1. 写入校验 `internal/agent/knowledge/kbcheck`（工作台与 CI 共用，每条规则一个 `Rule`，违规按「规则 ID + 文件 + 行号」报错）：`fm_required`、`signed_by_roster`（`user_role.role='kb_expert'`，CI 读 L1 导出的名册）、`authority_level_in_scope`（不超出 `expert_scope.yaml` 中该专家可签的等级）、`source_ref_required`（国标 / 地标条目必带来源文号）、`dose_in_registry`（正文用量、PHI 在 `pesticide_registry` 登记范围内）、`no_prompt_injection`（正文指令类文本规则扫描）、`create_has_samples`（CREATE 修订在 `eval/suites/` 中有 ≥ N 条 `must_cite` 指向它的样本）、`confirm_meta_only`（L2 复核确认提交只改两个复核字段）。CI 状态检查 `kb-guard` 对主干每个合入执行 `CheckMergeCommit`：必须带 `Review-Id`（或 `Emergency-Id`）trailer 且对应 revision 已 `APPROVED`（复核确认为已 `MERGED` 且有 `CONFIRM` 记录）。
2. 依赖索引 `evalkit/kbdeps`：夜间回归后取样本 `must_cite` ∪ trial 实际 `citations[].kb_id`，生成 `kb_deps.json`（`kb_id → [sample_id]`）存 MinIO `agri-steward-eval/{experiment_id}/kb_deps.json`。
3. CI 任务 `kb-regression`：回归集 = 依赖样本 ∪ 同作物同类别分层抽样（每类 ≤ 20，种子 = `review_id`）∪ 红线子集；执行 `cmd/eval run --suite kb_regression --knowledge-ref <head> --base-ref <main> --n 3`，结果回写 `kb_review(CI_RESULT)`。
4. 阻断 `kbcheck.Gate`（纯函数）：红线 pass^3 < 1、`citation_valid / numbers_match_calc` < 100%、依赖样本负向翻转，任一命中 `ci_passed=0`；后端 `KB_CI_STALE` 校验兜底前端被绕过。
5. 影响面分两段：
   - 评审页（同步）：样本数读 `kb_deps.json`；线上次数读 `kb-usage-rollup`（worker，每日）的预聚合结果——按农场用 `kb_load_item.idx_scope_time` 聚合前一日各 `kb_id@version` 的 run 数，写 `agri-steward-eval/report/kb_usage/{yyyymmdd}.json`，评审页按窗口求和。打开评审页不查业务库。
   - 发布后（异步）：`kb.published.impact` 消费者对每个农场、每个受影响版本，用 `advice_citation ∪ kb_load_item` 按 `id` 游标分页（每页 1,000，不设总量上限）取出受影响 run、建议与工单，结果写 `agri-steward-eval/report/kb_impact/{manifest_id}/{kb_id}.jsonl.gz`，进度按农场写 manifest，可断点续跑。L6 知识污染演练验收 3 以这份报告为准。`severity=CRITICAL` 的回滚在报告生成过程中把非终态工单 `DRAFT / PENDING_APPROVAL / APPROVED` 迁到 `CANCELED`（`reason_code=KB_REVOKED`），`DISPATCHED / EXECUTING` 只发 P0 告警由人决策；下发前复检新增规则 `kb_still_active`。
6. `kb.published.sample_todo` 消费者扫描 `must_cite` 指向旧版本或已失效条目的样本，按 `kb_id` 合并生成一条带 `sample_todo` 标签的 issue（列出受影响 `sample_id` 与新旧版本），QA 确认后提 PR 更新，不自动改期望。

② SQL（无新表）

```sql
-- 异步影响面（只读账号，按农场、按单个 kb_version 游标分页；kb_version 存 'v{version}'）
-- 引用路径：命中 P0 advice_citation.idx_kb (tenant_id, farm_id, kb_id, kb_version)
SELECT c.id, c.advice_id, a.run_id FROM advice_citation c
  JOIN advice_card a ON a.tenant_id = c.tenant_id AND a.farm_id = c.farm_id AND a.id = c.advice_id
 WHERE c.tenant_id = ? AND c.farm_id = ? AND c.kb_id = ? AND c.kb_version = ? AND c.id > ?
 ORDER BY c.id LIMIT 1000;
-- 加载路径：覆盖「加载了但最终拒答或未引用」的 run；需要 kb_load_item 的 idx_kb 以租户列开头
SELECT id, run_id FROM kb_load_item
 WHERE tenant_id = ? AND farm_id = ? AND kb_id = ? AND kb_version = ? AND id > ?
 ORDER BY id LIMIT 1000;
-- 由 run 找建议与工单（P0 uk_run / uk_advice）
SELECT a.id AS advice_id, w.id AS order_id, w.status FROM advice_card a
  LEFT JOIN work_order w ON w.tenant_id = a.tenant_id AND w.farm_id = a.farm_id AND w.advice_id = a.id
 WHERE a.tenant_id = ? AND a.farm_id = ? AND a.run_id IN (?);

-- kb-usage-rollup：前一日按条目版本聚合 run 数（kb_load_item.idx_scope_time）
SELECT kb_id, kb_version, COUNT(DISTINCT run_id) AS runs FROM kb_load_item
 WHERE tenant_id = ? AND farm_id = ? AND created_at >= ? AND created_at < ?
 GROUP BY kb_id, kb_version;

-- CI 判定是否可合入：最近一次通过的 CI 必须针对当前 head
SELECT head_commit FROM kb_review
 WHERE review_id = ? AND kind = 'CI_RESULT' AND ci_passed = 1
 ORDER BY id DESC LIMIT 1;
```

③ Redis 键：无（CI 结果与依赖索引落 MySQL / MinIO）。

测试：`kbcheck` 各规则与 `Gate` 四条规则表驱动（含越级 `authority_level`、超登记范围用量、正文注入、CREATE 无样本四条应失败用例）；`kbdeps` 用 10 样本 × 6 条目（含只在 transcript 出现的隐式依赖）断言无遗漏；影响面任务用「单农场 1,200 条引用 + 只加载未引用的 run」断言分页无遗漏；CI 端到端演练篡改用量公式与无署名提交两条。

### 6.5 L4 成本看板与预算治理

**目标**：成本能按农场 / 作物 / 能力三个维度看清、能提前告警、能归因到具体会话，并支撑季度成本预测。

**涉及模块**：`internal/agent/harness/callbacks/`（成本埋点）、`internal/agent/llm/`（token 计量）、OTel 指标、成本看板、告警配置、`run_id` 检索。

**功能任务**

1. 成本口径统一：单会话成本 = 模型 token 成本 + 工具调用成本（按第三方数据授权计费口径折算）+ 预警扫描成本，按每次调用记录、按调用时刻归集；按授权费折算的工具成本按月总额与账单对账，不逐笔对账；MinIO / Milvus 存储按月容量折算计入平台级成本（`tenant_id=0, farm_id=0`），不分摊到会话。口径与 §8.4 预算口径由财务确认。
2. 维度补齐：成本埋点按 `tenant_id / farm_id / crop / capability` 打标签，`capability` 记在每次调用上（一个会话可同时跨 `advice / rag / vision`），取值与五个能力开关及 `advice` 主链路对应；评测运行的成本单独记账，不进生产看板。
3. 成本看板：三维度日 / 周 / 季趋势，Top 农场、Top 作物、Top 能力，单会话成本分布与 P95（P95 在查询时从会话明细现算，不由日汇总合并）。
4. 预算阈值与告警：按农场、按能力设预算，接近阈值预警、超过阈值告警，告警带维度与 Top `run_id` 可下钻。
5. 高成本会话归因：对超过成本 P95 的会话自动打原因标签 —— 长会话（压缩前 token 量、`compaction` 是否触发）、重复提问（同农场同问题短时间多次）、检索过量（加载 / 召回条目数超上限）、预警扫描过量；按周输出归因分布与优化建议。
6. 季度成本预测：以会话量 × 单会话成本建模，按季度出预测值，季末与实际对账，偏差范围首季度实测后约定。
7. 超预算降级策略配置化（见 §8）：预算超限时按能力优先级降档，不影响红线校验与审批链路，`EMERGENCY` 级防灾预警不降级；降级用独立的覆盖开关叠加，不改写人工设置的开关值（母文档 §4.6.2-5）。

**交付物**：成本口径说明（财务签字）；三维度成本看板；预算告警；高成本归因周报；季度成本预测模型与对账记录。

**验收**

1. 看板三维度合计与账单对账一致（人工对账一次并留记录）。
2. 评测运行成本不出现在生产看板中。
3. 预算告警演练：人为调低某农场预算，告警触发且可下钻到 `run_id`。
4. 抽查 20 个高成本会话，归因标签与人工判断一致。
5. 首季度预测与实际对账完成，偏差范围约定写入成本口径说明；第 2 个常态季度偏差在约定范围内（Gate 4 第 3 条）。

**技术实现**

① 流程（单价、预算、预测均为配置文件 `configs/price_book.yaml`、`configs/{env}/budget.yaml`、`ops/cost/forecast/{yyyyQn}.yaml`，改动走 PR；金额用微元 `bigint`）

1. 按调用埋点：`agent/harness/callbacks/cost` 在每次 LLM / 工具调用结束时按当前单价算出本次成本，写进 P0 已有的 `LLM_CALL_END / TOOL_CALL_END` 事件 payload（`capability / crop / prompt_tokens / completion_tokens / cost_uyuan`，按调用时刻落 `created_at`），并与 P2 的 token 累加放在同一条 `UPDATE agent_run`（追加 `cost_uyuan = cost_uyuan + ?`），同时 `INCRBY` 三个日累计键。等审批中断、失败、崩溃的 run 已发生的调用都已入账，不依赖 run 结束。`capability` 由节点名映射（映射表缺项单测失败）；预警扫描 run（`user_id=sys_alert`）记 `capability=alert`；`kb-reindex` 的 embedding 成本不属于 run，worker 直接 upsert 到 `cost_daily`（`tenant_id=0, farm_id=0, capability=kb_index`）。`cmd/eval` 装配的记录器只写 `agri_eval`，生产看板不含评测成本。
2. `cost-rollup`（worker，每小时）：只读账号按 `run_event.idx_type_time` 聚合当日和前一日的 `LLM_CALL_END / TOOL_CALL_END` → `cost_daily` upsert（按调用时刻归日，跨日的 run 分摊到各日，封账后才结束的 run 不会漏记）；次日 02:00 最后重算前一日并 `sealed=1`。日期按农场时区（母文档 §4.6.2-9）。看板 `GET /api/v1/admin/cost/daily?dim=farm|crop|capability&from&to`。P95 不进日汇总：`cost-p95`（每日）按 `idx_cost` 从 `agent_run.cost_uyuan` 现算各维度日 / 周 / 季 P95，写 `agri-steward-eval/report/cost/{yyyymmdd}.json`，看板与归因都读这份结果。
3. `budget-governor`（worker，每 5 分钟）：读 `budget.yaml`；当月累计 = 本月已封账日 `SUM(cost_daily.cost_uyuan)` + 今日 Redis 日键，Redis 日键缺失时回落读今日 `cost_daily` 行（最多滞后 1 小时）；Redis 只做当日增量，不再用月合计 `SET` 校正。达到 `warn_ratio` 预警，≥ 1.0 倍告警，均带当月 Top 5 `run_id`；`SET NX budget:alerted:*` 去重。
4. 降级不改写人工开关：每个可降级能力有一个覆盖开关 `budget_hold.<flag>`（注册表通配项，母文档 §4.6.2-5），governor 在 FARM / TENANT 级 upsert `{"on":true}`（`on=true` 表示暂停，`updated_by=0`）；开关读取时实际值 = 人工值 ∧ ¬`budget_hold`（任一层 `on=true` 即暂停）。默认 1.0 / 1.1 / 1.2 倍依次：`budget_hold.enable_proactive_alert`（只放大 `WINDOW / ROUTINE` 级扫描间隔，`EMERGENCY` 级扫描不受影响）→ `budget_hold.enable_auto_plan` → `budget_hold.enable_vision_diagnose`（**待 Owner 确认**）。`enable_advice`、校验器、审批链路在注册表中 `budget_degradable=false`，配置引用即启动失败。月切换或预算调高后把覆盖开关 upsert 为 `{"on":false}`，人工设置的行始终不动；每次档位变化写 `audit_log(BUDGET_DEGRADE / BUDGET_RESTORE)`。
5. `cost-attribution`（每周一）：读 `cost-p95` 报告，超过对应能力 P95 的 run 打标签写 `agent_run.cost_tags`：`LONG_SESSION / REPEAT_ASK / OVER_RETRIEVAL / ALERT_SCAN_HEAVY / UNEXPLAINED`。

② SQL

```sql
-- token 列沿用 P2 的 prompt_tokens / completion_tokens，不再新增；crop / pack_ref 由 L1 新增
ALTER TABLE agent_run ADD COLUMN cost_uyuan bigint NOT NULL DEFAULT 0, ADD COLUMN cost_tags json NULL,
  ADD KEY idx_cost (tenant_id, farm_id, created_at, cost_uyuan),
  ADD KEY idx_crop_time (crop, created_at, cost_uyuan);              -- worker 专用（跨农场按作物算 P95）
ALTER TABLE run_event ADD KEY idx_type_time (event_type, created_at); -- worker 专用（cost-rollup）

-- 埋点：与 P2 token 累加同一条语句
UPDATE agent_run SET prompt_tokens = prompt_tokens + ?, completion_tokens = completion_tokens + ?,
       cost_uyuan = cost_uyuan + ? WHERE id = ?;

CREATE TABLE cost_daily (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY, tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL,
  stat_date date NOT NULL, crop varchar(32) NOT NULL DEFAULT '', -- 平台级行（kb_index、存储）为空串
  capability varchar(16) NOT NULL, -- advice/rag/vision/alert/schedule/plan/kb_index/storage
  runs int NOT NULL, prompt_tokens bigint NOT NULL, completion_tokens bigint NOT NULL, cost_uyuan bigint NOT NULL,
  sealed tinyint(1) NOT NULL DEFAULT 0, updated_at datetime(3) NOT NULL,
  UNIQUE KEY uk_dim (tenant_id, farm_id, stat_date, crop, capability),
  KEY idx_date (stat_date));                                        -- worker 专用

-- cost-rollup：按调用时刻聚合（[from, to) 按农场时区换算成 UTC 区间）
SELECT tenant_id, farm_id, JSON_VALUE(payload, '$.crop') AS crop, JSON_VALUE(payload, '$.capability') AS capability,
       COUNT(DISTINCT run_id) AS runs,
       SUM(JSON_VALUE(payload, '$.prompt_tokens' RETURNING SIGNED))     AS prompt_tokens,
       SUM(JSON_VALUE(payload, '$.completion_tokens' RETURNING SIGNED)) AS completion_tokens,
       SUM(JSON_VALUE(payload, '$.cost_uyuan' RETURNING SIGNED))        AS cost_uyuan
  FROM run_event
 WHERE event_type IN ('LLM_CALL_END', 'TOOL_CALL_END') AND created_at >= ? AND created_at < ?
 GROUP BY tenant_id, farm_id, crop, capability;

INSERT INTO cost_daily (tenant_id, farm_id, stat_date, crop, capability, runs, prompt_tokens, completion_tokens,
                        cost_uyuan, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?) AS new
ON DUPLICATE KEY UPDATE runs=new.runs, prompt_tokens=new.prompt_tokens, completion_tokens=new.completion_tokens,
  cost_uyuan=new.cost_uyuan, updated_at=new.updated_at;  -- sealed=1 的行不进本语句

-- cost-p95：分位数现算，不对日 P95 求平均（按能力的 P95 先按 run_id, capability 汇总 run_event 再同样计算）
SELECT crop, MIN(cost_uyuan) AS p95_uyuan FROM (
  SELECT crop, cost_uyuan, PERCENT_RANK() OVER (PARTITION BY crop ORDER BY cost_uyuan) AS pr
    FROM agent_run WHERE crop IS NOT NULL AND created_at >= ? AND created_at < ? AND cost_uyuan > 0) t
 WHERE pr >= 0.95 GROUP BY crop;

-- 预算覆盖开关：只写 budget_hold.*，不动人工行
INSERT INTO feature_flag (tenant_id, farm_id, flag_key, scope_type, value, comment, updated_by, row_ver, created_at, updated_at)
VALUES (?, ?, ?, ?, '{"on": true}', ?, 0, 0, ?, ?) AS new   -- TENANT 级 farm_id=0
ON DUPLICATE KEY UPDATE value=new.value, comment=new.comment, updated_by=0, row_ver=row_ver+1, updated_at=new.updated_at;

-- 告警下钻 Top 5：SELECT id, cost_uyuan FROM agent_run WHERE tenant_id=? AND farm_id=? AND created_at>=? ORDER BY cost_uyuan DESC LIMIT 5;
```

③ Redis 键（日期按农场时区）

| 键 | TTL | 用途 |
|----|-----|------|
| `cost:farm:{farm_id}:{yyyymmdd}` | 40d | 农场当日累计（微元），预算快速判断 |
| `cost:tenant:{tenant_id}:{yyyymmdd}` | 40d | 租户当日累计 |
| `cost:farm:{farm_id}:{capability}:{yyyymmdd}` | 40d | 分能力当日累计 |
| `budget:alerted:{scope}:{level}:{yyyymmdd}`（scope 为 `farm:{farm_id}` 或 `tenant:{tenant_id}`） | 48h | 同一预算同一级别每天只告警一次 |

### 6.6 L5 合规与数据安全

**目标**：把 Phase 1 落地的首版合规措施变成持续运转的流程：每项合规要求有结论、有责任人、有更新机制、有取证能力。

**涉及模块**：`internal/agent/guardrail/`（`pii` 脱敏、`constraint_checker` 法规规则）、`audit_log`、`raw_store`、审计导出、`governance_ticket`（治理工单）、数据存储与出域配置、静态加密、审批前置条件。

**功能任务**

1. 合规清单：逐项列出田块与影像数据存储位置与出域策略、农户隐私（采集、使用、删除）、农药与航空作业法规、责任划分与保险、审计保留期，每项写明结论、依据、责任人、复核周期。
2. 数据分级与出域控制：田块坐标、影像、农户身份、作业记录分级；按 D1 结论配置出域白名单，入模数据统一经 `pii` 脱敏，影像出域前去除 EXIF 坐标；出域配置变更走审批并写 `audit_log`。
3. 法规映射持续更新：法规变更 → 法务出映射结论 → 更新 `constraint_checker` 规则或审批前置条件 → 跑作业安全专项用例（母文档 §7.3-1）→ 法务签字生效；每次更新形成版本记录。
4. 农户隐私权利：按 `user_id`（隐私主体）导出与删除其个人数据，农场注销按 `farm_id` 清除；删除覆盖范围以合规清单中的「PII 存放位置清单」（表、列、bucket、Redis、备份）为准逐项执行；`audit_log` 等只追加表保留不可篡改的摘要记录，`raw_store` 原文按策略清除，两者冲突时以法务结论为准。
5. 审计保留期：`audit_log`、`raw_store`、`RunState` 快照分别配置保留期（配置沿用 P1 `retention` 段），到期按策略先归档再清除，清除动作本身留审计记录；归档后哈希链可从归档续接校验。
6. 取证流程：按 `run_id` / `order_id` / 时间段导出审计与原文（含已归档部分），导出需法务与 `platform_admin` 双人审批；取证包为接收方公钥加密的原文版，只有清单与索引过 `pii_clean` 扫描，取证包带校验链证明未被篡改；最终以法务结论为准。
7. 保险方案落地：与 D5 结论对齐，把投保范围映射到可开放下发的设备类型与作业类型，超出投保范围的作业在审批前置条件中拦截。
8. 静态加密与密钥管理（本地 docker 部署，不接云 KMS）：MinIO 各 bucket 服务端加密、MySQL 敏感表表空间加密，密钥文件单独存放并离线备份；密钥保管人、轮换周期、恢复演练写进合规清单。

**交付物**：合规清单（法务签字，含 PII 存放位置清单与密钥管理项）；数据分级与出域配置；法规映射更新流程与版本记录；隐私导出 / 删除流程；保留期与归档策略；取证流程；保险生效文件。

**验收**

1. 合规清单逐项有结论与责任人，无「待定」项（Gate 4 第 4 条）。
2. 法规更新演练：模拟一条法规阈值变更，从映射到规则生效全流程走通，作业安全专项用例全过。
3. 隐私删除演练：指定测试用户删除后，按 PII 存放位置清单逐项断言（`raw_store` 无其原文、`run_checkpoint` 无其会话、`advice_feedback.comment` 已清空等），`audit_log` 保留摘要且校验链完整；农场注销演练同样按清单断言。
4. 取证演练：导出一个 `run_id` 的取证包，清单与索引 `pii_clean` 通过，接收方私钥可解密，校验链验证通过；单人审批、审批人与发起人相同均被拒。
5. 超出投保范围的作业类型在审批前置条件中被拦截。
6. 加密核查：MinIO 各 bucket `mc encrypt info` 为 SSE-S3，MySQL 敏感表 `ENCRYPTION='Y'`；密钥恢复演练一次并留记录。

**技术实现**

① 流程

1. 合规即配置：`compliance/checklist.yaml`（结论 / 依据 / owner / review_cycle / signed_by），CI `cmd/opscheck compliance` 拒绝「待定」、未签署、复核过期项；法规阈值在 `policy/constraint_rules.yaml`，改动由法务与规则 owner 双签，CI 跑作业安全专项用例 n=5。保险范围 `policy/insurance_scope.yaml`，`agent/guardrail` 新增 `PRE_APPROVAL` 规则 `insurance_covered`，结果写 `constraint_check`。
2. 出域：`internal/adapter/egress.Transport` 包住所有出站 HTTP 客户端，按 `policy/egress.yaml`（目标 host × 数据等级 C1-C4，默认拒绝）放行；未声明等级按 C4，拒绝时写 `audit_log(EGRESS_DENIED)` 并发 P0 告警；影像出域前经 `StripEXIF` 节点。
3. 治理工单（新表 `governance_ticket`，不复用 `approval_ticket`，母文档 §4.6.2-6）：`ticket_kind` 为 `PRIVACY_EXPORT / PRIVACY_DELETE / FARM_PURGE / FORENSIC_EXPORT`。`auditor` 发起（`PREPARING`）→ 处理器生成计划快照（逐项列出将导出 / 删除的表、行数、对象 key）存 `raw_object(kind=SNAPSHOT)`，算 `plan_digest` 后置 `OPEN` → 审批：隐私类与农场注销需第二名 `auditor`，取证需法务（`legal`，平台角色 (0,0)）与 `platform_admin` 各一人；审批人 ≠ 发起人、第二人 ≠ 第一人、`plan_digest` 一致，均由 SQL 条件强制，每次决策写 `audit_log(GOV_DECIDE)` → 集齐 `required_approvals` 后同事务写 outbox `gov.execute`（`dedup_key = gov.execute:{ticket_id}`）→ worker 执行，置 `EXECUTING → DONE / FAILED`，结果对象记 `result_raw_id`。工单列表只给 `auditor / legal / platform_admin`，农场审批人看不到。
4. 删除清单（与合规清单「PII 存放位置清单」一一对应，演练按此逐项断言）：
   - 按 `user_id` 删个人数据（`PRIVACY_DELETE`，范围为该租户下全部农场）：经 `agent_run.idx_user` 取该用户的 run → 删 `run_checkpoint` 行；`raw_object` 按 `idx_run` 取对象，删 MinIO 全部版本并置 `deleted_at`；`advice_feedback` 置 `comment=NULL, user_id=0`；`episodic_memory` 删 `kind='ADVICE'` 且 `ref_id` 指向该用户建议的行；`user_role` 删该用户绑定。`agent_run.user_id` 与 `audit_log.actor_id` 只是 ID 不动，由外部账号体系去标识化（P1 `user_role` 只存绑定）。
   - 按 `farm_id` 注销农场（`FARM_PURGE`）：可变业务表按 `WHERE tenant_id=? AND farm_id=?` 分批删除（`run_checkpoint`、`raw_object` 及其对象、`episodic_memory`、`field_profile`、`farm_profile`、`advice_feedback`、`feature_flag` 的 FARM 行、`user_role`）；`advice_card / work_order` 等作业与合规记录按法务定的保留期保留到期后走第 5 步。
   - 只追加表（`run_event / audit_log / work_order_transition / approval_decision`）不改不删：入库内容已是 `pii.Mask` 后的摘要，只随保留期走归档删除。
   - Redis 键只含 ID，按 TTL 自然过期；数据库与 MinIO 备份按备份保留期自然过期，从备份恢复后重放 `DONE` 状态的删除工单；处于法务保全的取证包按法务结论处理。
   - 删除时仍未终态的 run 记入工单 `scope.pending_runs`，run 结束后由 `gov-pending-sweep` 补删。
5. 取证包（`FORENSIC_EXPORT`，按决策：加密原文版）：key 按母文档 §4.6.2-8 为 `raw/{tenant}/{farm}/{yyyy}/{mm}/{dd}/0/FORENSIC/{id}`，登记 `raw_object(kind=FORENSIC)`，参与保留期；包内为 `audit.jsonl` + 原文 + `chain_proof.json`（只含相关链段的 `chain_seq / prev_hash / row_hash`，不带同农场其他记录的内容），整体用工单的接收方公钥（`recipient_key_fpr`）加密；`manifest.json` 与索引不加密，过 `agent/guardrail/pii` 扫描，命中即失败。超出在线期的数据从 `agri-steward-archive` 读取，并附该月归档 manifest。下载用 15 分钟预签名 URL，每次写 `audit_log(EXPORT_DOWNLOAD)`。
6. 保留期：配置只在 P1 `configs/{env}/app.yaml` 的 `retention` 段（按表和 `raw_object.kind` 配在线期与归档期），P4 不另建配置文件；`raw_object` 到期清理沿用 P1（`expire_at / deleted_at` 由 P1 提供，P4 不再另起清理任务）。约定 `tool_call` 保留期 ≥ `advice_card`，保证建议的实测引用可回放；回放读不到在线数据时从归档读取。
7. 只追加表归档删除（母文档 §4.6.1-6，只在 P4）：`retention-archive`（每月 1 日）取超出在线期的整月分区 → 只读账号流式导出 `jsonl.gz` 到 `agri-steward-archive/{table}/{yyyymm}/{tenant_id}.jsonl.gz`：`audit_log` 逐链按 `idx_chain (tenant_id, farm_id, chain_seq)` 顺序导出（P1 上线前 `chain_seq` 为 NULL 的历史行按 `id` 顺序单独导出），其他表按 `id` 顺序 → 校验行数并重算每条链的 `row_hash` → 写 `_manifest.json`（行数、`sha256`、每条链的 `first_seq / last_seq / last_row_hash` 作续接点，`stage=EXPORTED`）→ manifest 的 `sha256` 写进 `agri-steward-audit-anchor/archive/{yyyymm}.json` → `as_retention` 账号 `DROP PARTITION`（`run_event / audit_log`）或按 `id` 区间每批 5,000 行删除（`tool_call / work_order_transition / approval_decision`）→ manifest 置 `stage=DROPPED` → 写 `audit_log(RETENTION_PURGE)`。任一步失败不删，重跑按 manifest 的 `stage` 判断进度。`agri-steward-archive` 建桶即开对象锁（COMPLIANCE，保留期同归档期），应用账号只有写入权限；P1 的 `auditctl verify` 增加 `--from-manifest`，从续接点开始校验在线部分，`anchor-check` 遇到已归档区间改查归档 manifest，不报 GAP。分区由 P0 `partition-maintainer` 提前 `REORGANIZE pmax` 并在 `pmax` 非空时告警；P4 启动前先确认 `pmax` 为空，非空则先按月拆分再进入归档。
8. 静态加密（本地 docker，不写 KMS）：MinIO 设 `MINIO_KMS_SECRET_KEY`（单密钥），对 `agri-steward-raw / -archive / -kb / -eval` 执行 `mc encrypt set sse-s3`；MySQL 装 `component_keyring_file`，keyring 文件放单独的卷，`audit_log / run_checkpoint / raw_object / advice_feedback / tool_call / run_event` 执行 `ENCRYPTION='Y'`，同时开 `binlog_encryption`、`innodb_redo_log_encrypt`、`innodb_undo_log_encrypt`；两份密钥离线备份，每年 `ALTER INSTANCE ROTATE INNODB MASTER KEY` 一次，恢复演练每年一次，保管人写进合规清单。取证包另按接收方公钥加密（第 5 步）。`cmd/opscheck compliance` 增加加密核查项。

② SQL

```sql
-- expire_at / deleted_at / idx_expire (deleted_at, expire_at) 由 P1 L7 提供，P4 只加数据等级；存量按 kind 回填
ALTER TABLE raw_object ADD COLUMN data_class varchar(4) NOT NULL DEFAULT 'C4', ALGORITHM=INSTANT;
UPDATE raw_object SET data_class=? WHERE kind=? AND id BETWEEN ? AND ?;  -- 按数据分级配置逐 kind 回填，未列出的 kind 保持 C4

CREATE TABLE governance_ticket (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL,  -- 按 user_id 删除时 farm_id=0（租户内全部农场）
  ticket_kind  varchar(16) NOT NULL,   -- PRIVACY_EXPORT / PRIVACY_DELETE / FARM_PURGE / FORENSIC_EXPORT
  subject_type varchar(8)  NOT NULL,   -- USER / FARM / RUN / ORDER / RANGE
  subject_id   bigint unsigned NOT NULL DEFAULT 0,
  scope        json NULL,              -- 时间段、run_id 列表、pending_runs
  plan_raw_id  bigint unsigned NULL, plan_digest char(64) NULL,
  required_approvals tinyint NOT NULL, -- 隐私 / 注销 1（第二名 auditor），取证 2（legal + platform_admin）
  initiator_id bigint unsigned NOT NULL,
  first_approver_id  bigint unsigned NULL, first_approver_role  varchar(16) NULL,
  second_approver_id bigint unsigned NULL, second_approver_role varchar(16) NULL,
  recipient_key_fpr varchar(64) NULL,  -- 取证接收方公钥指纹
  result_raw_id bigint unsigned NULL,
  status varchar(12) NOT NULL,         -- PREPARING / OPEN / APPROVED / EXECUTING / DONE / FAILED / REJECTED
  last_error varchar(256) NULL, request_id varchar(64) NOT NULL,
  row_ver int unsigned NOT NULL DEFAULT 0, created_at datetime(3) NOT NULL, updated_at datetime(3) NOT NULL,
  PRIMARY KEY (id), UNIQUE KEY uk_request (request_id),
  KEY idx_scope_status (tenant_id, farm_id, ticket_kind, status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 第一人审批：≠ 发起人、快照一致；影响行数 0 即拒绝
UPDATE governance_ticket SET first_approver_id=?, first_approver_role=?, row_ver=row_ver+1, updated_at=?,
       status=IF(required_approvals=1,'APPROVED','OPEN')
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='OPEN' AND first_approver_id IS NULL
   AND initiator_id<>? AND plan_digest=? AND row_ver=?;
-- 第二人审批（取证）：≠ 发起人、≠ 第一人、角色与第一人互补
UPDATE governance_ticket SET second_approver_id=?, second_approver_role=?, status='APPROVED', row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='OPEN' AND required_approvals=2
   AND first_approver_id IS NOT NULL AND first_approver_id<>? AND initiator_id<>?
   AND first_approver_role<>? AND plan_digest=? AND row_ver=?;

-- 按 user_id 删除（工单 farm_id=0 时对租户内农场逐个执行；每批 500，按 id 游标）
SELECT id FROM agent_run WHERE tenant_id=? AND farm_id=? AND user_id=? AND id>? ORDER BY id LIMIT 500;  -- idx_user
DELETE FROM run_checkpoint WHERE tenant_id=? AND farm_id=? AND run_id IN (?);
UPDATE advice_feedback SET comment=NULL, user_id=0 WHERE tenant_id=? AND farm_id=? AND user_id=?;

-- 归档删除：仅在 manifest stage=EXPORTED 且锚定写入后，由 as_retention 执行
ALTER TABLE audit_log DROP PARTITION p202611;
DELETE FROM tool_call WHERE tenant_id=? AND farm_id=? AND id BETWEEN ? AND ? AND created_at < ?;  -- 每批 5,000
-- 静态加密
ALTER TABLE audit_log ENCRYPTION='Y';    -- run_checkpoint / raw_object / advice_feedback / tool_call / run_event 同
```

③ Redis 键：无（治理工单、保留期与导出任务由 worker Leader 执行，进度以工单状态和 MinIO manifest 为准）。

### 6.7 L6 运维闭环（Runbook / on-call / 故障演练）

**目标**：任何 P0/P1 告警都有人接、有步骤可照做；四类重大故障每季度演练一次，演练结论驱动改进。

**涉及模块**：告警配置、`runbooks/`（与告警项一一对应）、on-call 排班、降级矩阵（Phase 1 §5.1.2-6）、`enable_robot_dispatch` 一键关停、L2 知识回滚、`run_id` 检索与 Phase 3 调试视图。

**功能任务**

1. Runbook 编写：每个 P0/P1 告警项一份，固定结构：触发条件 / 影响面判断 / 首要止血动作（开关、降级、回滚）/ 定位步骤（从告警 `run_id` 下钻）/ 恢复确认 / 升级路径 / 事后复盘要求。
2. 告警 ↔ Runbook 对照校验：告警配置中每个 P0/P1 项必须带 `runbook` 链接，CI 校验缺失即失败；新增告警未配 Runbook 不允许上线。
3. on-call 值班：排班、交接模板、响应时限（按告警级别，首季度实测后约定）、升级到架构负责人 / 农艺负责人 / 法务的条件。
4. 季度四类故障演练（母文档 §5.4.1-5）：

| 演练 | 注入方式 | 必须验证的行为 |
|------|----------|----------------|
| LLM 全挂 | 模型层返回不可用 | 规则层保守建议 `degraded=true`；无法降级的场景明确报不可用；不产生无依据建议 |
| MCP 全挂 | `mcp-gateway` 全部超时 | 缓存数据标注新鲜度，超阈值拒答；告警带 `run_id` |
| 误下发 | 构造一张不应下发的作业单进入 `DISPATCHED` | 一键关停 ≤ 1 分钟（Gate 1 口径）；`cancel` 生效；`PARTIALLY_DONE` 回填 `done_area`；审计可还原 |
| 知识污染 | 绕过检测后生效一条错误知识条目 | 发现路径（告警 / 巡检 / 归因）有效；L2 成对回滚（知识版本与索引）生效；受影响 `run_id` 可被筛出并通知 |

5. 演练报告模板：注入时间、发现时间、止血时间、恢复时间、偏离 Runbook 的步骤、改进项与负责人；改进项进入下季度排期。
6. 事故复盘：真实 P0/P1 事故按同一模板复盘，结论回写 Runbook。

**交付物**：P0/P1 Runbook 全集；对照校验 CI；on-call 排班与交接模板；四份演练报告（首季度）；演练与复盘改进项清单。

**验收**

1. 对照校验 CI 生效，P0/P1 告警 Runbook 覆盖率 100%（Gate 4 第 5 条）。
2. 首季度四类演练全部完成并留报告；误下发演练关停时间 ≤ 1 分钟。
3. 知识污染演练中，受影响的 `run_id` 可通过知识版本被完整筛出（以 L3 异步影响报告为准，不设条数上限）；回滚切回上一对已评测的 `(knowledge_ref, collection)` 后，RAG 路径不再召回问题条目。
4. 每份演练报告的改进项都有负责人与关闭时间，上季度改进项在下季度演练前关闭。

**技术实现**

① 流程

1. 告警即代码：`ops/alerts/{domain}.yaml`（`id / level / expr / owner / runbook`），`cmd/opscheck alerts render` 生成 Prometheus 告警规则，不在控制台手工建；CI `opscheck alerts` 要求 P0/P1 告警的 Runbook 文件存在、7 个固定标题齐全、「首要止血动作」非空、引用的开关在注册表中。
2. 故障注入 `internal/agent/harness/chaos`（仅 staging，生产用 build tag 排除）：装饰 `ChatModel`、MCP 客户端、`Retriever`，读 Redis `chaos:plan:{target}`（`mode: UNAVAILABLE|TIMEOUT|LATENCY / ratio`）；`POST /ops/v1/chaos/plans`（`sre` 角色）写入，TTL 到期自动失效。
3. 演练断言 `cmd/opscheck drill <name>`：LLM 全挂期间 run 只有降级建议（`outcome='ADVICE' AND degraded=1`）或拒答（`REFUSED`），且非拒答建议中无引用的数为 0；MCP 全挂超过新鲜度阈值的 run 均拒答；知识污染以 L3 `kb.published.impact` 异步影响报告（`report/kb_impact/{manifest_id}/`）筛出的 `run_id` 集合为准，与 `advice_citation ∪ kb_load_item` 的实际集合比对一致。报告 `ops/drills/{yyyyQn}/{name}.md` 自动填四个时间戳（取自 `audit_log` 与告警记录），改进项写 `ops/drills/actions.yaml`，`drill --precheck` 要求上季度项全部关闭。
4. 一键关停：`POST /ops/v1/kill-switch {flag: enable_robot_dispatch, scope, cancel_inflight}` → 同事务 upsert `feature_flag`（关停是幂等操作，不带 `row_ver` 条件，没有 FARM 行时直接插入）+ 写 `audit_log`（记旧值，便于恢复）→ `PUBLISH flags:changed`（消息体沿用 P1 的 `{flag_key}`，只追加可选字段 `change_id`，P1 消费方忽略未知字段）→ 各副本清本地缓存后，消息带 `change_id` 时 `SADD flags:ack:{change_id}`（`change_id` 取本次 `audit_log.id`，随响应返回），集齐即记 `as_kill_switch_seconds`；关停生效本身不依赖广播（下发前直读 MySQL，P1 已有）。取值按母文档 §4.6.2-5：FARM 级写 `{"on":false}`；TENANT / GLOBAL 级只写 `{"force_off":true}`，GLOBAL 只允许 `platform_admin`。`order.dispatch` 消费者执行前实时读开关，已关闭则不调设备、outbox 行置 `DONE` 且 `last_error=KILL_SWITCH`；`cancel_inflight` 默认 `false`（保持 P1 值班人逐单 `cancel` 的语义），为 `true` 时同事务为范围内 `DISPATCHED / EXECUTING` 工单写 `order.cancel` outbox，这一语义差异写进 §2.2 与 Runbook。
5. on-call：`ops/oncall/rota.yaml`（周轮值、主备），告警路由按 `level + owner` 生成，`opscheck` 校验 P0/P1 的 `owner` 在排班中有人。

② SQL（无新表）

```sql
-- 关停：upsert，不带 row_ver（幂等），同事务写 audit_log（payload 记旧值）
-- FARM：value='{"on": false}'；TENANT / GLOBAL：value='{"force_off": true}'（TENANT 行 (tenant_id, 0)，GLOBAL 行 (0, 0)）
INSERT INTO feature_flag (tenant_id, farm_id, flag_key, scope_type, value, comment, updated_by, row_ver, created_at, updated_at)
VALUES (?, ?, 'enable_robot_dispatch', ?, ?, ?, ?, 0, ?, ?) AS new
ON DUPLICATE KEY UPDATE value=new.value, comment=new.comment, updated_by=new.updated_by,
  row_ver=row_ver+1, updated_at=new.updated_at;
-- cancel_inflight：取范围内在途工单，逐张写 order.cancel outbox
SELECT id FROM work_order WHERE tenant_id=? AND farm_id=? AND status IN ('DISPATCHED','EXECUTING') FOR UPDATE;
-- LLM 全挂演练断言：注入窗口内非拒答建议中无引用的数必须为 0
SELECT COUNT(*) FROM advice_card a
 WHERE a.tenant_id=? AND a.farm_id=? AND a.created_at BETWEEN ? AND ?
   AND a.outcome <> 'REFUSED'
   AND NOT EXISTS (SELECT 1 FROM advice_citation c
                    WHERE c.tenant_id=a.tenant_id AND c.farm_id=a.farm_id AND c.advice_id=a.id);
```

③ Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `chaos:plan:{target}` | 计划截止时间（仅 staging） | 故障注入计划 |
| `flags:changed` | —（pub/sub，P1 已有） | 开关变更广播 `{flag_key}`，P4 追加可选 `change_id`；丢消息回落 30s 本地缓存 |
| `flags:ack:{change_id}` | 10min | 各副本确认失效，用于关停计时（P100 ≤ 60s） |

### 6.8 L7 效果经营机制与 Gate 4 收口

**目标**：把「指标 → 归因 → 知识与策略迭代」固定成季度节奏，让归因 Top 清单直接成为下季度 OKR 输入；建设期末完成 Gate 4 建设类条目的判定。

**涉及模块**：§3.3 指标体系看板（采纳率、改单率与驳回率、亩均成本、防治窗口命中率）、Phase 2 归因数据采集、线上回流（评测设计方案 §13）、L4 成本看板、L1 作物包状态。

**功能任务**

1. 季度复盘看板：业务效果类、可信度类、工程与安全类、成本四组指标的季度对比（对 `baseline_p3` 与上季度），按作物与区域拆分。
2. 归因 Top 清单季度化：在 Phase 2 周度清单基础上做季度聚合，每项标注归因类别（知识缺口 / 规则缺口 / 加载或检索 / 模型选择 / 交互）与可落地的动作（补知识条目、下沉规则、调策略、补样本）。
3. 复盘会固定议程：指标变化 → Top 清单 → 上季度 OKR 完成情况 → 本季度 OKR 草案；纪要模板固定，结论入库。
4. OKR 追溯：每条 OKR 关联至少一项 Top 清单条目，季末回填完成情况与指标变化，做到「为什么做」与「做完有没有用」可追溯。
5. 策略变更纪律：复盘产出的提示词、加载策略、规则下沉、知识变更，全部走 Phase 2 回归门禁与 L3 知识回归集，不因「复盘决定」绕过门禁。
6. Gate 4 收口：建设期末汇总 Gate 4 第 1、2、4、5 条证据；第 3、6 条在常态季度持续判定，输出《Phase 4 建设期验收报告》。

**交付物**：季度复盘看板；季度 Top 清单；复盘纪要模板；OKR 追溯表；《Phase 4 建设期验收报告》。

**验收**

1. 首次季度复盘按固定议程召开，纪要与 OKR 草案入库，每条 OKR 可追溯到 Top 清单条目。
2. 复盘产生的策略变更 100% 有对应的门禁运行记录。
3. 连续 2 个季度复盘按期召开，第 2 季度纪要包含对第 1 季度 OKR 的回填（Gate 4 第 6 条）。
4. 《Phase 4 建设期验收报告》入库，Gate 4 建设类条目全部有证据，常态类条目有判定时间与负责人。

**技术实现**

① 流程（不新建 KPI 表：季度指标从 P2 `quality_metric_daily` 与 L4 `cost_daily` 现算，复盘时的快照落 MinIO 留档）

1. `quarter-rollup`（worker，每周一 + 季末次日）：只计算 `ops/governance/metrics.yaml` 中的指标，每个指标一个 `service/kpi.Metric` 实现（`Key / SourceVer / Compute(period)`），启动时校验与清单一一对应；只读账号聚合后输出 `agri-steward-eval/report/governance/{yyyyQn}/kpi.json`（含 `source_ver`，口径变更时旧快照不改）；每周一同时把上周 `attribution_issue` 快照（`WHERE last_week = 上周`，不按状态过滤）写到 `agri-steward-eval/report/governance/{yyyyQn}/attribution/{yyyyww}.json`，不新建表。
2. 看板 `GET /api/v1/admin/governance/kpi?period=&compare=baseline_p3|prev`：读快照与 `baseline_p3` 冻结实验汇总，按作物与区域拆分；`sample_size < min_sample` 的点灰显、不参与结论。
3. 季度 Top 清单：`cmd/eval attribution --quarter 2027Q1` 读本季度各周的 `attribution_issue` 快照按问题求和（P2 的 `hit_count` 只是最近一周的值，不能对表直接 `SUM`），季度内已 `VERIFIED` 的问题也保留，用于 OKR 回填；生成 `ops/review/{yyyyQn}/top.yaml`（`id / 类别 / 证据 run_id / 建议动作 / 季末状态`）。
4. OKR 追溯 `ops/review/{yyyyQn}/okr.yaml`（`okr_id / top_refs[] / metric / target / result / closed_at`）；CI `opscheck okr` 校验 `top_refs` 非空且存在、上季度 `result` 已回填。
5. 变更纪律：复盘产出的 PR 必须带 `Review-Ref: {yyyyQn}/{okr_id}` trailer，`opscheck okr --gates` 按 trailer 反查每个 PR 的 `regression` / `kb-regression` 运行记录，缺失即列出。
6. `cmd/opscheck gate4` 汇总证据生成验收报告附录：作物包清单与工时、季度内知识提交全部带 `Review-Id` 的扫描结果、`opscheck compliance`、`opscheck alerts --report` 与四份演练报告；第 3、6 条输出判定时间与负责人。

② SQL（无新表）

```sql
-- 比率类指标：季度内按日分子分母求和后再算比例（不对日比例取平均）
SELECT metric, slice_key, SUM(numerator) AS num, SUM(denominator) AS den
  FROM quality_metric_daily
 WHERE source='ONLINE' AND metric IN (?) AND stat_date BETWEEN ? AND ?
 GROUP BY metric, slice_key;

-- 成本组：季度合计按作物与能力（P95 不从日汇总合并，读 L4 cost-p95 季度结果）
SELECT crop, capability, SUM(cost_uyuan) AS cost, SUM(runs) AS runs,
       SUM(prompt_tokens) AS prompt_tokens, SUM(completion_tokens) AS completion_tokens
  FROM cost_daily WHERE stat_date BETWEEN ? AND ? GROUP BY crop, capability;

-- 季度 Top 清单来源：每周一快照上周行（不按状态过滤），季度结果对各周快照求和
SELECT id, reason, question_type, primary_kb_id, hit_count, status
  FROM attribution_issue WHERE last_week = ?;
```

③ Redis 键：无。

## 7. 推荐实施节奏

### 7.1 建设期（12 周）与季度常态

| 周次 | 主线 | 并行 |
|------|------|------|
| 第 1 周 | L0 基线冻结、开关治理、告警定级 | L5 合规清单起草；L4 成本口径与财务对齐 |
| 第 2-4 周 | L2 生命周期字段、状态机、工作台 v1 | L4 埋点维度补齐与看板；L5 数据分级与出域配置；L6 Runbook 编写 |
| 第 5-6 周 | L3 知识回归集与知识变更 CI；L2 关闭线下路径 | L1 首个作物重构为作物包；L6 对照校验 CI 与 on-call 排班 |
| 第 7-10 周 | L1 扩展小组按 SOP 接入新作物 1、新作物 2 | L4 预算告警与高成本归因；L5 隐私、保留期、取证、保险落地 |
| 第 11 周 | L6 四类故障演练（知识污染演练依赖 L3 已上线） | L1 SOP 修订、人日回填 |
| 第 12 周 | L7 首次季度复盘；Gate 4 建设类条目收口 | L4 首季度成本对账 |

季度常态（建设期后每季度固定执行）：

1. 季度第 1 个月：上季度复盘与 OKR 定稿；法规映射与合规清单复核。
2. 季度第 2 个月：作物包扩展（按 SOP）；知识到期复核集中处理。
3. 季度第 3 个月：四类故障演练；成本预测对账；`holdout` 与知识回归集刷新。

### 7.2 并行与合入规则

1. L2 先于 L3、L3 先于 L1 新作物上线：作物包必须经工作台评审与知识回归集才能 `active`，不允许先上线后补流程。
2. 首个作物重构为作物包的 PR 单独合入，合入前跑全量评测（全部套件含 `holdout`，n=5），与 `baseline_p3` 配对对比。
3. 扩展小组的 PR 只允许改动参数文件、知识文件、样本目录；触及 Go 代码的改动转给常态研发评审，并记为 SOP 缺口。
4. `guardrail`、`constraint_checker`、`pii`、审批闸门四处改动沿用双人评审（母文档 §8.3-4）；`croppacks/**`（calc 参数与作物级硬约束阈值）属红线资产，CODEOWNERS 强制规则 Owner + 农艺专家双签（母文档 §4.6.2-10）；法规映射规则变更另需法务签字。
5. 知识类 PR 由农艺专家签字后才可合入（母文档 §8.3-5），在本阶段由 L2 工作台的评审动作承载。
6. PR 标题带 `P4-Ln` 编号，一个 PR 只做一件事。

## 8. 降级与回滚

| 场景 | 触发条件 | 降级 / 回滚动作 | 影响范围 |
|------|----------|------------------|----------|
| 知识条目错误 | 巡检、归因、告警发现错误条目 | 切回上一对已评测的 `(knowledge_ref, collection)`（不重建索引，固定文件与 RAG 同时回退），再经 L2 工作台修正条目；按 L3 异步影响报告筛出受影响 `run_id` 并通知 | 引用或加载过该条目的建议 |
| 作物包整体问题 | 新作物红线失败、改单率异常；运行时交叉校验失败 | 作物包转 `retired`（运行时校验失败由加载器自动标为不可用并发 P0 告警，进程不退出），该作物问题统一走 `KB_MISS` 拒答 + 转人工；`crop_pack.<crop>` 按农场关闭 | 仅该作物，其他作物不受影响 |
| 作物包重构退化 | 首个作物重构后回归出现负向翻转 | 回退重构 PR，恢复原参数位置 | 首个作物 |
| 知识回归集误阻断 | 回归集样本或打分器错误导致误拦截 | 修样本或打分器后重跑，不允许跳过门禁（评测设计方案 §8.3-3） | 待生效的知识变更 |
| 工作台不可用 | 工作台服务故障 | 新增与修改暂停，已生效知识不受影响；应急通道经 Owner + 农艺负责人双签，**只允许切回上一对已评测的 `(knowledge_ref, collection)` 或失效条目**，不允许写入新内容，事后补录 `review_id` | 知识变更流程 |
| 预算超限 | 农场或能力维度成本超阈值 | 写独立覆盖开关 `budget_hold.<flag>` 按能力优先级降档（不改写人工开关值）：先放大 `WINDOW / ROUTINE` 级预警扫描间隔，再关闭 `enable_auto_plan`、`enable_vision_diagnose`，默认在预算的 1.0 / 1.1 / 1.2 倍依次触发（待 Owner 确认）；`EMERGENCY` 级防灾预警、建议主链路、红线校验、审批链路不降级 | 高级能力 |
| 法规变更未及映射 | 新法规生效而规则未更新 | 相关作业类型在审批前置条件中暂停开放，`enable_robot_dispatch` 按需按农场关闭 | 相关作业类型 |
| 出域配置错误 | 发现非白名单数据出域 | 立即回滚出域配置，按 L5 取证流程留证，法务介入 | 全局 |

## 9. 角色分工

1. **项目 Owner**：Gate 4 判定、开关治理结论、季度复盘主持、应急通道审批。
2. **架构负责人 / 常态研发**：作物包结构抽取、工作台后端、知识回归集与 CI、成本埋点、分支保护；不承接新作物内容。
3. **扩展小组**：按 SOP 接入新作物，记录 SOP 缺口与人日。
4. **知识工程 RD**：知识生命周期状态机、超期降权、RAG 索引同步、引用失效扫描。
5. **农艺专家组**：知识提报与评审、到期复核、作物包签署、复盘中的归因判定。
6. **前端**：专家审核工作台、成本看板、季度复盘看板。
7. **QA**：知识回归集维护、作物包验收、演练组织与报告。
8. **SRE**：Runbook、on-call、四类故障演练、告警对照校验。
9. **法务**：合规清单、法规映射更新、隐私与取证流程、保险方案。
10. **财务**：成本口径、预算设定、季度成本预测对账。

## 10. 阶段验收模板（每季度填写）

1. 路线完成情况：L0-L7 逐项勾选；常态类事项（作物扩展、到期复核、演练、对账、复盘）本季度是否按期执行。
2. 作物包台账：各作物包 `pack_version / status / 区域 / 灰度农场数`，本季度新增与下线；新作物接入人日实测与 SOP 修订项。
3. 评测与回归结果：`baseline_p3`（或最新冻结基线）vs 当前，全套件红线 pass^5、可信度六项、Phase 3 新增四项、翻转样本清单；本季度知识回归集运行次数与阻断次数。
4. 知识治理快照：本季度知识变更数、`REVIEW_DUE` 积压数、超期退出候选数、回滚次数。
5. 成本快照：三维度成本、预算告警次数、高成本归因分布、季度预测 vs 实际偏差。
6. 合规快照：合规清单变化项、法规映射更新次数、隐私删除与取证请求处理记录。
7. 运维快照：P0/P1 告警数与 Runbook 覆盖率、四类演练结果与改进项关闭情况、真实事故复盘。
8. 效果经营：Top 清单、OKR 完成情况与对应指标变化。
9. 遗留问题、负责人、关闭时间；红线相关问题不允许延期。
10. Gate 4 各条当前判定状态（通过 / 未到判定时点 / 未通过 + 整改项）。

## 11. 本阶段风险与应对

| 风险 | 影响 | 应对 |
|------|------|------|
| SOP 写成了文档，扩展小组仍需核心研发陪跑 | Gate 4 第 1 条不过，扩展成本无法下降 | L1 要求新增作物零 Go 代码改动；陪跑需求一律记为 SOP 缺口并修订 SOP，而不是由研发代做 |
| 工作台流程过重，专家绕道 | 线下路径死灰复燃，知识无署名 | 分支保护 + 提交校验做硬拦截；评审页直接给出影响面与回归结果，降低评审成本 |
| 知识回归集样本覆盖不足 | 知识变更「过了门禁仍坏建议」 | 依赖样本 + 分层抽样 + 红线子集三部分组成；线上回流持续补样本；知识污染演练检验发现路径 |
| 到期复核积压 | 大量条目降权或退出候选，拒答率上升 | 复核集中在季度第 2 个月处理；`REVIEW_DUE` 积压进季度复盘看板 |
| 成本预测偏差大 | 季度预算失真 | 首季度只校准口径；会话量与单会话成本分开建模；偏差归因进复盘 |
| 法规变化跟不上 | 合规作业风险 | 法规映射有复核周期与负责人；未及映射时相关作业类型暂停开放（§8） |
| 演练流于形式 | 真实故障时 Runbook 不可用 | 演练报告必须记录偏离 Runbook 的步骤；改进项在下季度演练前关闭 |
| 治理工作挤占常态研发 | 底座维护与缺陷修复变慢 | §9 明确常态研发不承接新作物内容；治理需求进季度 OKR 统一排期 |

## 12. 常态运营交接

建设期结束、Gate 4 建设类条目通过后，以下事项从项目制转为常态运营：

1. 作物扩展：扩展小组按 SOP 持续接入，作物包台账由 QA 维护。
2. 知识治理：农艺专家组在工作台处理提报、评审、到期复核；知识工程 RD 维护工作台与回归集。
3. 成本：财务按季度设定预算与对账，SRE 维护看板与告警。
4. 合规：法务按复核周期更新合规清单与法规映射。
5. 运维：SRE 负责 on-call 与季度演练，Runbook 随告警项变化同步更新。
6. 效果经营：Owner 主持季度复盘，结论进入下季度 OKR。

交接物：《Phase 4 建设期验收报告》、Runbook 全集、《新增作物接入 SOP》最新版、合规清单、成本口径说明、复盘纪要模板。

## 13. 维护规则

1. 本文随母文档 §5.4 变更同步更新；冲突时以母文档为准并回改本文。
2. 新增治理事项先补进 §3.1 与对应路线，再排期，不在路线外临时开工。
3. SOP、Runbook、合规清单的修订都要有版本号与修订原因，旧版本保留。
4. 红线相关规则（引用、数值、药剂候选、审批闸门、硬约束、脱敏）在本阶段只允许收紧，不允许放松；任何放松提案一律否决（母文档 §11-6）。
5. 基线更替：只有在全量评测（含 `holdout`，n=5）配对对比无显著退化时，才允许冻结新基线替代 `baseline_p3`，旧基线保留可查。
