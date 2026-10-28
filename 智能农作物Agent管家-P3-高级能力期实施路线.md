# 智能农作物 Agent 管家 Phase 3（P3）高级能力期 实施路线（14 周）

> 文档类型：阶段详细实施路线（母路线图 §5.3 的展开）
> 上游文档：《分阶段实施路线图》§5.3、§4.6、§4.7；《评测 Harness 设计方案》§5、§10、§15；《P2 质量优化期实施路线》
> 下游文档：《P4 企业治理与规模化期实施路线》
> 读者：架构负责人、知识与检索小组、多模态小组、编排与调度小组、平台与可观测、农艺专家组、QA / SRE
> 状态：草案，Gate 2 通过后评审定稿

---

## 1. 文档定位

1. 本文是 Phase 3 的执行手册：把母文档 §5.3 的 L0-L8 拆到「模块 → 任务 → 交付物 → 验收」粒度，团队按本文排期与提 PR，PR 标题带 `P3-Ln` 编号。
2. 本阶段做五件 MVP 刻意回避的事：RAG、多模态诊断、主动预警、多机协同排程、一周农事计划。每项能力独立开关、独立评测、独立回滚。
3. 与母文档的关系：本文只细化，不改动母文档的范围、Gate 与红线。两者冲突时以母文档为准，并回改本文。

---

## 2. 阶段基线与输入

### 2.1 来自 Phase 2 的基线（进入 P3 的前提）

1. Gate 2 八条全部通过，`make eval` 门禁在 PR 与夜间生效，至少拦下过 1 次真实回退。
2. 六个可信度指标（引用覆盖率、事实一致率、数值准确率、幻觉率、拒答正确率、过度保守率）进日报，引用覆盖率与数值准确率保持 100%。
3. 评测集 ≥ 500 条，`regression / capability / redteam / holdout` 四套件齐备，三个 LLM-judge 维度校准达标并进门禁。
4. 规则与模型职责再切分完成，模型只负责「情境理解 + 方案选择 + 解释生成」。
5. 长对话压缩 `compaction` 已上线，成本与延迟门禁生效。

### 2.2 沿用的契约、开关与指标（不得改名）

1. 契约：`AdviceCard / WorkOrder / Citation / RunState / ApprovalTicket`，变更走版本号与 Owner 批准（母文档 L1 验收 3）。
2. 状态机：`DRAFT → PENDING_APPROVAL → APPROVED → DISPATCHED → EXECUTING → DONE`，异常分支 `REJECTED / EXPIRED / FAILED / CANCELED / PARTIALLY_DONE`，复审迁移 `APPROVED → PENDING_APPROVAL`。
3. 已有开关：`enable_advice`、`enable_robot_dispatch`；本阶段新增五个能力开关：`enable_rag / enable_vision_diagnose / enable_proactive_alert / enable_multi_device_schedule / enable_auto_plan`，另加 L5 多方案子开关 `enable_alternatives`，与五个能力开关同等纳入灰度、回滚演练与逐项关闭回归（§8、Gate 3 第 6 条）。
4. 红线组件：`calc`、`output_validator`、`constraint_checker`、`pii_masking`、审批闸门、`tool_policy.yaml`，本阶段新增的 `conflict_checker` 同等对待；只允许新增校验，不允许放宽。红线组件只按 `kb_id@version` 精确读取知识，不走检索（母文档 §4.6.2-10）。
5. 评测注入接缝：`Clock`、`ToolProvider`、`DeviceAdapter`、存储、`KnowledgeSource`、`MemoryStore`、`UserChannel`、`Approver`；本阶段 RAG 检索与固定文件加载共用 `KnowledgeSource`，该接缝只服务主链路的知识注入，红线组件不经过它。
6. 公共约定沿用母文档 §4.6.1 与 §4.6.2（鉴权路径、下发路径与顺序、outbox 去重键、开关取值、`ticket_kind`、事件类型、对象 key、时区、红线资产），本文只写 P3 的增量；本文 Redis 键名省略公共前缀 `as:{env}:`。

---

## 3. 范围边界

### 3.1 本阶段必须完成

1. 知识层从固定文件升级为 RAG，`enable_rag=false` 时回落固定文件 + 渐进式加载，回落路径保留到 Phase 4 结束。
2. 多模态病虫害诊断：手机拍照 + `M 系列` 巡田影像 + `FC5` 定点相机，带置信度阈值与专家兜底。
3. 主动预警与排程：按日期 / 节气 / 物候进度 / 气象预警主动推送，带分级、频控、配额、去重与静默时段。
4. 多设备协同排程与作业窗口滚动重排。
5. 一周农事计划（自动规划）小流量灰度，计划内每张 `WorkOrder` 执行前仍需确认。
6. 质量出口加严：数值二次校验、方案冲突检测、多方案对比、解释增强。
7. `run` 回放调试视图与回归门禁扩表（图像子集、预警子集）。
8. 五个能力开关与 `enable_alternatives` 默认全关，逐项灰度，每项有独立回滚演练记录。

### 3.2 本阶段明确不做

1. 不做全自主执行：红线二不变，动设备仍必须人审；`actuate` 工具仍不注册给模型。
2. 不做模型微调与自建推理集群（除 D1 另有结论）。
3. 不做跨区域大规模放量，不做多作物扩展工厂化（留 Phase 4）。
4. 不做产量预测与经营决策类高不确定性能力。
5. 不做知识生命周期线上化与专家审核工作台完整版（P3 只做诊断转专家的最小队列，完整工作台留 Phase 4）。
6. 不放宽任何 Phase 0-2 的校验规则，不下调任何红线门禁。

---

## 4. 目标与通过标准（Gate 3）

| # | 通过标准 | 判定方式 |
|---|----------|----------|
| 1 | RAG 路径在同一评测集上可信度指标**不劣于**固定文件路径；知识规模扩大 ≥ 5 倍后检索精度不下降 | 两个单因子实验：①原规模知识下 `baseline_p2`（固定文件）vs RAG，同一 `knowledge_ref` 配对对比 + 置信区间；②RAG 在原规模 vs 5 倍规模，比较 `retrieval_precision`（定义见 L1 第 12 条）与加载精度（P2 `kb_load_item` 口径） |
| 2 | 多模态诊断在标注图集上达到约定准确率；低置信度样本 100% 转专家；**不出现高置信度误诊导致的作业单** | `evalkit` 图像子集；准确率目标线 Gate 2 后约定；线上以 `CONFIDENT` 诊断的抽样专家复核（L2 第 10 条）为证据，复核判误且已生成工单的计数须为 0 |
| 3 | 主动预警有效率达标，用户关闭率低于阈值，无一次因刷屏导致投诉 | 线上指标看板；阈值在预警灰度首档（第 10 周）观察窗实测后确定并回填 |
| 4 | 多机协同排程在示范农场实测缩短总作业时长，无设备冲突与航线重叠 | 冲突与重叠由 `evalkit` 排程子集判定；作业时长实测**由田间验证方执行，本团队提供功能与数据支撑** |
| 5 | 自动规划灰度期间**零自动执行**；计划采纳率与人工改单率有数据 | `stop_at_approval` 扩展到计划级 + 审计日志抽查 |
| 6 | 每项能力可单独关闭，关闭后系统退回 Phase 2 形态且指标不劣化 | 五个能力开关与 `enable_alternatives` 逐项关闭后跑全量 regression，与 `baseline_p2` 配对对比 |

补充硬条件（随 Gate 一并判定）：

1. 所有红线类样本（含新增图像、预警、计划子集）pass^5 = 1。
2. `citation_valid` 与 `numbers_match_calc` 通过率保持 100%。
3. 单会话 token 成本与 P95 不劣于 `baseline_p2` 超阈值；超阈值需专项说明并经 Owner 签字。

Gate 3 指标口径（`quality_metric_daily` 与验收报告同口径，dry-run 单不计入）：

1. `retrieval_precision`：RAG 注入的 top-k 片段中，属于样本 `must_cite` 或专家标注相关条目的比例（按 `kb_id@version` 计）。
2. `diagnosis_accuracy`：图像子集中 `top_pest_id` 与专家标注一致的比例；线上抽检复核同口径。
3. `alert_effective_rate`：`feedback='USEFUL'` 或已转出 `AdviceCard`（`advice_id` 非空）的预警 / 已推送（`PUSHED`）预警。
4. `alert_close_rate`：`feedback='MUTED'`（用户关闭该类预警）的预警 / 已推送预警。
5. `plan_adoption_rate`：计划内作业项中逐单确认通过的工单数 / 计划作业项数（不含 `SKIPPED`）。
6. `plan_edit_rate`：计划内工单中被驳回后重提、或经人工修改时间窗 / 设备而换票的工单数 / 计划内工单数。

---

## 5. 实现路线总览（L0 → L8）

1. `L0`：基线冻结、开关与评测集扩展
2. `L1`：知识底座升级（RAG）
3. `L2`：多模态诊断链路
4. `L3`：主动预警与排程调度
5. `L4`：风险控制与降级策略
6. `L5`：质量校验与结果增强
7. `L6`：自动规划灰度（多步作业计划）
8. `L7`：调试视图、可观测性与回归门禁
9. `L8`：灰度发布、回滚预案与验收收口

建议顺序：`L0 → L1 + L2 → L3 + L4 + L5 → L6 + L7 → L8`

```
            ┌──── L1 RAG ─────┐
L0 基线 ────┤                 ├──► L3 主动预警/排程 ──┐
冻结与开关  └──── L2 多模态 ──┘         │             ├──► L6 自动规划 ──┐
                 │                     ▼             │                  │
                 │              L4 风险控制与降级 ◄───┤（与各能力同 PR）  ├──► L8 灰度
                 │                     │             │                  │    与收口
                 └──────────────► L5 质量校验 ────────┘                  │
                                                     L7 调试与门禁 ──────┘
L8 灰度与开发重叠：RAG（第 6 周）→ 视觉（第 7 周）→ 多方案（第 8 周）→ 预警（第 10 周）→ 排程（第 11 周）→ 规划（第 12-13 周）
```

依赖说明：

1. L1 / L2 只依赖 L0，两组并行，共用评测底座。
2. L3 的预警转 `AdviceCard` 依赖 L1（知识检索）；预警中的病虫害线索可来自 L2。
3. L4 不是独立能力，而是 L1-L3、L5、L6 的刹车，**降级路径必须与对应能力同 PR 提交**，周次跟随对应能力。
4. L5 的数值二次校验依赖 L1（RAG 命中的公式），方案冲突检测是 L6 的前置条件。
5. 每项能力的事件类型登记、关键指标、回滚告警和红线样本进 PR 冒烟，都随该能力同 PR 交付，是它进灰度的前置条件；L7 只做统一回放视图、检索可视化与指标对账，回归门禁扩表依赖 L0 的新增子集。

---

## 6. 路线详细拆解

### 6.1 L0 基线冻结、开关与评测集扩展（第 1 周，标注延续至第 4 周）

**目标**：先把 Phase 2 的成绩钉成不可回退的基线，再动任何策略。

**涉及模块**：`internal/app`（开关装配）、`internal/service/flag`（`feature_flag` 表）、`eval/baselines/`、`eval/suites/`、`internal/evalkit/sample`、`internal/evalkit/env`、`internal/evalkit/grader`。

**功能任务**

1. 以 Gate 2 最后一次发版评测为准，冻结指标快照 `baseline_p2`：实验记录 ID 写入 `eval/baselines/baseline_p2`，附配置快照（`git_sha / prompt_version / model_tier / loader_strategy / knowledge_ref / judge_versions`）。
2. `baseline_p2` 设为只读：`eval/baselines/` 目录变更需 Owner 审批（CODEOWNERS），实验存储中该记录禁止覆盖写。
3. 新增五个能力开关 `enable_rag / enable_vision_diagnose / enable_proactive_alert / enable_multi_device_schedule / enable_auto_plan` 与 L5 子开关 `enable_alternatives`，默认全关，按农场维度生效，沿用 Phase 1 `enable_robot_dispatch` 的开关框架；开关读取统一经 `internal/app` 注入，业务包不直接读 `feature_flag`。
4. 开关依赖在读取时求值：`effective(enable_auto_plan) = enable_auto_plan && effective(enable_multi_device_schedule)`。写入时只拒绝「打开 `enable_auto_plan` 而排程未生效」这一种情况；关闭方向（含 `force_off`）永远放行。库中出现非法组合时，只把该农场的自动规划按关闭处理并告警，不让进程退出。
5. 评测集扩到 1000+ 条：新增图像子集（`category=诊断`）、主动预警场景子集、排程子集、计划子集；「应拒答」负样本占比保持 ≥ 20%。
6. 样本格式扩展：`input.images[]`（图片路径 + 采集来源 `phone / M500 / M2000 / FC5`）；`fixtures.clock_advance[]`（`FrozenClock.Advance()` 的推进序列，用于触发预警扫描）；`expect.alert`（是否推送、级别、去重键）。
7. `cmd/eval validate` 补充静态校验：图片文件存在且已脱敏（无 EXIF 定位）；预警样本必须声明 `clock_advance`；新增子集的 `must_cite` 在 `knowledge_ref` 中存在。
8. 农艺专家组启动图集标注与新增样本签署（并行至第 4 周）。

**交付物**：`baseline_p2` 快照；六个开关及读取时依赖求值；扩展后的样本 schema 与静态校验；新增子集首批样本。

**验收**

1. 所有新开关默认关闭时，全量 regression 与 `baseline_p2` 配对对比无翻转样本（回归全绿）。
2. `baseline_p2` 快照入库，修改尝试被拒绝并留记录。
3. 关闭 `enable_multi_device_schedule` 后，`enable_auto_plan` 的生效值随之为关；打开 `enable_auto_plan` 而排程未生效时写入被拒（`400`）；库中存在非法组合时服务正常启动并告警。均有单测覆盖。
4. 新增子集样本全部通过 `cmd/eval validate`，且对空实现可生成报告。

**技术实现**

流程

1. `cmd/eval baseline freeze --name baseline_p2 --exp {id}`：写 `eval/baselines/baseline_p2.yaml`（实验 ID + 配置指纹），评测库 `experiment` 行置 `frozen=1`，`evalkit` 存储层拒绝更新冻结行。
2. 开关注册：在 `configs/flags_registry.yaml` 声明 `enable_rag / enable_vision_diagnose / enable_proactive_alert / enable_multi_device_schedule / enable_auto_plan / enable_alternatives`（L5 多方案），默认全关，取值形态按母文档 §4.6.2-5；`service/flag` 启动时加载，`key` 支持前缀通配（P4 登记 `crop_pack.*`、`budget_hold.*`），未匹配任何条目的 `flag_key` 读写报错。依赖表 `enable_auto_plan → enable_multi_device_schedule` 写在同一文件，在 `service/flag` 读取合并时求值。注册时为每个开关预置 GLOBAL 行 `{"force_off": false}`，供 L8 关停时按 `row_ver` 更新。
3. 写入校验：`PUT /api/v1/admin/flags/{flag_key}` 只在「FARM 行写 `{"on":true}` 打开 `enable_auto_plan`」时检查同农场排程生效值，未生效返回 `400` 不落库；其余写入（关闭、`force_off`）不做依赖检查。启动加载时发现非法组合只记告警 `flag_dependency_violation{farm_id}`，读取时按关闭处理。
4. 读取：run 开始时 `service/flag` 生成快照，摘要写 `agent_run.flags_digest`，生效为开的开关名写 `agent_run.flags_on`（A/B 分组与回放用），经 `ctx` 传递，resume 沿用；worker 扫描任务每轮按农场取一次快照。
5. 样本扩展：`evalkit/sample` 新增 `images[] / clock_advance[] / alert_expect`，旧样本零值可解析；`cmd/eval validate` 增加 EXIF GPS / 序列号检查。

SQL

```sql
-- 沿用 P1 feature_flag，不加表；打开 enable_auto_plan 前取同农场排程开关三层取值，代码合并求生效值
SELECT flag_key, scope_type, value FROM feature_flag
 WHERE flag_key='enable_multi_device_schedule'
   AND ((tenant_id=0 AND farm_id=0) OR (tenant_id=? AND farm_id IN (0, ?)));
-- 基线冻结（评测库 agri_eval）
UPDATE experiment SET frozen=1 WHERE id=? AND frozen=0;   -- 沿用 P2
-- run 记录开关快照，回放与 A/B 统计用
ALTER TABLE agent_run ADD COLUMN flags_digest char(64) NULL,
  ADD COLUMN flags_on json NULL,          -- 生效为开的开关名数组，如 ["enable_rag","enable_alternatives"]
  ALGORITHM=INSTANT;
```

Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `flags:changed` | —（pub/sub，沿用 P1） | 新开关变更广播，各进程清本地缓存 |

### 6.2 L1 知识底座升级（RAG）（第 2-5 周）

**目标**：让知识从「几十个文件」扩到「几千条条目」仍能精准命中，且随时能退回固定文件路径。

**涉及模块**：`internal/agent/knowledge`（新增 `retriever/`、`chunker/`、`index/` 子包，与 `progressive_loader` 并列实现 `KnowledgeSource`）、`knowledge/`（front-matter 不变）、开关 `enable_rag`、指标 `retrieval_precision / retrieval_recall / citation_hit_rate`。

**功能任务**

1. 定义检索接口：沿用 `internal/agent/knowledge` 的 `Retriever.Retrieve(ctx, q)`（`q` 含问题与情境），签名保持不变，`progressive_loader` 与 `rag_retriever` 各为一个实现；`internal/app` 按 `enable_rag` 选择注入，主链路代码不感知差异。
2. 切片策略 `chunker`：按语义段切片，每片继承原条目 front-matter（`kb_id / crop / stage / region / category / authority_level / version / author / effective_from`）；一片只属于一个 `kb_id@version`，保证 `Citation` 可回指。
3. embedding 与向量索引：embedding 模型档位走 D1 结论；查询侧用哪个 embedding 模型由 ACTIVE 索引登记的 `embed_model` 决定，不读独立配置；索引按 `knowledge_ref` 构建，一个知识版本一个索引快照，可按提交号重建。
4. 检索融合：先元数据硬过滤（作物 / 生育期 / 区域 / 生效时间 `effective_from` 与版本有效性），再向量召回，过滤条件不满足的片段不得进入候选；`common/` 下不属于具体作物或区域的条目（安全、作业窗口阈值等）始终保留在过滤结果内。
5. 权威等级优先排序：`国标 > 地标 > 企业标准 > 专家经验`；同一结论有多级来源时，高等级排前，排序规则可配置、可 A/B。
6. 注入上限与决策表：沿用渐进式加载「单次注入 ≤ 5 条」的上限，检索结果截断后再注入。检索结果按以下顺序处理（与母文档 §5.3.8 一致）：①索引异常（超时、报错、版本或维度不一致）→ 回落固定文件加载，标 `degraded`；②召回为空或最佳相关度低于阈值 → 回落固定文件加载，标 `degraded`；③固定文件也未命中 → `KB_MISS` → 拒答。
7. 版本固定：run 开始时读一次 ACTIVE 索引登记，把索引 id（含集合名与 `knowledge_ref`）写入 `agent_run` 与 `RunState`，HITL resume 沿用，不随中途激活的新索引切换；固定文件回落也用同一个 `knowledge_ref`。
8. 红线隔离：`calc`、`guardrail`、`constraint_checker`、`conflict_checker` 读阈值与公式只按 `kb_id@version` 精确读取（母文档 §4.6.2-10），与 `enable_rag` 无关，永远不经过 `Retriever`。
9. 知识规模扩充演练：用专家已签署条目 + 极飞学园内容构造 ≥ 5 倍规模的知识集（新增条目同样需署名与 `authority_level`），用于 Gate 3 第 1 条的第二个实验。
10. `enable_rag=false` 无缝回落：同一请求切换开关后走固定文件加载，不需重启。
11. 紧急下线：检索后置拒绝名单 `policy/kb_denylist.yaml`（`kb_id@version` 列表，热加载），命中片段在注入前剔除，固定文件路径同样生效；用于不等重建索引就撤下错误条目。
12. **评测与指标**（随本能力同 PR 交付）：实现 `retrieval_recall`（期望 `must_cite` 是否被召回）、`retrieval_precision`（注入的 top-k 中属于 `must_cite` 或专家标注相关条目的比例）与 `citation_hit_rate`（召回条目中被最终引用的比例）打分器；RAG 路径同样写 P2 的 `kb_load_item(strategy='rag')`，加载精度与无关知识注入率沿用 P2 口径；`KnowledgeSource` 评测实现支持按 `knowledge_ref` 加载对应索引快照；`RETRIEVAL_HIT / RETRIEVAL_FALLBACK` 事件、回落率指标与「RAG 回落率突增」告警在灰度前上线。

**交付物**：`rag_retriever` 实现；索引构建脚本（`make kb-index KB_REF=<sha>`）；5 倍规模知识集；三个检索打分器及表驱动单测；检索事件、指标与告警。

**验收**

1. 原规模知识下，同评测集上 RAG 路径六个可信度指标不劣于固定文件路径（配对差值置信区间不跨负向阈值）。
2. 知识规模扩 5 倍后，`retrieval_precision` 与加载精度不低于原规模 RAG。
3. 元数据过滤用例：跨作物、过期版本、未生效条目 100% 不被召回；`common/` 条目在任意作物与区域下都可召回。
4. 一键回滚固定文件路径演练通过，回滚后 regression 全绿。
5. 召回为空、相关度过低、索引异常三类用例都回落固定文件并标 `degraded`；固定文件也未命中时走 `KB_MISS` → 拒答，`kb_miss_refuse` 通过。
6. 运行中激活新索引，进行中的 run（含 HITL 中断后 resume）仍用 run 开始时固定的集合；红线组件在 `enable_rag` 开 / 关两种状态下读到的阈值一致。

**技术实现**

流程

1. 本地部署：`deploy/docker-compose.yml` 增加 `etcd` 与 `milvus`（`milvusdb/milvus:v2.5.x` standalone，镜像固定到补丁版本，用原生 BM25 与中文分词），对象存储复用已有 MinIO（bucket `agri-steward-milvus`）；DashVector 只作为 `agent/knowledge/index` 接口的可替换云端实现，不在本阶段部署。
2. 建索引：`cmd/migrate kb-reindex --ref <knowledge_ref> --embed <model>` → `chunker`（按标题 / 段落切 300-500 字，表格与公式整块）→ embedding → 写 collection `kb_{env}_{ref8}_{chunker_ver}_{embed_short}_{dim}`（换模型即换集合，不会覆盖 ACTIVE 集合）→ `kb_index_manifest` 置 `READY`。
3. 激活：评测过门禁后，先校验 `manifest.knowledge_ref` 等于当前部署的知识 ref，再在 MySQL 同事务把旧 ACTIVE 置 `RETIRED`、新行置 `ACTIVE`（`uk_one_active` 兜底）；已评测过的 `RETIRED` 行允许重新激活，用于紧急回滚。不使用 Milvus 别名，运行时只认 manifest 登记的集合名；`RETIRED` 集合保留 ≥ 审批最长时效 + 1 天、且至少保留最近 2 个，再删，保证中断中的 run 能 resume、回滚不需重建。
4. run 开始：`agent/knowledge/rag` 取当前活动的 `(knowledge_ref, collection)` 一对（P3 读 ACTIVE manifest，P4 改为读 `current.json` 指针，调用方不变）→ 校验 `knowledge_ref` 与本实例部署版本一致、集合 schema 的维度等于 `embed_dim`（进程内按 manifest id 缓存）→ 写 `agent_run.kb_manifest_id` 与 `knowledge_ref`，这一对在 run 内固定；不一致按决策表①回落，`degrade_reason=RAG_FALLBACK`。resume 读 `agent_run.kb_manifest_id`，不再取活动指针。
5. 检索：用 manifest 的 `embed_model` 编码查询 → 元数据硬过滤 → 稠密 + 稀疏（Milvus BM25 函数）各取 30 → RRF 融合 → 按 `authority_level` 加权 → 同条目聚合 → 剔除 `kb_denylist` → 截断 5 条。过滤表达式：`tenant_id in [0, {tenant_id}] and (crop == "{crop}" or crop == "common") and (ARRAY_CONTAINS(region_codes, "{region}") or region_scope == "ALL") and (ARRAY_CONTAINS(stages, "{stage}") or ARRAY_LENGTH(stages) == 0) and effective_from <= {now} and (effective_to == 0 or effective_to > {now})`，参数由代码绑定，不拼接用户输入。
6. 决策表：异常 → 回落；全部结果最佳稠密分 < `rag.min_dense_score` 或召回为空 → 回落；回落后固定文件未命中 → `KB_MISS` 拒答。回落写 `RETRIEVAL_FALLBACK(reason=ERROR|EMPTY|LOW_SCORE|REF_MISMATCH)`，命中写 `RETRIEVAL_HIT`，并按 P2 口径写 `kb_load_item(strategy='rag')`。
7. 取正文：注入模型的正文按 `kb_id@version` 回查知识仓库并校验 `text_sha`，不一致丢弃该片；Milvus 中的 `text` 字段只供 BM25 使用。红线组件不走以上流程，直接按 `kb_id@version` 读 `knowledge/` 文件。

SQL

```sql
CREATE TABLE kb_index_manifest (       -- 平台级表（母文档 §4.6.1 表设计约定 3），无租户列；向量库丢失可按此重建
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  knowledge_ref varchar(40) NOT NULL, collection varchar(128) NOT NULL,
  embed_model varchar(64) NOT NULL, embed_dim smallint NOT NULL,
  chunker_ver varchar(16) NOT NULL, tokenizer_ver varchar(16) NOT NULL,   -- tokenizer_ver 对应 Milvus analyzer 配置版本
  chunk_count int NOT NULL,
  status varchar(16) NOT NULL,           -- BUILDING / READY / ACTIVE / RETIRED / FAILED
  active_flag tinyint GENERATED ALWAYS AS (IF(status='ACTIVE', 1, NULL)) STORED,
  eval_experiment_id bigint unsigned NULL, activated_by bigint unsigned NULL, activated_at datetime(3) NULL,
  row_ver int NOT NULL DEFAULT 0, created_at datetime(3) NOT NULL, updated_at datetime(3) NOT NULL,
  PRIMARY KEY (id), UNIQUE KEY uk_build (knowledge_ref, chunker_ver, embed_model), UNIQUE KEY uk_collection (collection),
  UNIQUE KEY uk_one_active (active_flag), KEY idx_status (status, activated_at));
-- 激活：同一事务内旧 ACTIVE → RETIRED，新行 READY（或已评测过的 RETIRED，紧急回滚）→ ACTIVE（乐观锁 + uk_one_active 兜底）
UPDATE kb_index_manifest SET status='RETIRED', row_ver=row_ver+1, updated_at=? WHERE status='ACTIVE';
UPDATE kb_index_manifest SET status='ACTIVE', activated_by=?, activated_at=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND status IN ('READY','RETIRED') AND eval_experiment_id IS NOT NULL AND knowledge_ref=? AND row_ver=?;
-- run 开始固定索引；resume 只读 agent_run.kb_manifest_id
SELECT id, collection, knowledge_ref, embed_model, embed_dim FROM kb_index_manifest WHERE active_flag=1;
ALTER TABLE agent_run ADD COLUMN kb_manifest_id bigint unsigned NULL, ALGORITHM=INSTANT;   -- NULL 表示走固定文件
```

Milvus collection 字段：`chunk_id`（VARCHAR 主键，由 `kb_id@version#seq` 哈希得出）、`tenant_id`（INT64，partition key，平台知识为 0，给 P4 租户知识预留）、`kb_id / kb_version / seq / char_start / char_end / text_sha`、`crop / category / region_scope`（INVERTED，`crop` 取 `common` 表示通用）、`stages / region_codes`（ARRAY）、`authority_level`、`effective_from / effective_to`（`0` 表示无截止）、`text`（VARCHAR，`enable_analyzer=true`，中文 analyzer）、`dense`（HNSW，COSINE）、`sparse`（由 BM25 函数从 `text` 生成，SPARSE_INVERTED_INDEX，metric `BM25`）。

Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `cache:emb:{model}:{sha256(text)}` | 24h | 查询 embedding 缓存，`model` 取 manifest 的 `embed_model`，丢失则重新调用 |

### 6.3 L2 多模态诊断链路（第 2-5 周）

**目标**：农户拍一张叶片照片，或巡田无人机一次飞行影像，能给出病虫害候选与依据；不确定时转专家，不硬判。

**涉及模块**：`internal/service/vision`（预处理、质量校验、诊断编排）、`internal/agent/llm`（新增视觉档位配置）、`internal/agent/tools/mcp`（新增影像类工具 `imagery.field_scan` / `imagery.camera_frame`）、`internal/agent/guardrail/pii`（图像脱敏）、专家转办队列（最小实现，挂在 `internal/service/approval` 旁的 `expert_queue`）、开关 `enable_vision_diagnose`。

**功能任务**

1. 视觉模型接入：通过 `internal/agent/llm` 新增视觉档位（`Qwen3-VL` 系 / `Qwen3.6-Plus` 原生视觉），档位经图像子集评测确定，写入配置可一键切换。
2. 图像预处理：尺寸归一、格式统一、多图合并上限；原图存 MinIO（`raw_object` 登记），模型入参与日志只引用图片 ID。
3. 质量校验前置：模糊、过曝、主体缺失（非作物叶片 / 植株）直接退回重拍，返回可读文案，不进入诊断。
4. 诊断输出契约：新增 `Diagnosis` 结构（`candidates[]{pest_id, confidence, features[]}, citations[], image_refs[]`），作为 `AdviceCard.reasons[]` 的输入，不改动 `AdviceCard` 已冻结字段；需要新增字段时走契约版本号。
5. 诊断必须挂引用：每个候选绑定 `knowledge/<crop>/pest/<pest_name>.md` 中的识别特征条目；无引用的候选在 `output_validator` 处被拒收。
6. 封闭集合：候选的 `pest_id` 必须属于 `mcp-crop` 返回的该作物病虫害谱，集合外候选在 `output_validator` 处拒收（与 `product_in_candidates` 同等级），整次诊断转专家流程，不进主链路。
7. 置信度分流：置信度低于阈值（图像子集实测确定）、Top-2 候选差距过小或 Top-1 落在集合外时，转 `expert_queue`，用户侧显示「已转专家」；该路径不生成 `WorkOrder` 草稿。
8. 专家结论回流：专家单 `RESOLVED` 后，以 `source=EXPERT`、专家给出的 `pest_id` 为上下文重新走一次主链路（`calc`、引用校验、审批全部照常），不直接写工单；SLA 超时只通知专家组负责人并提示用户仍在处理，不自动出建议。
9. 影像接入：`M 系列` 巡田数据与 `FC5` 定点相机经 MCP 工具获取，返回带 `source / collected_at / data_freshness`，作为 measured 类 `Citation`；MCP 影像与手机上传走同一条 `service/media` 清洗流程，分辨率上限按来源分别配置。
10. 线上复核：诊断来源的工单除 `risk_level=HIGH` 外，`required_role` 必须包含 `agronomist`；`CONFIDENT` 诊断按配置比例抽样转专家复核（不阻塞主链路），复核结论作为 Gate 3 第 2 条的线上证据。
11. 图像脱敏：去除 EXIF 定位与设备序列号；巡田影像的坐标降精度为地块级标识，与 L5 脱敏规则对齐。
12. **评测与指标**（随本能力同 PR 交付）：图像子集 fixture 支持图片输入；新增 `diagnosis_accuracy`（与专家标注比对）与 `low_conf_to_expert`（低置信度或集合外必须转专家，红线）打分器，红线样本进 PR 冒烟；`pii_clean` 扩展到图像元数据；`VISION_RESULT / EXPERT_CASE_OPENED` 事件、视觉置信度分布与「低置信度占比突增」「图像脱敏漏出」告警在灰度前上线。

**交付物**：`internal/service/vision` 包；视觉档位配置；`Diagnosis` 结构与 schema；`expert_queue` 最小实现；图像子集打分器。

**验收**

1. 标注图集准确率达到 Gate 2 后约定的目标线。
2. 低置信度样本 100% 转专家，且未生成任何 `WorkOrder` 草稿（`low_conf_to_expert` pass^5 = 1）。
3. **诊断结论必须携带知识条目引用**：人为注入一条无引用诊断，被校验层拒收并落 `VALIDATION_FAILED` 事件；注入一条病虫害谱外的 `pest_id`，被拒收并转专家。
4. 质量不合格图片 100% 退回重拍，不进入模型诊断。
5. 图像与坐标数据在模型入参、日志、审计表、导出文件中无明文定位（`pii_clean` 通过）。
6. 媒体上传越权用例（用别的用户或农场的 `upload_id` 提交）、超限用例（超大小、错类型、超像素）100% 被拒。

**技术实现**

流程

1. 上传：`POST /api/v1/farms/{farm_id}/media`（单次 ≤ 4 张）→ 生成随机 `upload_id`，`SET media:upload:{upload_id} {tenant_id, farm_id, user_id, key, content_type} EX 600` → 返回 MinIO PostPolicy（5 分钟有效，条件含 `key` 固定、`content-length-range` 1B-10MB、`Content-Type` ∈ jpeg / png / webp），key 为 `agri-steward-raw` 下 `incoming/{tenant_id}/{farm_id}/{upload_id}`；`incoming/` 前缀配置 MinIO 生命周期 1 天过期，不 commit 的对象自动清理。
2. 确认：`POST /api/v1/farms/{farm_id}/media/{upload_id}/commit` → `service/media` 读 `media:upload:{upload_id}`，键不存在返回 `410`（上传失效），`tenant_id / farm_id / user_id` 与 `Scope` 不一致返回 `404`（不暴露存在性）→ `StatObject` 复核大小，读文件头校验魔数与像素尺寸（`media.max_pixels`，超限拒收，不解码）→ 解码重编码（丢 EXIF / XMP / 序列号）→ 按来源缩放（`media.max_edge.{phone,m_series,fc5}`）→ `RawStore.Put(kind=IMAGE)`（key 按母文档 §4.6.2-8）并登记 `raw_object` → 删 incoming 对象与 Redis 键；重复 commit 按 `raw_object.uk_key` 返回已有 `raw_id`；模型与日志只见 `raw_id`。MCP 影像（`imagery.*`）取回后直接从「读头部」一步进入同一流程。
3. 诊断：图节点 `vision_diagnose` → `service/vision/quality`（模糊、曝光、主体检测）不合格 → `RETAKE`；合格 → `agent/llm` 视觉档位输出候选 → `calibrate` 校准分数 → 按 `mcp-crop` 病虫害谱过滤，集合外候选剔除并记 `out_of_spectrum`。
4. 分流：`top_pest_id` 在谱内、`top_score ≥ 阈值` 且 `margin ≥ δ` → `CONFIDENT`，进主链路出 `AdviceCard`（诊断触发的工单 `risk_level` 强制 `HIGH`，`required_role` 并入 `agronomist`）；否则 `LOW_CONF` → 建 `expert_case(case_kind=LOW_CONF|OUT_OF_SPECTRUM)`，图结构上不经过 `to_order`。`CONFIDENT` 按 `vision.audit_sample_rate` 抽样建 `expert_case(case_kind=SAMPLE_AUDIT)`，不阻塞主链路。
5. 专家结论：专家 `RESOLVED`（`resolution` 含 `pest_id`、判别依据、引用条目）→ 同事务置 `diagnosis.status=EXPERT_RESOLVED` 并写 `outbox(topic=expert.resolved)` → worker 以 `source=EXPERT` 发起新 run 走主链路与审批；`SAMPLE_AUDIT` 单只记复核结论，判误时告警并按 §8 视觉回滚条件评审。
6. 视觉服务不可用 → `DEGRADED`，`degrade_reason=VISION_UNAVAILABLE`，提示文字描述症状；worker 任务 `expert-sla-scan`（Leader）扫 `sla_due_at` 已过的未结专家单 → 写 `outbox(notify.send, kind=EXPERT_SLA_BREACH)` 通知专家组负责人。

SQL

```sql
CREATE TABLE diagnosis (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL, field_id bigint unsigned NOT NULL,
  run_id bigint unsigned NOT NULL, crop varchar(32) NOT NULL, phenology_stage varchar(32) NOT NULL,
  image_refs json NOT NULL,              -- [{raw_id, source, collected_at}]
  quality json NOT NULL, candidates json NULL,
  status varchar(16) NOT NULL,           -- RETAKE / LOW_CONF / CONFIDENT / EXPERT_RESOLVED / DEGRADED
  top_pest_id varchar(64) NULL,          -- 必须属于 mcp-crop 该作物病虫害谱
  top_score decimal(4,3) NULL, margin decimal(4,3) NULL,
  model_tier varchar(32) NULL, calibration_ver varchar(16) NULL,   -- 专家单经 expert_case.uk_diagnosis 反查，不存反向 id
  schema_ver smallint NOT NULL, row_ver int NOT NULL DEFAULT 0, created_at datetime(3) NOT NULL, updated_at datetime(3) NOT NULL,
  PRIMARY KEY (id), KEY idx_run (tenant_id, farm_id, run_id), KEY idx_scope_field (tenant_id, farm_id, field_id, created_at));
CREATE TABLE expert_case (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL,
  diagnosis_id bigint unsigned NOT NULL, run_id bigint unsigned NOT NULL,
  case_kind varchar(16) NOT NULL,        -- LOW_CONF / OUT_OF_SPECTRUM / SAMPLE_AUDIT
  status varchar(16) NOT NULL,           -- OPEN / ASSIGNED / RESOLVED / CLOSED
  assignee_id bigint unsigned NULL, resolution json NULL,   -- {pest_id, verdict(AGREE/DISAGREE), features, citations}
  sla_due_at datetime(3) NOT NULL, resolved_at datetime(3) NULL, followup_run_id bigint unsigned NULL,
  row_ver int NOT NULL DEFAULT 0, created_at datetime(3) NOT NULL, updated_at datetime(3) NOT NULL,
  PRIMARY KEY (id), UNIQUE KEY uk_diagnosis (tenant_id, farm_id, diagnosis_id), KEY idx_run (tenant_id, farm_id, run_id),
  KEY idx_scope_queue (tenant_id, farm_id, status, sla_due_at),
  KEY idx_sla (status, sla_due_at));     -- worker 专用：专家跨农场待办与 SLA 扫描
ALTER TABLE advice_card
  ADD COLUMN source varchar(8) NOT NULL DEFAULT 'ASK',  -- ASK / ALERT / PLAN / EXPERT
  ADD COLUMN diagnosis_id bigint unsigned NULL, ADD COLUMN degrade_reason varchar(32) NULL, ALGORITHM=INSTANT;
ALTER TABLE advice_card ADD KEY idx_diagnosis (tenant_id, farm_id, diagnosis_id);
-- 专家认领（乐观锁）
UPDATE expert_case SET status='ASSIGNED', assignee_id=?, row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND id=? AND status='OPEN' AND row_ver=?;
-- 专家结论：同一事务写 expert_case、diagnosis 与 outbox
UPDATE expert_case SET status='RESOLVED', resolution=?, resolved_at=?, row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND id=? AND status='ASSIGNED' AND assignee_id=? AND row_ver=?;
UPDATE diagnosis SET status='EXPERT_RESOLVED', top_pest_id=?, row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND id=? AND status='LOW_CONF' AND row_ver=?;   -- SAMPLE_AUDIT 单不改 diagnosis
INSERT INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, attempts, next_run_at, row_ver, created_at, updated_at)
VALUES (?, ?, 'expert.resolved', ?, CONCAT('expert.resolved:', ?, ':0:1'), ?, 'PENDING', 0, ?, 0, ?, ?);
-- SLA 扫描（worker）
SELECT id, tenant_id, farm_id FROM expert_case WHERE status IN ('OPEN','ASSIGNED') AND sla_due_at < ? LIMIT 100;
```

Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `rl:vision:{farm_id}:{yyyymmdd}` | 48h | 单农场每日诊断次数上限（成本保护）；日期按农场本地日期（母文档 §4.6.2-9） |
| `media:upload:{upload_id}` | 10min | 上传归属 `{tenant_id, farm_id, user_id, key, content_type}`，commit 时校验；丢失即上传失效 |

### 6.4 L3 主动预警与排程调度（第 6-9 周）

**目标**：从「问了才答」变成「该说就说」，同时不变成骚扰；多台设备的作业能被统一排程。

**涉及模块**：`internal/service/alert`（扫描调度器、分级、频控、推送）、`internal/service/schedule`（多设备排程与作业窗口计算）、`internal/agent/graph`（预警转 `AdviceCard` 复用主链路）、`internal/agent/harness/runner`（预警触发的 run 同样分配 `run_id`）、交互层推送通道（`UserChannel`）、开关 `enable_proactive_alert / enable_multi_device_schedule`、指标 `alert_effective_rate / alert_close_rate / alert_dedup_hits`。

**功能任务**

1. 扫描调度器：按日期、节气、物候进度（`mcp-crop` 生育期推算）、气象预警（`mcp-weather`）触发扫描；时间一律取自注入的 `Clock`，不直接读系统时间；按日计数、静默时段、节气按农场本地日期换算（母文档 §4.6.2-9）。
2. 扫描以「农场 × 地块」为单位生成候选事件，每个事件按单个地块计算去重键（`field_id + rule_id + 锚定窗口`，锚定窗口取气象预警 ID 或农场本地日期桶，不取扫描时刻）；同一次扫描的多地块合并只在推送展示层做，不影响去重。
3. 预警分级：`紧急防灾`（台风 / 暴雨 / 低温 / 高温 / 干旱）、`防治窗口临近`、`常规农事提醒`；级别决定推送渠道与是否穿透静默时段（仅紧急防灾可穿透）。
4. 频控与配额：按农场按日推送上限、同类事件去重与合并、静默时段；配额参数可配置，初值在预警灰度首档观察窗实测后确定。`紧急防灾` 不受配额与静默时段限制，只计数并在超过配置上限时告警。
5. 预警内容走既有出口：结论与理由过引用校验与拒答规则，无依据的预警不推送，计入 `VALIDATION_FAILED`。
6. 预警一键转 `AdviceCard`：用户点开预警即以预警上下文发起一次主链路 run，后续审批链路完全复用，不新开下发通道。
7. 多设备协同排程：输入为一批未下发 `WorkOrder`（`DRAFT / PENDING_APPROVAL / APPROVED`）、可用设备清单与续航（`mcp-device`）、作业窗口（`constraint_checker` 输出），输出排程方案（设备分配 + 时间片 + 航线）；设备按租户级共享，互斥锁与冲突查询按 `(tenant_id, device_id)`（母文档 §4.6.2-12）；`DISPATCHED / EXECUTING` 单只参与冲突计算，不允许改写；排程只调整 `assigned_device_id / time_window_*`，不触发下发。
8. 排程写回与换票：改写字段与状态回退用同一条带 `row_ver` 守卫的 UPDATE；`PENDING_APPROVAL` 与 `APPROVED` 单在同一事务内旧票 `SUPERSEDED`、开新票（`ticket_seq+1`、新快照、新 `snapshot_digest`），`APPROVED` 单回到 `PENDING_APPROVAL`；审批通过时票据记下 `approved_schedule_version`，下发器比对不一致即不下发，排程改动无法绕过审批。
9. 成本埋点：预警扫描与预警转建议的 token 成本单列，进成本看板。
10. **评测与指标**（随本能力同 PR 交付）：主动预警样本用 `FrozenClock.Advance()` 推进时间触发扫描，含跨农场本地日、跨静默时段的用例；新增 `alert_expected`（该推则推、不该推不推）、`alert_dedup`（同一事件不重复推送）、`schedule_no_conflict`（无同地块同时段互斥冲突、无同设备跨农场重叠、无航线重叠）打分器，红线样本进 PR 冒烟；`ALERT_EMITTED / ALERT_SUPPRESSED / SCHEDULE_CONFLICT` 事件、`alert_effective_rate / alert_close_rate` 日指标与「预警关闭率突增」「排程失败率突增」告警在各自灰度前上线。

**交付物**：`internal/service/alert`、`internal/service/schedule` 两个包；预警分级与频控配置；预警与排程样本子集及打分器。

**验收**

1. 预警有效率达标、用户关闭率低于阈值（阈值依灰度前实测确定）；线上实测由田间验证方执行，本团队提供指标埋点与看板。
2. 同一事件不重复推送（`alert_dedup` pass^5 = 1）。
3. 预警成本进成本看板，与日志统计口径一致。
4. 关闭 `enable_proactive_alert` 后无任何主动推送（日志与推送通道计数为 0，含关闭前已排队的顺延预警）。
5. 排程子集 `schedule_no_conflict` 全通过；排程改动未下发作业单时 100% 换票，`approved_schedule_version` 不一致的单 100% 不下发。
6. 每个农场每天第一条预警正常推送（配额行自动建立）；`紧急防灾` 在配额用尽时仍推送。
7. 扫描峰值压测（母文档 §7.3-6）：Redis 调度队列清空后重建、气象预警同时命中全部农场两种场景下，扫描速率不超过 `alert.scan_max_per_min`，无重复推送。

**技术实现**

流程

1. 扫描调度：事实源是 `farm_profile.alert_next_scan_at`。worker 任务 `alert-scheduler`（Leader）每分钟从 zset `alert:next_scan` 取到期农场；zset 不存在或距上次重建超过 10 分钟时，从 MySQL 重建。逐个农场用条件 UPDATE 认领（影响行数 0 即被别的副本认领或已改期），下次时间 = 本地规则间隔 + 按 `farm_id` 哈希的抖动；每分钟认领数不超过 `alert.scan_max_per_min`，超出的留到下一分钟。气象预警触发时只把受影响农场的 `alert_next_scan_at` 提前到「当前 + 抖动」，同样受速率上限约束。P4 的预算覆盖开关 `budget_hold.enable_proactive_alert` 生效时，只把 `WINDOW / ROUTINE` 级规则的扫描间隔乘以 `alert.budget_hold_factor`；`EMERGENCY` 级规则与气象预警触发的扫描不受影响，该覆盖开关也不参与 `enable_proactive_alert` 生效值与发送前复查。
2. 执行：按农场取开关快照，关闭则只推进 `alert_next_scan_at` → `service/alert.ScanOnce` 作为一个 `agent_run`（`user_id=0`，系统发起）执行，可回放。
3. 过闸：规则 `policy/alert_rules.yaml` 产出候选（每地块一条）→ 去重（Redis 快速路径，`uk_dedup` 兜底）→ 配额：按农场本地日期 `INSERT IGNORE` 当天配额行，再条件 UPDATE 占用，`EMERGENCY` 走不带上限条件的计数 UPDATE → 静默时段（农场本地时间）内非 `EMERGENCY` 置 `DEFERRED`，`deliver_at` = 静默结束的 UTC 时刻。
4. 推送：同一事务写 `alert_event`（`QUEUED` 或 `DEFERRED`）与 `INSERT IGNORE outbox(topic=notify.send, kind=ALERT, dedup_key=notify:ALERT:{alert_id}:0, next_run_at=deliver_at 或当前)`；同一次扫描同一规则命中的多块地合并为一条 outbox 消息（`payload.alert_ids[]`，`ref_id` 取最小 `alert_id`），配额按推送条数计。worker 发送前用 `BoolLive` 复查 `enable_proactive_alert`，关闭则置 `SUPPRESSED(reason=FLAG_OFF)`；发送调用 `UserChannel` 带幂等键 `alert:{alert_id}`，成功后条件更新为 `PUSHED`，写 `ALERT_EMITTED / ALERT_SUPPRESSED` 事件。
5. 转提问：`POST /api/v1/farms/{farm_id}/ask` 新增可选 `alert_id`，以预警上下文走主链路，`advice_card.source=ALERT`，同时回写 `alert_event.advice_id`；反馈 `POST /api/v1/farms/{farm_id}/alerts/{alert_id}/feedback`（`USEFUL / USELESS / FALSE_POSITIVE / MUTED`）。
6. 排程求解（事务外）：`service/schedule` 读候选工单、设备与窗口，在内存中求解，不持锁、不开事务；需换票的单按求解结果先 `RawStore.Put` 新快照（key 含 `order_id` 与 `ticket_seq`，回滚重跑覆盖同一对象），把 MinIO IO 留在锁外。
7. 排程写回（事务内）：按 `device_id` 升序逐个抢 `lock:schedule:{tenant_id}:{device_id}`，其中 `APPROVED` 单再按 `order_id` 升序抢 `lock:order:{order_id}`（与下发器互斥）→ 开事务 → 冲突查询（同租户同设备跨农场）→ 对每张单执行带守卫的改写 UPDATE（`status IN ('DRAFT','PENDING_APPROVAL','APPROVED') AND row_ver=?`，`APPROVED` 改回 `PENDING_APPROVAL` 并清空 `dispatch_deadline_at`）；任一行影响 0 或冲突非空，整批回滚并重新求解（≤ 3 次，仍失败按 L4 排程失败处理）。
8. 换票（同一事务）：`PENDING_APPROVAL / APPROVED` 单把当前 `OPEN / APPROVED` 票置 `SUPERSEDED`，按 P1 开票流程开 `ticket_seq+1` 的新票（新快照、新 `snapshot_digest`，`PREPARING → OPEN`）；`APPROVED → PENDING_APPROVAL` 写 `work_order_transition(reason_code=SCHEDULE_CHANGED)`；写 `outbox(notify.send, kind=SCHEDULE_CHANGED)`。旧票对应的 `outbox(order.dispatch)` 消息在下发器第①步因工单不是 `APPROVED` 而丢弃；新票通过后按新 `ticket_id` 写新的下发消息。
9. 审批与下发守卫：`Decide` 的 `ORDER` 分支 APPROVE 时同事务写 `approval_ticket.approved_schedule_version = work_order.schedule_version`；下发器在母文档 §4.6.2-3 第①步追加比对两者，不一致 → `DISPATCH_HELD` 并告警，不调 Adapter。

SQL

```sql
CREATE TABLE alert_event (               -- 一地块一行；合并只在推送展示层
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL, field_id bigint unsigned NOT NULL,
  rule_id varchar(64) NOT NULL, level varchar(12) NOT NULL,  -- EMERGENCY / WINDOW / ROUTINE
  anchor varchar(64) NOT NULL,           -- 气象预警 ID 或农场本地日期桶（如 2027-04-12）
  dedup_key char(64) NOT NULL,           -- sha256(field_id|rule_id|anchor)
  window_from datetime(3) NOT NULL, window_to datetime(3) NOT NULL,
  status varchar(16) NOT NULL,           -- CANDIDATE / SUPPRESSED / QUOTA_EXCEEDED / QUEUED / DEFERRED / PUSHED / FAILED
  scan_run_id bigint unsigned NOT NULL, advice_id bigint unsigned NULL,
  deliver_at datetime(3) NULL, pushed_at datetime(3) NULL,
  feedback varchar(16) NULL,             -- USEFUL / USELESS / FALSE_POSITIVE / MUTED
  row_ver int NOT NULL DEFAULT 0, created_at datetime(3) NOT NULL, updated_at datetime(3) NOT NULL,
  PRIMARY KEY (id), UNIQUE KEY uk_dedup (tenant_id, farm_id, dedup_key), KEY idx_scope_deliver (tenant_id, farm_id, status, deliver_at),
  KEY idx_scan_run (tenant_id, farm_id, scan_run_id), KEY idx_advice (tenant_id, farm_id, advice_id),
  KEY idx_pushed (status, pushed_at));   -- worker 专用：日指标汇总
CREATE TABLE alert_quota (               -- 配额事实源；quota_date 为农场本地日期
  id bigint unsigned NOT NULL AUTO_INCREMENT, tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL,
  quota_date date NOT NULL, level varchar(12) NOT NULL, used smallint NOT NULL DEFAULT 0, limit_value smallint NOT NULL,
  updated_at datetime(3) NOT NULL, PRIMARY KEY (id), UNIQUE KEY uk_quota (tenant_id, farm_id, quota_date, level));
-- 配额：先建当天行（limit_value 取配置），再占用；非 EMERGENCY 影响行数 0 即超额
INSERT IGNORE INTO alert_quota (tenant_id, farm_id, quota_date, level, used, limit_value, updated_at) VALUES (?, ?, ?, ?, 0, ?, ?);
UPDATE alert_quota SET used=used+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND quota_date=? AND level=? AND used<limit_value;
-- EMERGENCY 不受配额限制，只计数；used >= limit_value 时告警
UPDATE alert_quota SET used=used+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND quota_date=? AND level='EMERGENCY';
-- 推送入队（与 alert_event 同事务）；通知失败不回滚主事务
INSERT IGNORE INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, next_run_at, created_at, updated_at)
VALUES (?, ?, 'notify.send', ?, CONCAT('notify:ALERT:', ?, ':0'), ?, 'PENDING', ?, ?, ?);   -- next_run_at = deliver_at 或当前
-- 发送成功（outbox 消费者）
UPDATE alert_event SET status='PUSHED', pushed_at=?, row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND id=? AND status IN ('QUEUED','DEFERRED') AND row_ver=?;
-- 扫描调度事实源：zset 只做加速
ALTER TABLE farm_profile ADD COLUMN alert_next_scan_at datetime(3) NULL, ALGORITHM=INSTANT;   -- 开关首次在该农场打开时写入
ALTER TABLE farm_profile ADD KEY idx_next_scan (alert_next_scan_at);   -- worker 专用
SELECT tenant_id, farm_id, alert_next_scan_at FROM farm_profile WHERE alert_next_scan_at < ? ORDER BY alert_next_scan_at LIMIT 5000;   -- 重建 zset，取未来 1 天内
UPDATE farm_profile SET alert_next_scan_at=? WHERE tenant_id=? AND farm_id=? AND alert_next_scan_at=?;   -- 认领：以旧值做 CAS，不升 row_ver，避免与档案编辑互相冲突
ALTER TABLE advice_card ADD COLUMN alert_id bigint unsigned NULL, ALGORITHM=INSTANT;
ALTER TABLE advice_card ADD KEY idx_alert (tenant_id, farm_id, alert_id);
ALTER TABLE work_order
  ADD COLUMN assigned_device_id bigint unsigned NULL, ADD COLUMN schedule_version int NOT NULL DEFAULT 0, ALGORITHM=INSTANT;
ALTER TABLE work_order ADD KEY idx_device_window (tenant_id, assigned_device_id, time_window_from);   -- 设备按租户共享，跨农场冲突查询用，不以 farm_id 开头
ALTER TABLE approval_ticket ADD COLUMN approved_schedule_version int NULL, ALGORITHM=INSTANT;   -- ORDER 票 APPROVE 时写入
-- 冲突检查（事务内，持设备锁）：同租户同设备时间窗重叠，排除本批工单
SELECT id, farm_id FROM work_order WHERE tenant_id=? AND assigned_device_id=? AND time_window_from < ? AND time_window_to > ?
   AND id NOT IN (?) AND status IN ('PENDING_APPROVAL','APPROVED','DISPATCHED','EXECUTING') FOR UPDATE;
-- 改写：字段与状态一条 UPDATE；MySQL 按从左到右赋值，dispatch_deadline_at 必须写在 status 之前
UPDATE work_order SET assigned_device_id=?, time_window_from=?, time_window_to=?, schedule_version=schedule_version+1,
       dispatch_deadline_at=IF(status='APPROVED', NULL, dispatch_deadline_at),
       status=IF(status='APPROVED', 'PENDING_APPROVAL', status), row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND id=? AND status IN ('DRAFT','PENDING_APPROVAL','APPROVED') AND row_ver=?;
INSERT INTO work_order_transition (tenant_id, farm_id, order_id, run_id, from_status, to_status, actor_type, actor_id, reason_code, created_at)
VALUES (?, ?, ?, ?, 'APPROVED', 'PENDING_APPROVAL', 'SYSTEM', 0, 'SCHEDULE_CHANGED', ?);   -- 仅原状态为 APPROVED 时写
-- 换票：旧票作废，再按 P1 复检退回的开票 SQL 插入 ticket_seq+1 的新票（ticket_kind='ORDER'）
UPDATE approval_ticket SET status='SUPERSEDED', row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND ticket_kind='ORDER' AND order_id=? AND status IN ('PREPARING','OPEN','APPROVED') AND row_ver=?;
INSERT IGNORE INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, next_run_at, created_at, updated_at)
VALUES (?, ?, 'notify.send', ?, CONCAT('notify:SCHEDULE_CHANGED:', ?, ':', ?), ?, 'PENDING', ?, ?, ?);   -- ref_id=order_id，新 ticket_id
-- 下发器第①步追加：已通过票的排程版本必须等于当前版本
SELECT w.status, w.schedule_version, t.id AS ticket_id, t.approved_schedule_version FROM work_order w
  JOIN approval_ticket t ON t.tenant_id=w.tenant_id AND t.farm_id=w.farm_id AND t.ticket_kind='ORDER' AND t.order_id=w.id AND t.status='APPROVED'
 WHERE w.tenant_id=? AND w.farm_id=? AND w.id=?;
```

Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `alert:next_scan` | 1d，每次 ZADD 续期 | zset `farm_id → alert_next_scan_at`，只做加速；丢失或过期由 Leader 从 `farm_profile` 重建 |
| `alert:dedup:{farm_id}:{dedup_key}` | 预警窗口长度，≤ 7d | 去重快速路径，`uk_dedup` 兜底 |
| `quota:alert:{farm_id}:{yyyymmdd}` | 48h | 配额快速预判，日期为农场本地日期，`alert_quota` 兜底 |
| `lock:schedule:{tenant_id}:{device_id}` | 30s | 同一设备的排程写回串行（设备租户级共享）；锁内只有 MySQL 操作，丢失由 `row_ver` 与行锁兜底 |
| `lock:order:{order_id}` | 10s（沿用 P1） | 改写 `APPROVED` 单时与下发器互斥 |

### 6.5 L4 风险控制与降级策略（随 L1-L3、L5、L6 各自周次，各项与对应能力同 PR）

**目标**：高级能力带来的新风险必须有对应的刹车，每条降级路径可演练、可观测。

**涉及模块**：`internal/service/alert`（误报抑制）、`internal/service/schedule`（冲突消解、滚动重排）、`internal/agent/knowledge`（RAG 异常回落）、`internal/service/vision`（视觉不可用降级）、`internal/service/workorder`（已审批单变更复审）、Phase 1 故障分类与降级矩阵（扩表）。

**功能任务**

1. 预警误报抑制：同类预警被用户标记「无用」或被专家判误报连续达到阈值时，自动降频；降频后**只能人工恢复**，恢复操作留审计。
2. 多设备冲突消解：同地块同时段互斥；同设备跨农场时间窗重叠（设备租户级共享）；航线重叠检测；共享资源（药箱补给点、充电位、水源）排队；冲突无法消解时排程失败并提示人工，不降级为「先到先得」。
3. 作业窗口滚动重排：天气变化触发重算 → 生成变更差异 → 通知审批人；未下发作业单的时间窗或设备变更一律按 L3 第 8 条改写并换票，`APPROVED` 单回到 `PENDING_APPROVAL` 重新确认；`DISPATCHED / EXECUTING` 单不改写，只提示人工。
4. 重排与下发前复检协同：滚动重排不替代 Phase 1 的下发前复检，二者都要执行；复检不通过仍按原规则退回；下发器额外比对 `approved_schedule_version`。
5. RAG 检索异常回落：按 L1 第 6 条决策表执行（索引异常、召回为空或低于阈值都回落固定文件并标注 `degraded=true`，固定文件也未命中才 `KB_MISS` → 拒答）；红线组件不经检索，不受本条影响。
6. 视觉模型不可用：退回纯文本问诊（引导用户描述症状），标注 `degraded=true`，不对图片给出任何结论。
7. 故障分类与降级矩阵扩表：新增「检索异常 / 视觉不可用 / 排程冲突 / 预警误报」四类，每类有错误码、用户可读文案与告警项。
8. **评测**：四类降级各一组样本（`FixtureMCP` 故障注入 + 检索 / 视觉组件故障桩）；`kb_miss_refuse` 扩展到检索异常路径；新增 `no_silent_dispatch`（条件变化后已审批单未经确认不得下发，红线）。

**交付物**：四条降级路径代码与演练记录；扩表后的降级矩阵；对应样本与打分器。

**验收**

1. 每条降级路径有演练记录，降级时用户可读文案与告警均出现。
2. 已审批作业单在条件变化后不会被静默执行（`no_silent_dispatch` pass^5 = 1）。
3. 冲突用例（同地块同时段、航线重叠、共享资源超额）100% 拦截。
4. 误报降频后无人工操作不会自动恢复。
5. 每项降级代码与对应能力在同一 PR 合入（PR 审查清单可查）。

**技术实现**

流程

1. 误报降频：`POST /api/v1/farms/{farm_id}/alerts/{alert_id}/feedback` → `service/alert` 按 `rule_id` 累计 `USELESS / FALSE_POSITIVE` 连续次数（`USEFUL` 清零），达 `alert.throttle_streak`（默认 3）→ 该规则在农场内 `THROTTLED`，非 `EMERGENCY` 去重窗口放大，写审计与 `ALERT_SUPPRESSED(reason=THROTTLED)`。
2. 人工恢复：唯一入口 `POST /api/v1/farms/{farm_id}/alerts/throttle/{rule_id}/restore {reason}`（角色 `agronomist`），写 `audit_log`；代码中不存在自动恢复分支。
3. 冲突消解：求解阶段（事务外）一次取出相关设备在时间窗内的全部活动工单（按 `(tenant_id, assigned_device_id)` 跨农场取），内存中校验地块互斥、设备重叠、航线重叠、补给点 / 充电位容量；写回阶段在设备锁内用 L3 冲突查询复核；无解 → 排程失败，`degrade_reason=SCHEDULE_CONFLICT`，写 `SCHEDULE_CONFLICT` 事件，提示人工。
4. 滚动重排：气象轮询发现 48h 内工单窗口受影响 → 重排 → 走 L3 第 6-8 步改写与换票 → 差异推审批人；下发器按 L3 第 9 步比对 `approved_schedule_version`，不一致不下发（`no_silent_dispatch`）。
5. 检索 / 视觉降级：见 L1 技术实现第 6 步、L2 技术实现第 6 步；降级矩阵新增四类错误码，与能力代码同 PR。

SQL

```sql
-- 降频状态记在 farm_profile，不另建表：{rule_id: {state, streak, throttled_at, restored_by, restored_at}}
ALTER TABLE farm_profile ADD COLUMN alert_throttle json NULL, ALGORITHM=INSTANT;
UPDATE farm_profile SET alert_throttle=JSON_SET(COALESCE(alert_throttle, JSON_OBJECT()), CONCAT('$."', ?, '"'), CAST(? AS JSON)),
       row_ver=row_ver+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND row_ver=?;
-- 复审改写、换票与下发器比对：沿用 L3 SQL（改写 UPDATE 带 row_ver 守卫、旧票 SUPERSEDED、approved_schedule_version 比对），本节不另写
-- 降级矩阵新增错误码只进代码常量与 P1 降级矩阵文档，不建表
```

Redis 键

无（沿用 L3 `lock:schedule:{tenant_id}:{device_id}` 与 `lock:order:{order_id}`）。

### 6.6 L5 质量校验与结果增强（第 6-9 周）

**目标**：让高级能力的产出经过比 Phase 2 更严的出口检查，同时把「为什么」讲到农户能懂。

**涉及模块**：`internal/agent/guardrail/output_validator`（新增数值二次校验、方案冲突检测）、`internal/agent/calc`、`knowledge/common/safety.md`（混用禁忌与间隔规则源）、`internal/agent/graph`（多方案输出、解释生成节点）、`AdviceCard` 契约（多方案需版本升级）。

**功能任务**

1. 数值二次校验：RAG 命中条目中的用量公式与 `calc` 层实现逐一比对（公式版本 + 测试输入下的计算结果），不一致即拒收并落事件，以 `calc` 为准；同时告警知识与规则层漂移。
2. 方案冲突检测 `conflict_checker`：药剂混用禁忌、连续施药间隔、与近期已执行作业（`mcp-field` 历史作业）冲突；规则由农艺专家组整理进 `safety.md` 并署名，代码只执行规则。
3. 冲突检测挂载点：`AdviceCard` 出库前一次；`WorkOrder` 提交审批前一次（与 `constraint_checker` 同位置，结论写入 `constraint_checks[]`）。
4. 多方案对比输出（子开关 `enable_alternatives`，按农场开启，治理同五个能力开关）：给 2-3 个可选方案，每个方案的药剂仍限 `product_candidates`、数值仍由 `calc` 回填、各自带 `Citation`，附取舍理由；由人选择，不由模型代选。`source=PLAN / EXPERT` 的 run 不进入多方案（计划生成与专家回流不在选方案处中断），只对用户发起的 `ASK / ALERT` 生效。
5. 契约变更：多方案需要 `AdviceCard` 新增 `alternatives[]` 字段，走契约版本号与 Owner 批准，同步下游（审批页、evalkit schema、审计）。
6. 解释增强：解释生成节点提示词新增「为什么现在做、为什么这个量」结构，提示词版本化，变更过 `make eval` 门禁。
7. **评测与指标**（随本能力同 PR 交付）：新增 `conflict_blocked`（混用禁忌与间隔冲突必拦，红线，样本进 PR 冒烟）、`formula_consistent`（RAG 公式与 `calc` 一致）打分器；`explanation_quality` judge 扩展到多方案输出；`numbers_match_calc` 与 `product_in_candidates` 覆盖 `alternatives[]` 每个方案；`ALTERNATIVE_SELECTED` 事件与多方案采纳率指标在 `enable_alternatives` 灰度前上线。

**交付物**：`conflict_checker`；冲突规则条目（专家署名）；`AdviceCard` 新版本 schema；多方案与解释增强提示词版本。

**验收**

1. 混用禁忌与间隔冲突用例 100% 拦截（`conflict_blocked` pass^5 = 1）。
2. 人为注入一条公式与 `calc` 不一致的知识条目，建议被拒收并告警。
3. 多方案中每个方案都通过引用、数值、药剂三项校验。
4. 多方案输出的采纳率相对单方案对照有可观测提升（线上 A/B，由灰度数据判定；按农场分层，结论注明农场级分组偏差）。
5. 解释可读性经农户抽样评分达标（抽样评分由田间验证方执行，本团队提供评分样本导出）。

**技术实现**

流程

1. 公式校验：知识条目 front-matter 新增可选 `formulas[]{calc_ref, test_vectors}`；`cmd/migrate kb-reindex` 与知识仓库 CI 用 `agent/calc` 跑 `test_vectors`，不一致构建失败；运行期 `output_validator` 检查 `numbers[].calc_ref` 与条目声明一致。`calc` 自身的参数仍按 `kb_id@version` 精确读取，不读检索结果（母文档 §4.6.2-10）。
2. 多方案生成：`effective(enable_alternatives)` 为真且 `source IN (ASK, ALERT)` 时，`agent/graph` 从 `product_candidates` 取 2-3 个方案，各自经 `calc` 回填数值；模型只写 `tradeoff` 文字；开关快照随 run 写入 `agent_run.flags_on`（L0）。
3. 冲突检测：图节点 `conflict_check` 按 `knowledge/common/safety.md` 规则逐方案检查混用禁忌、安全间隔、与已排工单的冲突，结果写入 `alternatives[].conflict_checks`；全部被剔除 → `refuse_reason=CONFLICT`。
4. 展示顺序由代码决定：按风险等级升序、再按 `product_id`，`alt_id` 依次为 `a0 / a1 / a2`，不标「推荐」。
5. 选择：`persist` 后在 `select_alternative` 中断（`WAIT_INPUT`）→ `POST /api/v1/farms/{farm_id}/advice/{advice_id}/select {alt_id}` 只允许写一次 → resume 进 `to_order`，`PRE_APPROVAL` 复检照常写 `constraint_check`；写 `ALTERNATIVE_SELECTED` 事件。

SQL

```sql
ALTER TABLE advice_card
  ADD COLUMN alternatives json NULL,        -- [{alt_id, selected_product, numbers, tradeoff, conflict_checks}]
  ADD COLUMN selected_alt_id varchar(8) NULL, ALGORITHM=INSTANT;
-- 新行 schema_ver=2；v1 行按原解码器读取，永不回写
-- 选定方案：只允许写一次，影响行数 0 返回 409
UPDATE advice_card SET selected_alt_id=?, selected_product=?, numbers=?
 WHERE tenant_id=? AND farm_id=? AND id=? AND selected_alt_id IS NULL
   AND JSON_CONTAINS(JSON_EXTRACT(alternatives, '$[*].alt_id'), JSON_QUOTE(?));
-- A/B 对照：只按 enable_alternatives 的快照值分组（不按 flags_digest，避免其他开关变化导致分组碎片化），按农场分层
SELECT a.farm_id, JSON_CONTAINS(r.flags_on, '"enable_alternatives"') AS alt_on, COUNT(*) AS advices,
       SUM(w.status IN ('DONE','PARTIALLY_DONE')) AS adopted
  FROM advice_card a
  JOIN agent_run r ON r.tenant_id=a.tenant_id AND r.farm_id=a.farm_id AND r.id=a.run_id
  LEFT JOIN work_order w ON w.tenant_id=a.tenant_id AND w.farm_id=a.farm_id AND w.advice_id=a.id AND w.dispatch_mode='LIVE'
 WHERE a.tenant_id=? AND a.source IN ('ASK','ALERT') AND a.created_at >= ? AND a.created_at < ?
 GROUP BY a.farm_id, alt_on;
```

Redis 键

无。

### 6.7 L6 自动规划灰度（多步作业计划）（第 10-11 周开发，第 12-13 周灰度）

**目标**：从单次建议升级到「一周农事计划」，但绝不越过人审红线。

**涉及模块**：`internal/service/plan`（计划生成、计划级审批、变更追踪）、`internal/service/schedule`（复用排程）、`internal/service/approval`（计划级 + 逐单二次确认）、`internal/agent/harness/checkpoint`（多步计划的分段审批断点）、开关 `enable_auto_plan`、指标 `plan_adoption_rate / plan_edit_rate`。

**功能任务**

1. 计划生成：按生育期排布未来 7-14 天农事项；计划中每一项都是一张独立的 `AdviceCard`，带各自 `Citation`、数值由 `calc` 回填；计划本身不含模型自由生成的数值。
2. 计划契约：新增 `FarmPlan`（`plan_id / tenant_id / farm_id / horizon / items[]{advice_id, order_id?, planned_window} / status / version`），只引用已有契约，不复制字段；按契约冻结规则评审。
3. 计划级审批：整体审批通过只表示「计划可用」，不触发任何下发；计划内每张 `WorkOrder` 执行前仍需单独确认（逐单二次确认），沿用既有审批时效与 `EXPIRED` 规则。
4. 分段审批断点：HITL 对计划级审批与逐单确认分别落 `RunState` 快照，`resume` 只推进当前一段，不连带推进后续作业单。
5. 计划变更追踪：天气或长势变化触发重排，生成新 `version`，按 `item_key` 与当前 `ACTIVE` 版本做 diff，只有新增与变更项重跑主链路，未变项沿用原 `advice_id / order_id`；同一计划项任何时刻最多一张活动工单（数据库唯一键兜底）；变更项的旧单若未下发则作废，已下发则保留并提示人工；只换设备或时间窗的项走 L3 第 8 条改写并换票。生成失败（某项 `KB_MISS` 或排程无解）时整版作废，已建 `DRAFT` 单一并作废。
6. 小流量灰度：`enable_auto_plan` 按农场开启，先 1 家，2 周观察窗。
7. **评测与指标**（随本能力同 PR 交付）：新增计划子集；`stop_at_approval` 扩展为计划级断言（计划审批通过后、逐单确认前 `RecordingAdapter` 调用数为 0）；`state_machine_legal` 覆盖计划变更引发的复审迁移，以及计划票过期 / 驳回 / 通过三种结局（驳回、过期后计划内 `DRAFT` 单全部作废，通过后无下发）；新增 `plan_no_duplicate_order`（同一 `item_key` 不出现两张活动工单，红线）；`numbers_match_calc` 覆盖计划内每一项；`PLAN_REVISED` 事件与 `plan_adoption_rate / plan_edit_rate` 指标在灰度前上线。

**交付物**：`internal/service/plan` 包；`FarmPlan` schema；计划审批页（计划总览 + 逐单确认 + 变更点高亮）；计划子集与扩展打分器。

**验收**

1. 计划中每个作业单执行前仍需确认，**零自动执行**（计划子集 pass^5 = 1 + 灰度期审计日志全量核对）。
2. 计划变更后受影响的未下发作业单 100% 作废或换票，`plan_no_duplicate_order` pass^5 = 1。
3. 计划采纳率与人工改单率进指标看板。
4. 灰度期间无越权执行，未开开关的农场看不到任何计划功能。

**技术实现**

流程

1. 生成：`POST /api/v1/farms/{farm_id}/plans` 或 worker `plan-rebuild` → 抢 `lock:plan:{farm_id}` → 子图 `plan_build` 按 `policy/plan_rules.yaml` 与生育期排出 7-14 天农事项（`horizon_*` 为农场本地日期，母文档 §4.6.2-9）→ 与当前 `ACTIVE` 版本按 `item_key` 做 diff：`UNCHANGED` 项不跑主链路，新版本行沿用原 `advice_id / order_id`；`ADDED / CHANGED` 项以 `source=PLAN` 跑主链路到 `persist`，作业类再经 `to_order` 生成 `DRAFT` 工单（带 `root_plan_id / plan_item_key`）；旧单已 `DISPATCHED / EXECUTING` 的 `CHANGED` 项按 `UNCHANGED` 处理并通知人工；`REMOVED` 项只记入 diff。
2. 排程与入库：作业项交 `service/schedule`（L3）排设备与时间窗 → 写 `farm_plan(status=PENDING_APPROVAL)` 与 `farm_plan_item`，计划只引用 `advice_id / order_id`，不含数值。任一项 `KB_MISS` 或排程无解 → 整版 `FAILED`，本版已建 `DRAFT` 单同事务 `CANCELED(reason_code=PLAN_FAILED)`，当前 `ACTIVE` 版本不受影响。
3. 计划级审批：开 `approval_ticket(ticket_kind=PLAN, plan_id, order_id=NULL)`，快照为本版计划项摘要与 diff（`RawStore.Put(kind=SNAPSHOT)`），`risk_level` 取计划项最高风险（只用于展示与时效），`confirm_required=0`，`required_role` 取 `policy` 的计划审批配置；图在此 interrupt。
4. 激活（`Decide` 的 `PLAN` 分支 APPROVE，同一事务）：旧版本 `SUPERSEDED`、新版本 `ACTIVE` → `UNCHANGED` 项从旧行同步 `status / order_id` → 旧版本未完成项置 `SUPERSEDED` → `CHANGED / REMOVED` 项的旧单若仍为 `DRAFT / PENDING_APPROVAL / APPROVED`，置 `CANCELED(reason_code=PLAN_REVISED)`、其 `ORDER` 票置 `SUPERSEDED`、写 transition；不触发任何下发。驳回 / 过期 → 计划 `REJECTED / EXPIRED`，本版 `DRAFT` 单 `CANCELED`。
5. 逐单确认：worker `plan-item-due`（多副本均可跑）每 5 分钟扫临近（默认 12h）且所属计划为 `ACTIVE` 的 `PLANNED` 项 → 条件 UPDATE 认领为 `CONFIRMING`（影响行数 0 跳过）→ 起子图 `order_confirm`：重跑 `PRE_APPROVAL` → 开 `ticket_kind=ORDER` 票 → 人确认后才进入下发；结束时按 `order_id` 回写 `ACTIVE` 版本中的对应项。
6. 变更：天气 / 长势变化 → 走第 1-4 步得到 `version+1`，写 `PLAN_REVISED` 事件；只换设备或时间窗的项直接走 L3 改写与换票，不生成新单。
7. `ticket_kind` 分支（P1 已有路径须逐一区分，`PLAN` 票不触发任何下发逻辑）：
   - `Open`：`ORDER` 由 WorkOrder 快照算风险；`PLAN` 按第 3 步。
   - `Decide` APPROVE：`ORDER` 写 `outbox(order.dispatch)` 与 `approved_schedule_version`；`PLAN` 只做第 4 步激活。REJECT：`PLAN` 置计划 `REJECTED` 并作废 `DRAFT` 单。
   - 时效扫描：`PLAN` 票过期只把计划置 `EXPIRED`，不动任何工单的 `EXPIRED`。
   - 复检退回、排程换票、下发器校验：只处理 `ticket_kind='ORDER'`。
   - 审批列表与审批页：按 `ticket_kind` 分 tab，计划票展示计划总览与变更点。

SQL

```sql
CREATE TABLE farm_plan (                  -- 一个版本一行
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL,
  root_plan_id bigint unsigned NOT NULL,  -- 首版 id，同一计划各版本共用
  version int NOT NULL, horizon_from date NOT NULL, horizon_to date NOT NULL,
  status varchar(16) NOT NULL,            -- DRAFT / PENDING_APPROVAL / ACTIVE / SUPERSEDED / REJECTED / EXPIRED / FAILED / READ_ONLY（回滚后）
  run_id bigint unsigned NOT NULL, ticket_id bigint unsigned NULL, revision_reason varchar(32) NULL, diff json NULL,
  active_flag tinyint GENERATED ALWAYS AS (IF(status='ACTIVE', 1, NULL)) STORED,
  schema_ver smallint NOT NULL, row_ver int NOT NULL DEFAULT 0, created_at datetime(3) NOT NULL, updated_at datetime(3) NOT NULL,
  PRIMARY KEY (id), UNIQUE KEY uk_version (tenant_id, farm_id, root_plan_id, version),
  UNIQUE KEY uk_one_active (tenant_id, farm_id, active_flag),   -- 每个农场只有一个 ACTIVE 版本
  KEY idx_run (tenant_id, farm_id, run_id), KEY idx_ticket (tenant_id, farm_id, ticket_id));
CREATE TABLE farm_plan_item (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  tenant_id bigint unsigned NOT NULL, farm_id bigint unsigned NOT NULL, plan_id bigint unsigned NOT NULL,
  item_key varchar(64) NOT NULL,          -- {field_id}:{task_type}:{window_bucket}，跨版本稳定
  field_id bigint unsigned NOT NULL, task_type varchar(16) NOT NULL, advice_id bigint unsigned NOT NULL, order_id bigint unsigned NULL,
  planned_from datetime(3) NOT NULL, planned_to datetime(3) NOT NULL,
  change_type varchar(12) NOT NULL,       -- ADDED / CHANGED / UNCHANGED / REMOVED
  status varchar(12) NOT NULL,            -- PLANNED / CONFIRMING / CONFIRMED / DONE / SKIPPED / CANCELED / SUPERSEDED
  row_ver int NOT NULL DEFAULT 0, updated_at datetime(3) NOT NULL,
  PRIMARY KEY (id), UNIQUE KEY uk_item (tenant_id, farm_id, plan_id, item_key),
  KEY idx_order (tenant_id, farm_id, order_id), KEY idx_advice (tenant_id, farm_id, advice_id),
  KEY idx_due (status, planned_from));   -- worker 专用：plan-item-due 跨农场扫描
ALTER TABLE advice_card ADD COLUMN plan_id bigint unsigned NULL, ALGORITHM=INSTANT;
ALTER TABLE advice_card ADD KEY idx_plan (tenant_id, farm_id, plan_id);
ALTER TABLE work_order ADD COLUMN plan_id bigint unsigned NULL, ADD COLUMN root_plan_id bigint unsigned NULL,
  ADD COLUMN plan_item_key varchar(64) NULL, ALGORITHM=INSTANT;
-- 同一计划项最多一张活动工单（DRAFT 不计，未通过版本的草稿不会互相冲突）；STORED 生成列不是 INSTANT，低峰执行
ALTER TABLE work_order ADD COLUMN plan_live_key varchar(96) GENERATED ALWAYS AS
  (IF(root_plan_id IS NOT NULL AND status IN ('PENDING_APPROVAL','APPROVED','DISPATCHED','EXECUTING'),
      CONCAT(root_plan_id, ':', plan_item_key), NULL)) STORED,
  ADD UNIQUE KEY uk_plan_live (tenant_id, farm_id, plan_live_key), ADD KEY idx_plan (tenant_id, farm_id, plan_id);
-- ticket_kind 列 P0 已有（varchar(16)），P3 只扩枚举 PLAN；PLAN 票 order_id 为空、plan_id 必填（service 层校验）；非 INSTANT，低峰执行
ALTER TABLE approval_ticket ADD COLUMN plan_id bigint unsigned NULL, MODIFY order_id bigint unsigned NULL,
  ADD UNIQUE KEY uk_plan_seq (tenant_id, farm_id, ticket_kind, plan_id, ticket_seq);
-- 激活：同事务内旧版本 SUPERSEDED，再把新版本置 ACTIVE（uk_one_active 兜底双 ACTIVE）
UPDATE farm_plan SET status='SUPERSEDED', row_ver=row_ver+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND status='ACTIVE';
UPDATE farm_plan SET status='ACTIVE', row_ver=row_ver+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND id=? AND status='PENDING_APPROVAL' AND row_ver=?;
-- UNCHANGED 项从旧版本同步状态与工单（必须在旧项置 SUPERSEDED 之前）
UPDATE farm_plan_item n JOIN farm_plan_item o
    ON o.tenant_id=n.tenant_id AND o.farm_id=n.farm_id AND o.plan_id=? AND o.item_key=n.item_key
   SET n.status=o.status, n.order_id=o.order_id, n.row_ver=n.row_ver+1, n.updated_at=?
 WHERE n.tenant_id=? AND n.farm_id=? AND n.plan_id=? AND n.change_type='UNCHANGED';
UPDATE farm_plan_item SET status='SUPERSEDED', row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND plan_id=? AND status IN ('PLANNED','CONFIRMING');   -- plan_id = 旧版本
-- CHANGED / REMOVED 项旧单作废（逐单带 row_ver），随后写 transition(reason_code=PLAN_REVISED) 并把其 ORDER 票置 SUPERSEDED
UPDATE work_order SET status='CANCELED', row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND id=? AND status IN ('DRAFT','PENDING_APPROVAL','APPROVED') AND row_ver=?;
-- 生成失败 / 驳回 / 过期：本版 DRAFT 单作废（reason_code=PLAN_FAILED / PLAN_REJECTED / PLAN_EXPIRED）
SELECT id, row_ver FROM work_order WHERE tenant_id=? AND farm_id=? AND plan_id=? AND status='DRAFT';
-- 逐单确认：只扫 ACTIVE 计划的项，再条件认领
SELECT i.tenant_id, i.farm_id, i.id, i.row_ver FROM farm_plan_item i
  JOIN farm_plan p ON p.tenant_id=i.tenant_id AND p.farm_id=i.farm_id AND p.id=i.plan_id AND p.status='ACTIVE'
 WHERE i.status='PLANNED' AND i.planned_from < ? ORDER BY i.planned_from LIMIT 200;
UPDATE farm_plan_item SET status='CONFIRMING', row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND id=? AND status='PLANNED' AND row_ver=?;   -- 影响行数 0 即已被其他副本认领
-- 计划票时效扫描（PLAN 分支，沿用 P1 idx_expire）
SELECT id, tenant_id, farm_id, plan_id FROM approval_ticket WHERE ticket_kind='PLAN' AND status='OPEN' AND expires_at < ? LIMIT 200;
```

Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `lock:plan:{farm_id}` | 30s，续期 | 同农场计划生成 / 重排串行，`row_ver` 兜底 |

### 6.8 L7 调试视图、可观测性与回归门禁（第 10-11 周）

**目标**：高级能力更难解释，因此调试能力必须同步升级；回归门禁覆盖全部新增能力。

**涉及模块**：`internal/agent/harness/callbacks`（新增检索、视觉、预警、排程、计划事件类型）、统一事件流、`raw_object`（MinIO）、L8 run 详情页升级为回放视图、指标看板、`internal/evalkit/report`、CI 门禁配置。

**功能任务**

1. 统一事件流收口：各能力的事件类型（`RETRIEVAL_HIT / RETRIEVAL_FALLBACK / VISION_RESULT / EXPERT_CASE_OPENED / ALERT_EMITTED / ALERT_SUPPRESSED / SCHEDULE_CONFLICT / PLAN_REVISED / ALTERNATIVE_SELECTED`）已随 L1-L6 各自 PR 登记到 P0 L7 枚举表并上线；本 Ln 只核对全部类型已登记、payload 只记 ID 与控制字段、内容回查 `raw_object`（沿用 L7 原则）。
2. `run` 回放视图：按 `run_id` 逐步重放输入输出、检索命中、图像诊断中间结果、校验结论、审批与下发；评测 trial 与线上 run 共用同一视图。
3. 检索与加载可视化：展示命中条目、相关度、权威等级、是否最终被引用，用于定位「检索到了但没用 / 用了但没检索到」。
4. 指标看板汇总：各能力随 PR 上线的指标（`retrieval_precision`、视觉置信度分布、`alert_effective_rate`、`plan_adoption_rate` 等）汇总到统一看板；与日志统计口径对账一次并留记录。
5. 告警复核：各能力随 PR 上线的告警（RAG 回落率突增、视觉低置信度占比突增、预警关闭率突增、排程失败率突增）统一补 `run_id` 下钻链接。
6. 回归门禁复核：各能力红线样本已随 PR 进入 PR 冒烟；本 Ln 把全部新子集纳入夜间全量与发版评测，并核对每个新增红线打分器在 PR 冒烟中至少一条样本。
7. evalkit 报告链接从 run 详情页切换到回放视图。

**交付物**：回放视图与检索可视化；统一看板；对账记录；扩表后的夜间 / 发版门禁配置。

**验收**

1. 任一 run 可在 2 分钟内回放定位到出问题的具体步骤（抽 10 个历史问题 run 实测）。
2. 回归门禁覆盖全部新增能力，每个新增红线打分器在 PR 冒烟中至少有一条样本。
3. 门禁至少拦下 1 次真实回退（有记录）。
4. 看板新增指标与日志统计口径一致（对账记录入库）。

**技术实现**

流程

1. 事件注册：`agent/harness/callbacks` 只定义 `EventType` 与 `Registry.Register(t, validator)`；`service/{alert,schedule,plan,vision}` 与 `agent/knowledge` 由 `app.Build` 显式注册新类型，harness 不 import 业务包；每个类型在对应能力 PR 中同时登记 P0 L7 枚举表，未登记的类型写入被拒。
2. 新事件类型：`RETRIEVAL_HIT / RETRIEVAL_FALLBACK / VISION_RESULT / EXPERT_CASE_OPENED / ALERT_EMITTED / ALERT_SUPPRESSED / SCHEDULE_CONFLICT / PLAN_REVISED / ALTERNATIVE_SELECTED`（UPPER_SNAKE，母文档 §4.6.2-7），`payload` 只放 ID 与控制字段，正文回查 `raw_object`。
3. 回放：`GET /api/v1/farms/{farm_id}/runs/{run_id}/replay` → `controller/run` → `service/replay` 经 `ReplaySource` 接口读数据：`LiveSource` 读 `run_event`，`EvalSource` 读 MinIO `agri-steward-eval/{experiment_id}/`；前端按 `raw_id` 展开正文，取正文写 `audit_log(action=RAW_READ)`。
4. 指标：Prometheus 新增 `as_rag_retrieval_total / as_rag_fallback_total / as_vision_route_total / as_alert_total / as_schedule_conflict_total / as_plan_total` 等；业务指标由日任务 SQL 写入 P2 的 `quality_metric_daily`。
5. 门禁：PR 冒烟子集保证每个新增红线打分器至少 1 条样本；对账任务比对 `quality_metric_daily` 与 `run_event` 计数，结果入库。

SQL

```sql
-- 回放：按 seq 顺序取事件
SELECT seq, step_id, event_type, actor, payload, created_at FROM run_event
 WHERE tenant_id=? AND farm_id=? AND run_id=? ORDER BY seq;
-- 日指标：预警有效率（Gate 3 指标口径：反馈 USEFUL 或已转建议的占已推送比例），沿用 P2 quality_metric_daily 口径；走 worker 专用 idx_pushed
INSERT INTO quality_metric_daily (stat_date, source, metric, slice_key, numerator, denominator, point, created_at)
SELECT ?, 'ONLINE', 'alert_effective_rate', 'all', SUM(feedback='USEFUL' OR advice_id IS NOT NULL), COUNT(*),
       AVG(feedback='USEFUL' OR advice_id IS NOT NULL), ?
  FROM alert_event WHERE status='PUSHED' AND pushed_at >= ? AND pushed_at < ?
ON DUPLICATE KEY UPDATE numerator=VALUES(numerator), denominator=VALUES(denominator), point=VALUES(point);
-- 对账：事件计数
SELECT event_type, COUNT(*) FROM run_event WHERE tenant_id=? AND created_at >= ? AND created_at < ? GROUP BY event_type;
```

Redis 键

无。

### 6.9 L8 灰度发布、回滚预案与验收收口（第 6-14 周，与开发重叠）

**目标**：五个能力开关与 `enable_alternatives` 逐个放量，每个都能独立退回去；Gate 3 收口。

**涉及模块**：开关配置（按农场维度）、发布系统、§8 回滚预案、`evalkit` 发版评测、阶段验收材料。

**功能任务**

1. 按 `RAG → 视觉 → 主动预警 → 多方案（enable_alternatives）→ 多机排程 → 自动规划` 顺序逐项灰度，每项至少 1 周观察窗，**同一时间只放量一项**。
2. 某项能力开发完成即进入灰度，与后续能力开发并行（节奏见 §7.1）；该能力的事件、指标、回滚告警与红线样本未上线不得放量。
3. 每项放量前跑一次发版评测（全部套件含 `holdout`，n=5），门禁通过才放量。
4. 每项写明回滚触发条件与回滚步骤（见 §8），放量前完成一次回滚演练。
5. 六个开关全部开启后，再逐项关闭各跑一次全量 regression，验证 Gate 3 第 6 条。
6. 输出《Phase 3 验收报告》（按 §10 模板）与《Phase 4 启动包》（任务分解 + owner 指派 + 遗留风险）。

**交付物**：六项灰度记录与回滚演练记录；《Phase 3 验收报告》；《Phase 4 启动包》。

**验收**

1. 每项能力独立灰度记录与回滚演练齐全。
2. 逐项关闭后指标不劣于 `baseline_p2`。
3. Gate 3 六条全部通过并留证据。

**技术实现**

流程

1. 放量前门禁：`cmd/eval run --suite all --include holdout --trials 5 --compare baseline_p2`，Gate 3 相关打分器全过才放量。
2. 放量：`PUT /api/v1/admin/flags/{flag_key}`（`scope_type=FARM`，`comment` 写灰度档位）→ `service/flag` 写入校验（只拦「打开 `enable_auto_plan` 而排程未生效」）→ 乐观锁写 `feature_flag` + `audit_log(FLAG_CHANGE)` → `PUBLISH flags:changed`；顺序 `enable_rag → enable_vision_diagnose → enable_proactive_alert → enable_alternatives → enable_multi_device_schedule → enable_auto_plan`，同一时间只放量一项。
3. 演练：每项「开 → 造故障 → 关」，记录关闭到生效的时间与 regression 结果。
4. 回滚：GLOBAL 行写 `{"force_off": true}` 即可（GLOBAL 行在 L0 注册时预置为 `{"force_off": false}`），新表与新列保留不删；关闭方向不做依赖检查，可直接关 `enable_multi_device_schedule`，`enable_auto_plan` 的生效值随之为关。
5. 收口：六项全开后逐项只关一项跑全量 regression 对比 `baseline_p2`；Gate 3 通过后 `cmd/eval baseline freeze --name baseline_p3` 作为 P4 对照基线。

SQL

```sql
-- 农场级开启（首次走 INSERT，之后按 row_ver 更新，沿用 P1）；FARM 行只写 {"on": bool}
INSERT INTO feature_flag (tenant_id, farm_id, flag_key, scope_type, value, comment, updated_by, created_at, updated_at)
VALUES (?, ?, ?, 'FARM', '{"on": true}', ?, ?, ?, ?);
-- 关停某项：GLOBAL 行强制关，农场开不能覆盖（母文档 §4.6.2-5）
UPDATE feature_flag SET value='{"force_off": true}', comment=?, updated_by=?, row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=0 AND farm_id=0 AND flag_key=? AND row_ver=?;
-- 灰度面查询：某开关当前在哪些农场开启（JSON 布尔不能与整数比较，取文本比较）
SELECT tenant_id, farm_id, updated_by, updated_at, comment FROM feature_flag
 WHERE flag_key=? AND scope_type='FARM' AND value->>'$.on' = 'true';
```

Redis 键

| 键 | TTL | 用途 |
|----|-----|------|
| `flags:changed` | —（pub/sub，沿用 P1） | 放量 / 关停广播；丢消息时回落 30s 本地缓存过期 |

---

## 7. 推荐实施节奏

### 7.1 周排期

| 周 | 开发 | 灰度（L8） |
|----|------|------------|
| 第 1 周 | L0 基线冻结与开关；评测集扩容启动（标注并行至第 4 周） | — |
| 第 2-5 周 | L1 RAG 与 L2 多模态并行（两个独立小组，共用评测底座）；各自的 L4 降级路径、事件、指标、告警与红线样本同 PR | — |
| 第 6-9 周 | L3 / L5 并行，L4 对应部分同 PR | 第 6 周 RAG；第 7 周视觉；第 8 周多方案（`enable_alternatives`） |
| 第 10-11 周 | L6 自动规划开发；L7 回放视图、看板汇总与对账 | 第 10 周主动预警；第 11 周多机排程 |
| 第 12-13 周 | 缺陷修复与收口材料 | 自动规划（先 1 家农场，2 周观察窗） |
| 第 14 周 | Gate 3 验收收口 | 逐项关闭回归 |

### 7.2 并行与合入规则

1. L1 与 L2 分属两个小组，只通过 `KnowledgeSource` 与 `Citation` 契约交互，不互相 import 实现包。
2. 依赖方向不变：`internal/agent/harness` 不 import `vision / alert / schedule / plan` 任何业务包；新事件类型由业务包通过接口注册进 harness。
3. L4 的每条降级路径必须与对应能力同 PR 提交，不允许后补；该能力的事件类型（先登记 P0 L7 枚举表）、关键指标、回滚告警与红线 PR 冒烟样本同样同 PR，未齐不得进灰度；PR 审查清单增加「降级路径 + 演练用例 + 可观测项」一项。
4. 涉及契约变更（`Diagnosis`、`AdviceCard.alternatives[]`、`FarmPlan`）的 PR 先过契约评审，再合实现。
5. 红线相关改动（`output_validator`、`conflict_checker`、审批闸门、计划级确认、图像脱敏）双人评审，农艺规则相关 PR 需专家签字。
6. 一个 PR 只做一个 `P3-Ln` 的一件事；每个 PR 带单测，策略变更带 `evalkit compare` 对照。

---

## 8. 灰度、降级与回滚

通用规则：开关按农场维度生效，灰度分档沿用 Phase 1（1 → 3 → 5 个示范农场，观察窗内完成分档），自动规划例外、只开 1 家；回滚 = GLOBAL 行写 `{"force_off": true}`，不需发版；回滚后跑一次 regression 确认退回 Phase 2 形态。

| 开关 | 灰度时间 | 观察窗 | 回滚触发条件 | 回滚步骤 |
|------|----------|--------|--------------|----------|
| `enable_rag` | 第 6 周 | ≥ 1 周 | 任一可信度指标相对固定文件路径显著下降；`citation_valid` 或 `numbers_match_calc` < 100%；RAG 回落率突增 | 关 `enable_rag` → 自动回落固定文件加载 → 跑 regression → 按回放视图归因 |
| `enable_vision_diagnose` | 第 7 周 | ≥ 1 周 | 出现高置信度误诊（抽检复核判误）；低置信度或谱外未转专家；图像脱敏漏出 | 关开关 → 退回纯文本问诊 → 排查涉及 run 的作业单是否已提交审批并逐单复核 |
| `enable_alternatives` | 第 8 周 | ≥ 1 周 | 任一方案未过引用 / 数值 / 药剂校验；`conflict_blocked` 失败；采纳率低于单方案对照 | 关开关 → 新请求回到单方案；已在 `select_alternative` 中断的 run 仍可选择并照常走审批 |
| `enable_proactive_alert` | 第 10 周 | ≥ 1 周 | 关闭率超阈值；出现刷屏投诉；无依据预警被推送 | 关开关 → 扫描只推进 `alert_next_scan_at` 不出候选 → outbox 消费者发送前复查开关，已排队与顺延的预警置 `SUPPRESSED(FLAG_OFF)` → 复核频控参数 |
| `enable_multi_device_schedule` | 第 11 周 | ≥ 1 周 | 出现设备冲突或航线重叠；排程改动未下发单未换票；`approved_schedule_version` 不一致的单被下发 | 关开关（`enable_auto_plan` 生效值随之为关）→ 作业单回到单单人工排程 → 已改写未通过的单按新票走审批 |
| `enable_auto_plan` | 第 12-13 周 | 2 周（先 1 家） | 任何一次未经逐单确认的下发；计划内数值与 `calc` 不一致；同一计划项出现两张活动工单 | 关开关 → 计划功能下线，已生成计划转为只读 → 计划内未确认作业单作废 |

降级与回滚的关系：

1. 降级（L4）是单次请求级别的自动刹车，开关仍开启；回滚是能力级别的人工决策，关闭开关。
2. 同一能力一周内降级率持续高于阈值，即视为回滚触发条件，由平台与可观测小组发起回滚评审。
3. `enable_auto_plan` 依赖 `enable_multi_device_schedule`，依赖在读取时求值：关排程可直接执行，自动规划的生效值同时为关；只有「打开自动规划而排程未生效」的写入会被拒绝。
4. 任何回滚都不影响 `enable_advice` 与 Phase 1-2 已有能力。

---

## 9. 角色分工

1. **架构负责人**：契约与开关一致性（`Diagnosis` / `alternatives[]` / `FarmPlan` 契约评审）、跨路评审、依赖方向审查、Gate 3 判定。
2. **知识与检索小组**：L1 RAG 全部任务；L5 数值二次校验；知识规模扩充的工程侧。
3. **多模态小组**：L2 全部任务；图像脱敏；与农艺专家组的图集标注协同。
4. **编排与调度小组**：L3 预警与排程、L4 风险控制与降级、L6 自动规划。
5. **平台与可观测**：L7 回放视图、事件扩展、看板与告警；L8 灰度与回滚执行。
6. **农艺专家组**：图集标注、方案冲突规则（`safety.md`）、预警有效性评审、新增样本签署、5 倍规模知识集的条目署名。
7. **QA / SRE**：回归门禁扩表、红队样本补充（图像中夹带指令、预警诱导下发）、灰度与回滚演练组织。
8. **评测负责人**（Harness RD 兼任）：新增打分器评审、发版评测 transcript 阅读与签字。

---

## 10. 阶段验收模板（执行后填写）

1. **功能完成情况**：按 L0-L8 逐项勾选，未完成项写原因与负责人。

   | 路线 | 状态 | 关键交付物链接 | 备注 |
   |------|------|----------------|------|
   | L0 … L8 | 完成 / 部分 / 未完成 | | |

2. **离线评测与回归结果**：`baseline_p2` vs candidate 全指标对照表（套件、n、点估计、置信区间、差值、翻转样本数），附实验 ID 与配置快照。
3. **指标快照**：可信度六项 + 新增四项（`retrieval_precision`、视觉置信度分布、`alert_effective_rate`、`plan_adoption_rate`）+ 单会话成本与 P95。
4. **红线结果**：全部红线打分器（含 `low_conf_to_expert`、`no_silent_dispatch`、`conflict_blocked`、`plan_no_duplicate_order`、计划级 `stop_at_approval`）pass^5 结果。
5. **灰度与回滚演练结果**：五个能力开关与 `enable_alternatives` 逐项记录灰度区间、观察窗结论、回滚演练耗时与回滚后 regression 结论。
6. **Gate 3 逐条结论**：六条 + 补充硬条件，每条附证据链接；田间实测条目注明执行方。
7. **遗留问题**：问题、负责人、关闭时间；红线相关问题不允许延期。
8. **是否进入 Phase 4**：是 / 否 + 理由。

---

## 11. 本阶段风险与应对

| 风险 | 影响 | 应对 |
|------|------|------|
| RAG 引入后检索噪声导致引用错配 | 引用覆盖率表面 100%，但引用与结论不相关 | 元数据硬过滤先于向量召回；`citation_hit_rate` 与 `fact_consistency` 双看；一键回落固定文件 |
| 视觉高置信度误诊 | 错误用药，最高风险 | 低置信度强制转专家；诊断无引用不得转作业单；作业单仍经人审与硬约束两次校验；误诊即回滚 |
| 主动预警刷屏 | 用户关闭，主动服务失效 | 分级 + 频控 + 去重 + 静默时段；误报自动降频且只能人工恢复；关闭率进 Gate |
| 排程或重排静默改动已审批单 | 绕过红线二 | 关键字段变更强制 `APPROVED → PENDING_APPROVAL`；`no_silent_dispatch` 红线打分器 |
| 自动规划被理解为「批一次全执行」 | 越权执行 | 计划级审批不触发下发；逐单确认；计划级 `stop_at_approval` |
| 三路并行导致契约漂移 | 返工、下游不一致 | 契约变更先评审后实现；`evalkit` schema 与审批页同 PR 同步 |
| 新能力拉高 token 成本 | 单位经济不成立 | 预警扫描与视觉调用成本单列；成本门禁沿用 Phase 2 规则 |
| 图集与样本标注跟不上 | L2 无法验收 | L0 第 1 周锁定专家档期；标注与开发并行至第 4 周 |

---

## 12. 下一阶段交接（交给 Phase 4）

1. **知识资产**：5 倍规模知识集与 RAG 索引构建流程，作为 Phase 4 知识生命周期与专家审核工作台的起点；`expert_queue` 最小实现交给专家审核台扩展。
2. **评测资产**：图像、预警、排程、计划四个子集及打分器；知识变更后需重跑的样本清单，作为 Phase 4 知识回归集的种子。
3. **成本数据**：按能力维度（RAG / 视觉 / 预警 / 排程 / 规划）的成本基线，交给 Phase 4 成本看板与预算治理。
4. **运维资产**：四类新降级的错误码、告警与处置记录，交给 Phase 4 Runbook。
5. **扩展模板输入**：本阶段新增的模块按作物可替换的部分（病虫害图集、冲突规则、预警规则）清单，作为 Phase 4「作物包」与《新增作物接入 SOP》的输入。
6. **基线**：Gate 3 通过时的发版评测冻结为 `baseline_p3`，Phase 4 一切变更以其为对照。

---

## 13. 维护规则

1. 本文随执行更新：任务拆分、模块路径、阈值确定后回填，修改随 PR 提交并在 PR 描述中写明原因。
2. 母文档 §5.3 变更时，同步回改本文；两者冲突以母文档为准。
3. 阈值类内容（准确率目标线、置信度阈值、频控配额、关闭率阈值）确定后写明数值、确定依据与对应实验 ID，不写无来源的数字。
4. 新增开关、契约字段、打分器、指标必须先登记到本文对应章节，再进代码。
5. 红线相关条目（§3.2 第 1、6 条，§4 第 2、5 条，§8 回滚触发条件）只能加严，放宽须走 Owner 与农艺负责人双签。

