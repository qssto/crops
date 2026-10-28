# 智能农作物 Agent 管家 分阶段实施路线图（MVP → 规模化，Phase 0-4）

> 文档类型：新项目整体功能实现大纲（企业级融合路线图）
> 汇报定位：立项决策文档 —— 回答「做什么 / 分几步 / 每步多久 / 要多少人 / 风险在哪 / 何时能看到东西」
> 读者：公司决策层、项目 Owner、Agent 平台 RD、数据与物联网 RD、农艺专家、QA、SRE、法务合规
> 输入来源：①极飞科技官网公开产品线（植保无人机 / 巡田无人机 / 农业无人车 / 农机自驾仪 / 智能农场物联网 / XGEO 地图数据 / 极飞农服·农机·农场 App）；②业务诉求「按日期、季节、作物信息给出杀虫、施肥、灾害防备等指导，可调度产品机器人执行」；③Agent Harness 能力清单（MCP、护栏、HITL、记忆、长对话压缩、审计日志、可观测链路）
> 配套文档：各阶段实施路线（P0-P4），每个 `Ln` 含「技术实现」小节（流程、SQL、Redis 键）；存储与本地部署约定见 §4.6.1
> 命名占位：项目代号 `agri-steward`；核心契约 `AdviceCard`（建议卡）/ `WorkOrder`（作业单）/ `Citation`（依据引用）；命名由 Owner 拍板后全局替换

---

## 0. 一页看懂（给决策者）

**做什么**：给种植者一个「会种地的 AI 管家」。它自己知道今天是什么节气、这块地种的是什么作物、长到哪个生育期、未来三天什么天气，然后主动给出结论 —— 「稻飞虱虫口密度达到防治指标，建议今天下午 16:00 前完成喷施，药剂 X、亩用量 Y」「明天阵风 7 级，植保作业提前，同时提前关闭滴灌阀」—— 并且可以**一键把作业下发给极飞的植保无人机 / 无人车 / 智能阀门去执行**。

**为什么要做成 Agent，而不是一套规则引擎**：种植决策是「天气 × 作物 × 生育期 × 地块 × 历史作业」的高维叉乘，规则引擎只能回答人预先枚举过的问题，长尾覆盖不住也难以持续演进。Agent 的价值在于：大模型负责**编排与解释**，MCP 工具负责**取实测事实**，知识库负责**提供农艺依据**，三者分工后既能覆盖长尾，又能把「凭什么这么建议」讲清楚。

**两条不可让的红线**（本项目成败在这两条上，而不在模型跑分上）：

1. **建议必须有依据**：每条建议强制挂载「农艺知识条目 + 实测数据」两类来源（`Citation`）。取不到依据就明确回答「无法判断」并转人工，**绝不允许模型自由生成用药、用量、时机**。
2. **动机器必须人审**：任何下发到飞机 / 无人车 / 阀门的动作，一律先产出 `WorkOrder` 草稿 → 人工审批 → 才允许执行。Phase 0 连真实下发通道都不开，只跑 dry-run。

**分几步、每步能看到什么**：

| 阶段 | 名称 | 周期 | 阶段结束时可演示的成果 |
|------|------|------|------------------------|
| Phase 0 | MVP 闭环可用期 | 6 周 | 单作物 / 单区域 / 单示范农场：问一句，拿到带出处的建议 + 一张待审批作业单（dry-run，不真飞） |
| Phase 1 | 生产可用期 | 8 周 | 审批通过后**真实下发**到极飞设备；3-5 个示范农场跑通；有审计、有监控、有一键回滚 |
| Phase 2 | 质量优化期 | 8 周 | 一份评测报告：建议可信度、幻觉率、拒答正确率从 baseline 到目标线，全部有数字、有回归门禁 |
| Phase 3 | 高级能力期 | 14 周 | RAG 知识库 + 拍照识病虫害 + 主动预警（不等人问先提醒）+ 多机协同排程 + 一周农事计划（小流量灰度） |
| Phase 4 | 治理与规模化期 | 持续 | 单作物扩到 N 作物 × N 区域的工厂化流程；成本看板、专家审核台、合规闭环 |

**需要决策层现在拍板的 4 件事**（详见 §2.3）：

1. **底座模型与部署形态**：千问系列走阿里云百炼公有云 API，还是私有化部署 —— 直接牵动田块数据出域合规与单位成本。
2. **首个落地场景**：哪个作物 + 哪个区域 + 哪个示范农场做 Phase 0。建议选地块集中、设备齐全、有配合意愿的单一主粮或单一经济作物。
3. **农艺知识的权威来源与签字人**：知识条目必须有农艺专家署名背书，这是红线一的地基，没有签字人就无法开工。
4. **设备下发的开放范围与责任边界**：哪些设备允许 Agent 触发，作业事故的责任划分与保险方案。

---

## 1. 文档目的与使用方式

本文档是项目主线任务单，融合三条输入：极飞现有硬件与数据资产、种植决策业务诉求、Agent Harness 工程能力清单。

使用规则：

1. 严格按 Phase 0 → 4 顺序推进，**不跨阶段硬上复杂能力**（例如 Phase 0 不做 RAG、不做多模态识别、不开真实设备下发）。
2. 每阶段收尾按 §6「阶段门禁」过验收，门禁通过才进入下一阶段；未通过就地整改，不并行开新阶段。
3. 每个 route 固定 `目标 / 功能任务 / 验收` 三段，代码提交必须能对齐到 `Ln` 编号。
4. 术语以 §4.5「统一口径说明」为准，不允许同义漂移（例：「建议 / 方案 / 指导」三种叫法只保留 `AdviceCard` 一个）。
5. 任何优化策略都必须同时给出：baseline、candidate、指标、评测集、开关或回滚路径 —— 缺一项不进入排期。

---

## 2. 现状与输入盘点

### 2.1 可复用的存量资产（极飞侧）

本项目**不是从零造硬件**，而是在极飞已有的「感知 + 执行」底座上加一层大脑，这是本项目最大的先天优势：

1. **执行侧设备矩阵（Agent 的手脚）**：植保无人机 `P 系列`（P200 / P150 Ultra / P150 Pro / P120 / P60 Pro 等）承接喷施；农业无人车 `R 系列 / RM 系列`（R200 / R100 / RM80）承接地面作业与运输；农机自驾仪 `APC 系列` 承接耕播管收；智能农场物联网 `FBV / FGV` 智能阀、`智能施肥系统`（`FPDI` 注入式 / `FPI` 压差式）、`FSC` 控制开关系列承接水肥灌溉。
2. **感知侧数据入口（Agent 的眼睛）**：巡田无人机 `M500 / M2000` 提供田块影像与长势；`FC5` 农业智能相机提供定点连续观测；`FS3`（及存量 `FS2 / FR2`）农场网络提供物联网链路；`XGEO Map / XGEO Data` 提供地块边界与农事地理数据。
3. **用户与渠道触点**：`极飞农场`（农场管理）、`极飞农机`（机具）、`极飞农服`（农服）三个 App + 微信服务号与小程序 —— Agent 管家不必新建 App 入口，优先寄生在已有触点里。
4. **组织资产**：`极飞学园` 沉淀的培训与农艺内容体系、经销与服务网络 —— 是知识库首批内容与示范农场的现成来源。

> 说明：以上为官网公开产品线信息，**各设备是否具备可编程下发接口、接口协议与鉴权方式尚未确认**，属 Phase 0 必须闭环的对接确认项（见 §2.3）。

### 2.2 关键缺口（必须补齐）

1. **没有决策大脑**：现有产品是「人指挥机器」，缺「机器告诉人该干什么」的推理层。
2. **没有结构化农艺知识库**：农艺经验散在专家、培训材料、售后问答里，没有可被程序按「作物 × 生育期 × 病虫害 × 措施」检索的结构化条目，也没有权威分级与版本管理。
3. **没有 Agent 运行时**：MCP 工具网关、工具调用审批、脱敏、HITL 中断恢复、记忆、长对话压缩、审计日志、trace 链路 —— 七件套全部为零。
4. **没有统一数据视图**：天气、地块、作物、设备、历史作业分散在不同系统，缺 `field_id` 维度的统一聚合口径。
5. **没有可信度评估机制**：没有评测集、没有标注流程、没有幻觉率口径，导致「效果好不好」无法被讨论，只能凭感觉。
6. **没有作业安全校验层**：风速 / 降雨 / 温度 / 农药安全间隔期（PHI）/ 禁飞区 / 电量续航等硬约束，目前依赖机手经验，未沉淀为可程序化的拦截规则。

### 2.3 外部依赖与待确认项（阻塞项，按「最晚闭环」时间推进）

| 编号 | 待确认项 | 谁来定 | 最晚闭环 | 不定的后果 |
|------|----------|--------|----------|------------|
| D1 | 底座模型形态：百炼公有云 API / 私有化部署；模型档位（`Qwen3.7-Max` 级用于复杂编排、`Qwen3.6-Plus` 级用于常规问答、`Qwen3-VL` 系用于图像诊断） | 技术决策层 + 法务 | 模型形态：Phase 0 L4 开工前（第 3 周）；法务正式结论：Phase 1 开工前，期间按「脱敏后入模」最严口径开发 | 数据出域合规与成本口径都无法核算，L4 无法开工 |
| D2 | 首个作物 + 区域 + 示范农场 | 业务决策层 | Phase 0 第 1 周 | 知识库范围、评测集、田间验证全部无法收敛 |
| D3 | 农艺知识权威来源与署名专家 | 农艺负责人 | Phase 0 第 1 周 | 红线一无地基，建议不可上线 |
| D4 | 设备下发接口开放清单与协议文档 | 产品 + 设备团队 | Phase 1 开工前 | Phase 1 真实下发无法排期，只能停在 dry-run |
| D5 | 作业事故责任划分与保险方案 | 法务 + 业务 | Phase 1 灰度前 | 真实下发不敢放量，Phase 1 灰度受阻 |
| D6 | 气象数据源（自有气象站 + 第三方气象 API）与授权范围 | 数据负责人 | Phase 0 第 2 周（§9 首批任务需打通真实天气工具） | `mcp-weather` 只能走 mock，Gate 0 无法用真实问题演示 |
| D7 | Agent 框架与语言栈 | 架构负责人 | **已定**：Eino（Go），见 §4.6 | — |

---

## 3. 建设目标

### 3.1 业务目标

1. 让种植者从「自己判断该干什么」变成「确认 AI 给的建议」：单个决策（打什么药、施多少肥、什么时候浇水）的平均耗时从小时级压到分钟级。
2. 把极飞散落在设备端的能力串成闭环：**观测 → 诊断 → 建议 → 作业单 → 设备执行 → 效果回收**，形成设备之外的软件护城河与数据飞轮。
3. 用「可信 + 可审计」的建议降低农事风险：减少误用药、误施肥、错过防治窗口、恶劣天气强行作业四类高频损失。
4. 沉淀可复制的作物知识资产：新增一个作物的接入成本从「专家现场支持」降到「填模板 + 过验收清单」。

### 3.2 技术目标

1. **稳定性**：主链路会话完成率 ≥ 99%；单次建议生成 P95 目标线在 Gate 0 依 baseline 实测值确定（初步参考 ≤ 15s，不作为验收线）；LLM / MCP 任一依赖故障时有确定性降级路径，不出现整链不可用。
2. **可信**：建议引用覆盖率 100%（无依据即拒答，不允许无 `Citation` 的建议出库）；用量类数值由规则层计算，不由模型自由生成；药剂只能从规则层给出的候选集合中选择。
3. **安全**：涉及设备动作的调用 100% 经审批闸门；脱敏漏出 0 起；作业硬约束（风速 / 降雨 / PHI / 禁飞区）违规拦截率 100%。
4. **可运维**：全链路 trace 覆盖率 100%（`run_id / step_id / tool_call_id` 三级贯通）；审计日志 append-only 且可导出；任一高级能力可独立开关与回滚。
5. **成本**：单次会话 token 成本进月度看板；长对话压缩（Phase 2 落地）的降本目标线依 Phase 2 评测实测值确定。

### 3.3 建议核心指标（第一版）

**业务效果类**

1. 建议采纳率，按建议类型分别统计：作业类建议看 `AdviceCard` → `WorkOrder` 转化率；「暂不防治 / 继续观察」类建议看用户确认率，不计入转化率分母。
2. 人工改单率与驳回率（按原因分类：用量不对 / 时机不对 / 药剂不对 / 依据不足 / 其他）。
3. 单位面积农药与肥料用量变化、亩均作业成本变化。
4. 防治窗口命中率（建议时间 vs 实际最优窗口）。

**可信度类（Phase 2 核心）**

1. 引用覆盖率：带 `Citation` 的建议占比（目标 100%）。
2. 事实一致率：建议内容与引用依据一致的比例（专家标注 + LLM-judge 双轨）。
3. 数值准确率：用量 / 亩数 / 间隔期等数值与规则层计算结果一致率（目标 100%）。
4. 幻觉率：出现无依据结论的比例（目标趋零，且必须可归因到具体 run）。
5. 拒答正确率：该拒答时拒答的比例，以及不该拒答却拒答的过度保守率。

**工程与安全类**

1. 工具调用成功率、MCP 调用 P95、工具超时率与重试成功率。
2. HITL 指标：审批等待时长、审批通过率、**中断恢复成功率**（目标 ≥ 99%）。
3. 长对话压缩：压缩触发率、压缩后关键事实保留率、压缩前后成本对比。
4. 审计完整率（目标 100%）、trace 覆盖率、越权与违规作业拦截次数。

---

## 4. 功能实现大纲（全景）

### 4.1 能力分层

```
┌─────────────────────────────────────────────────────────┐
│ 7 治理层   审计日志 / trace / 指标看板 / 成本 / 知识生命周期 │
├─────────────────────────────────────────────────────────┤
│ 6 交互层   极飞农场 App / 小程序 / Web 管家页 / 主动推送     │
├─────────────────────────────────────────────────────────┤
│ 5 执行层   WorkOrder 状态机 + 设备 Adapter（P/R/APC/FBV…）  │
├─────────────────────────────────────────────────────────┤
│ 4 护栏层   工具分级审批 / 脱敏 / 输出校验 / 作业硬约束拦截    │
├─────────────────────────────────────────────────────────┤
│ 3 运行时层 Agent Harness：编排 / HITL / 记忆 / 压缩 / 重试   │
├─────────────────────────────────────────────────────────┤
│ 2 知识层   农艺知识条目（Phase 0-1 固定文件 → Phase 3 RAG）  │
├─────────────────────────────────────────────────────────┤
│ 1 接入层   MCP Servers：weather / crop / field / device    │
└─────────────────────────────────────────────────────────┘
```

1. **接入层（MCP Servers）**：`mcp-weather`（实时 + 预报 + 历史 + 灾害预警）、`mcp-crop`（作物品种、生育期物候、病虫害谱、防治指标）、`mcp-field`（地块边界、面积、土壤、当季种植档案、历史作业）、`mcp-device`（设备清单、状态、续航、作业能力）。统一经 `mcp-gateway` 注册、鉴权、限流、超时、重试。
2. **知识层**：结构化农艺知识条目，带 `权威等级 / 适用作物 / 适用生育期 / 适用区域 / 生效版本 / 署名专家`。Phase 0-1 为固定文件 + 渐进式加载，Phase 3 升级 RAG。
3. **运行时层（Harness）**：见 §4.2 七件套。
4. **护栏层**：工具按风险分级（只读 / 写业务 / 动设备）、敏感信息脱敏、模型输出结构与事实校验、作业硬约束拦截。
5. **执行层**：`WorkOrder` 状态机 + 各设备 Adapter；Phase 0 仅 mock dry-run，Phase 1 接真实通道。
6. **交互层**：优先复用 `极飞农场` App / 小程序既有触点，Phase 0 允许先用内部 Web 页验证。
7. **治理层**：审计、可观测、成本、知识生命周期、专家审核工作台。

### 4.2 Agent Harness 七件套（除长对话压缩外，MVP 必须具备最小可用版）

| # | 能力 | 职责 | Phase 0 最小实现 | 后续演进 |
|---|------|------|------------------|----------|
| 1 | **MCP 工具层** `mcp-gateway` | 工具注册、schema 声明、鉴权、超时、重试、结果标准化 | 4 个 MCP Server（weather / crop / field / device），每个工具带 mock 兜底实现 | Phase 1 接真实设备；Phase 3 增影像与遥感工具 |
| 2 | **护栏** `guardrail` | ①工具调用审批：按 `tool_policy` 分级，`只读` 直通 / `写业务` 记录 / `动设备` 不注册给模型、只能由审批后的 `WorkOrder` 状态机触发（策略层兜底拦截）；②脱敏：手机号、身份证、精确坐标、账号在入模与落日志前掩码；③输出校验：结构校验 + 引用校验 + 数值校验，不合格拒答 | 策略文件 `tool_policy.yaml` + 脱敏中间件 + 输出 schema 校验 | Phase 1 加作业硬约束拦截；Phase 2 加事实一致性校验；Phase 3 加方案冲突检测 |
| 3 | **HITL** `hitl` | 执行中断后可恢复接着执行：命中审批点 → 落 `RunState` 快照 + 开 `ApprovalTicket` → 人审 → `resume` 从断点继续，不重跑已完成步骤 | 快照落库 + `resume(run_id)` 接口 + 审批页 | Phase 1 加审批角色 / 时效 / 超时失效；Phase 3 加多步计划的分段审批 |
| 4 | **记忆** `memory` | 三层：`profile`（农场与地块长期画像）/ `episodic`（历史作业与建议结果）/ `working`（当前会话） | 农场画像 + 最近 N 次作业记录注入上下文 | Phase 2 加记忆命中度评估；Phase 4 加记忆衰减与纠错 |
| 5 | **长对话压缩** `compaction` | 超阈值自动压缩历史，保留关键事实（地块、作物、生育期、已确认结论、待办作业单） | 不做（农事问答以短会话为主）；`RunState` 预留 `compaction_state` 字段 | Phase 2 实现 token 阈值触发 + 结构化摘要 + 关键事实白名单，并做压缩前后成本与保留率评测 |
| 6 | **审计日志** `audit` | 全量 append-only：谁、什么时候、问了什么、调了哪些工具、拿到什么依据、生成什么建议、谁审批、设备执行结果 | 一张 `audit_log` 全链路落盘（只存摘要），回放所需原文存 MinIO 私有 bucket（`raw_object` 索引，预签名读取并审计），均以 `run_id` 关联 | Phase 1 加不可篡改、导出与保留期配置；Phase 4 加只追加表的归档删除与合规审计 |
| 7 | **可观测链路** `observability` | `run_id → step_id → tool_call_id` 三级 trace + 指标 + 告警 | OpenTelemetry 打点 + 基础看板 | Phase 1 加告警（含「建议无引用」告警）；Phase 3 加 run 回放调试视图 |

### 4.3 功能优先级

1. **P0**：单作物 / 单区域 / 单农场跑通「提问 → 取数（MCP）→ 加载知识 → 生成带依据的建议 → 生成作业单草稿 → 人工审批 → dry-run 执行 → 审计可查」全闭环。证明链路成立。
2. **P1**：真实设备下发 + 状态机 + 权限 + 降级 + 审计合规 + 监控告警 + 3-5 农场灰度。证明能上线。
3. **P2**：评测集 + 可信度指标 + 规则与模型职责切分 + 回归门禁 + 成本延迟门禁。证明质量可度量、可提升、不回退。
4. **P3**：RAG 知识库 + 多模态病虫害诊断 + 主动预警与排程 + 多机协同。证明能领先。
5. **P4**：作物与区域扩展工厂化 + 知识生命周期 + 专家审核台 + 成本看板 + 合规与 Runbook。证明可持续。

### 4.4 两条红线的工程落地方式

**红线一：建议必须可信，不许瞎编**

1. **职责切分**：模型只做「情境理解 + 在候选集合内选方案 + 语言解释」。**药剂候选集合**由规则层按「作物 × 防治对象 × 农药登记适用范围 × 区域」给出，模型只能从集合中选择，选出集合外的药剂即被校验层拒收；所有**数值**（亩用量、总药量、稀释倍数、安全间隔期、施肥量）由 `calc` 规则层按知识条目公式计算后回填，模型不得自行生成数值。
2. **强制引用**：`AdviceCard` 的每条结论字段必须绑定 ≥1 条 `Citation`（知识条目 ID + 版本，或实测数据来源 + 采集时间）。校验不通过的建议**不出库**。
3. **拒答优先**：知识未覆盖、数据缺失、置信度低于阈值时，统一返回「无法判断 + 缺什么 + 转人工」，并计入拒答指标。宁可拒答，不可编造。
4. **可回放**：任一条建议可按 `run_id` 完整回放当时的工具返回、加载的知识条目、模型输入输出，便于事后追责与纠正。审计表只存摘要，回放所需原文存于加密、权限受控的 `raw_store`（见 L7）。

**红线二：动机器必须人审**

1. **闸门位置**：`动设备` 类工具不注册进模型可见的工具集，模型在架构上无法发起设备动作；下发只能由 `WorkOrder` 状态机在 `APPROVED` 之后触发。护栏层的 `动设备` 拦截作为兜底：任何来自模型的设备调用一律拒绝并告警。
2. **审批物**：审批人看到的是 `WorkOrder` 完整快照（地块、作业类型、药剂与用量、时间窗、设备、预计成本、依据引用、硬约束校验结果），而非一句自然语言。
3. **硬约束两次校验**：①提交审批前校验，审批页展示风速 / 降雨 / 温度 / PHI 安全间隔期 / 禁飞区 / 设备续航的校验结论，任一不通过时**禁止提交审批**；②下发前（`APPROVED → DISPATCHED`）用实时数据**重新校验**，不通过则不下发并退回 `PENDING_APPROVAL` 重新审批。审批到下发之间条件会变，不依赖审批时的快照，也不依赖审批人记性。
4. **可中止**：已下发作业支持中止与回滚，`WorkOrder` 状态机必须包含 `CANCELED`、`PARTIALLY_DONE` 与补偿路径；中途中止时记录已作业面积，供 PHI 推算与补喷使用。

### 4.5 统一口径说明

以下术语在本文档、代码、日志、提交信息中固定含义：

1. `AdviceCard`（**建议卡**）= Agent 产出的一条结构化建议，必带 `Citation`，**不含执行动作**。
2. `WorkOrder`（**作业单**）= 由建议卡转化出的可执行作业，必经审批，状态机为 `DRAFT → PENDING_APPROVAL → APPROVED → DISPATCHED → EXECUTING → DONE`，异常分支 `REJECTED / EXPIRED / FAILED / CANCELED / PARTIALLY_DONE`。`EXPIRED` = 审批超时或已批未下发超过时效；`APPROVED → PENDING_APPROVAL` 用于下发前复检不通过或条件变化后重新审批；`PARTIALLY_DONE` = 作业中途中止，记录已作业面积。
3. `Citation`（**依据引用**）= 知识条目引用（`kb_id` + `version`）或实测数据引用（数据源 + 采集时间 + 数值）。
4. `RunState`（**运行快照**）= 一次 Agent 运行的可恢复状态，`run_id` 为全局追踪主键。
5. `ApprovalTicket`（**审批单**）= 一次人工审批请求，关联 `run_id` 与 `WorkOrder`。
6. `知识条目` = 结构化农艺知识的最小单元，带作物 / 生育期 / 区域 / 权威等级 / 版本 / 署名专家；**不等于**「文档」，一篇文档会拆成多条知识条目。
7. `渐进式加载` = 按当前问题类型与情境，只加载相关知识文件片段进上下文；**不等于** RAG（RAG 指 Phase 3 引入的向量检索）。
8. `dry-run` = 设备下发走 mock Adapter，产出完整作业指令与预期结果但**不触达真实设备**。
9. `护栏` 固定指 §4.2 第 2 项的三件事（工具审批 + 脱敏 + 输出校验），不泛指所有安全能力。
10. `tenant_id / farm_id` = 租户与农场标识，Phase 0 起贯穿全部契约与查询（Phase 0 为单值），避免 Phase 1 多农场隔离时契约返工。

### 4.6 技术选型与工程结构（D7）

**选型结论**：Agent 运行时基于 **Eino**（CloudWeGo，Go）。理由：`compose` Graph 可把主链路做成确定性流程，模型只出现在少数节点，契合红线一；ADK 原生中断 / 恢复与可插拔 checkpoint 存储，对应 HITL；eino-ext 已有 `qwen` 模型组件与 `mcp` 工具组件，对应 D1 与 L2；callbacks 切面承载 trace、审计、成本埋点。

**分层原则**（参考 pi agent 的 `pi-ai / pi-agent-core / pi-coding-agent` 三层拆分，只借鉴设计，不移植代码）：

| 层 | 对应 pi | 实现方式 |
|----|---------|----------|
| 模型层 `llm` | `pi-ai` | 直接用 Eino `ChatModel`，只做薄封装：档位配置与切换、超时重试、token 计量、脱敏 |
| 运行时层 `harness` | `pi-agent-core` + `pi-durable` | 基于 Eino ADK / compose 自建：`run_id`、`RunState` 检查点、工具分级、事件流、审计与 trace、记忆 |
| 业务层 | `pi-coding-agent` 所在的应用层 | 全新编写：建议主链路、知识加载、`calc`、护栏校验、`WorkOrder`、审批 |

**依赖方向是硬约束**：`harness` 不得 import 任何业务包；业务包通过接口把工具、策略、回调注册进 harness。红线逻辑（`calc`、`output_validator`、`constraint_checker`、状态机、审批闸门、脱敏）一律是自有确定性代码，不交给框架。

**代码分层**：业务部分按 `controller → service → repository → model` 四层组织；Agent 核心（模型、harness、主链路图、知识、`calc`、护栏）放在 `agent/` 下独立成包，由 `service` 调用。

```
agri-steward/
├── cmd/{server,worker,eval,migrate}/   # 服务、异步 worker（P1）、评测、迁移入口
├── internal/
│   ├── controller/        # HTTP handler：参数校验、鉴权、SSE、错误码映射；只调 service
│   ├── service/           # 业务编排：ask / advice / workorder / approval / audit；事务边界在这一层
│   ├── repository/        # MySQL / Redis / MinIO 读写，按表或聚合一个文件；只被 service 与 agent 的接口实现使用
│   ├── model/             # 五个契约与表结构体、状态常量、WorkOrder 迁移表（不依赖任何包）
│   ├── agent/             # Agent 核心，不 import controller / service
│   │   ├── llm/           #   模型层
│   │   ├── harness/       #   runner、checkpoint、policy、callbacks、memory
│   │   ├── graph/         #   建议主链路（compose Graph）
│   │   ├── tools/mcp/     #   MCP gateway 客户端
│   │   ├── knowledge/     #   渐进式加载器（P3 起 RAG）
│   │   ├── calc/          #   规则层数值与药剂候选集合
│   │   └── guardrail/     #   output_validator、constraint_checker、pii
│   ├── adapter/           # 设备 Adapter（mock / real），只被 workorder service 调用
│   ├── app/               # 依赖装配，server / worker / evalkit 共用
│   └── evalkit/           # 评测 harness（§4.7），生产代码不得 import
├── migrations/            # golang-migrate：0001_init.up.sql …
├── configs/  policy/  prompts/
├── deploy/docker-compose.yml   # 本地 MySQL / Redis / MinIO（§4.6.1）
├── knowledge/             # 知识条目文件（§5.0.7）
└── eval/                  # 评测样本、参考解、judge 提示词、baseline 指针
```

调用规则：`controller` 只调 `service`；`service` 调 `repository`、`agent`、`adapter`；`agent/harness` 需要的检查点、事件等存储能力以接口定义在 `agent` 内，由 `repository` 实现、在 `app` 装配注入；`model` 被所有层使用。以上规则用 `depguard` 在 CI 检查。

**从 pi 借鉴的设计**：①工具调用前后钩子作为 `policy` 与审计挂载点；②「上下文组装」与「转换为模型消息」两步分离，记忆注入与脱敏各在一步；③统一事件流，Web 页、审计、trace 都订阅它；④每步先持久化再展示、按请求 ID 幂等；⑤trace 只记 ID 与控制字段，内容回查原文存储；⑥扩展包模式 → Phase 4 的「作物包」（知识条目 + `calc` 公式 + 策略）。**不借鉴**：TUI / CLI、编码类工具、插件热加载、subagent、多模型厂商适配（YAGNI）。

**搭建方式**：不先把底座全部做完再写业务。第 1-2 周先打通一条最小链路（`runner → policy → mcp-weather 真实调用 → callbacks 落审计与 trace → 断点恢复`），用它验证 harness 接口，再逐步加入其余模块。

### 4.6.1 存储与本地部署

MySQL 8.0、Redis、对象存储都用 docker 部署（`deploy/docker-compose.yml`）。对象存储本地用 MinIO，代码只走 S3 兼容接口（`minio-go`），以后换阿里云 OSS 只改 `endpoint` 和密钥配置。

```yaml
services:
  mysql:
    image: mysql:8.0
    environment: { MYSQL_ROOT_PASSWORD: "${MYSQL_ROOT_PASSWORD}", MYSQL_DATABASE: agri_steward }
    command: --character-set-server=utf8mb4 --collation-server=utf8mb4_0900_ai_ci --default-time-zone=+00:00
    ports: ["3306:3306"]
    volumes: ["./data/mysql:/var/lib/mysql"]
  redis:
    image: redis:7.2
    command: redis-server --appendonly yes --requirepass "${REDIS_PASSWORD}"
    ports: ["6379:6379"]
    volumes: ["./data/redis:/data"]
  minio:
    image: minio/minio:RELEASE.2024-06-13T22-53-53Z
    command: server /data --console-address ":9001"
    environment: { MINIO_ROOT_USER: "${MINIO_USER}", MINIO_ROOT_PASSWORD: "${MINIO_PASSWORD}" }
    ports: ["9000:9000", "9001:9001"]
    volumes: ["./data/minio:/data"]
```

密码放 `deploy/.env`（不进 git）；服务启动时 `cmd/migrate up` 建表，并创建 bucket `agri-steward-raw`（原文、超限检查点、影像、取证包、导出）与 `agri-steward-eval`（评测产物，报告放 `report/` 前缀）；P1 增 `agri-steward-audit-anchor`（审计哈希链每日锚定，开对象锁）；P3 增 `agri-steward-milvus`（Milvus 段文件）；P4 增 `agri-steward-kb`（知识发布包）与 `agri-steward-archive`（审计归档，开对象锁；取证包本身仍走 `raw/` 路径）。

**表设计约定**

1. 主键统一 `id bigint unsigned AUTO_INCREMENT`；关联列同为 `bigint unsigned`（如 `run_id`、`order_id`）。
2. 需要按业务键去重的地方另建唯一索引，不靠主键：如 `tool_call.call_key`（`sha256(run_id|step|tool|参数|序号)` 截 32 位）、`approval_decision.request_id`、`work_order.advice_id`。
3. 所有业务表带 `tenant_id`、`farm_id`，业务索引以这两列开头；Phase 0 单值，Phase 1 起查询必须带。例外只有两类，须在建表处注释：①平台级表（`feature_flag`、`quality_metric_daily`、`attribution_issue`、`kb_index_manifest`、`kb_entry`、`kb_revision`、`kb_review`、`audit_chain_head`），平台数据 `tenant_id=0, farm_id=0`；②worker 跨农场扫描专用索引（如 `idx_expire (status, expires_at)`）及租户级共享资源索引（如 P3 `work_order.idx_device_window (tenant_id, assigned_device_id, …)`），注释「worker 专用」或「租户级」，只能经 `repository.SystemScope` 使用，depguard 限制只有 `cmd/worker` 与 server 内扫描任务可 import。
4. 时间用 `datetime(3)`，统一 UTC；可变状态表带 `row_ver int` 做乐观锁。
5. 不建外键，关联列都要有索引；嵌套结构（`reasons`、`numbers`、`payload`）用 `json` 列，需要过滤的字段单独成列。
6. `run_event`、`audit_log`、`work_order_transition`、`approval_decision`（及各阶段标注「只追加」的表）只追加：业务账号对这几张表只授 `INSERT, SELECT`。唯一允许的删除路径是保留期清理：专用运维账号 `as_retention` 在「导出到 `agri-steward-archive` → 写 manifest 并校验 → 记录哈希链续接点」之后按月 `DROP PARTITION` 或按 id 区间删除（Phase 4 L5）。
7. 按月分区的表（`audit_log`、`run_event`）由 `partition-maintainer` 任务每月 25 日 `REORGANIZE PARTITION pmax` 预建后两个月分区，`pmax` 非空即告警；迁移时预建 12 个月。
8. 迁移版本号在合入 main 时按合入顺序领取（golang-migrate 不补跑低版本）；各阶段文档中的编号按计划合入顺序排列，与周排期冲突时以合入顺序为准。

**Redis 键约定**

1. 实际键为 `as:{env}:` + 各阶段文档中的键名（文档表格省略前缀，由 `repository/rediskey` 统一拼接）；评测 trial 的 `env` 取 `eval-t{n}`，与临时 schema `agri_eval_t_{n}` 编号一致，trial 结束按前缀删除。
2. 每个键必须设 TTL；pub/sub 频道（`flags:changed`、`kb:changed`、`sse:run:*`）不存数据；消息体可追加可选字段，消费方忽略未知字段，不受此限。
3. Redis 只做加速与协调，丢失后以 MySQL 为准：任何「是否已做过」的判断最终落在 MySQL 唯一键上，Redis 未命中或丢失时回查 MySQL；MySQL 也没有记录即视为没做过，可以重做。
4. 分布式锁：`SET key {token} NX PX {ttl}`，释放与续期都用 Lua 比对 token；持锁期间要做外部 IO（MCP、Adapter、MinIO）的必须按 `ttl/3` 续期，否则把 IO 移到锁外。锁只由外层 service 持有，`WorkOrderService.Transition` 等内层入口不取锁、只靠 `row_ver` 乐观锁。
5. 幂等占位：`SET idem:* PENDING:{token} NX PX {操作超时 × 重试次数}`；业务失败或事务回滚时按 token 删除；事务提交后改写为 `DONE:{结果 id}` 并设长 TTL；只对成功结果去重返回，失败结果不去重。

各阶段用到的键名写在各阶段路线的「技术实现」小节。

### 4.6.2 跨阶段公共约定

各阶段文档引用以下约定，不各自另写一套。

1. **鉴权与 Scope**：资源路径统一 `/api/v1/farms/{farm_id}/...`，请求体中的 `farm_id` 一律忽略；`controller` 中间件解析身份得到 `Scope{tenant_id, farm_id, user_id, roles}` 传入 service。Phase 0 用配置文件静态「用户 → 农场 → 角色」表 + HS256 签名 token；Phase 1 起改查 `user_role`。平台角色记为 `(tenant_id=0, farm_id=0, role=platform_admin)`，GLOBAL 开关只允许平台角色修改。设备回执无用户身份，按 `device_job_id → dispatch_attempt` 反查 Scope 并校验与路径一致。
2. **下发路径**：会触达设备的调用只在 `service/workorder` 中发生，不经过 `agent` 包。审批通过时同事务写 `outbox(order.dispatch)`；Phase 0 由 server 内 outbox 轮询器执行，Phase 1 起迁到 `cmd/worker`。审批后 run 恢复只做收尾（通知、摘要、置 `COMPLETED`），不再下发。
3. **下发器唯一顺序**：①校验工单状态与 `dispatch_deadline_at` → ②读开关（`BoolLive`），关则 `DISPATCH_HELD` 保持 `APPROVED` → ③设备在线检查（Phase 1），离线推后 → ④下发前复检 `PRE_DISPATCH`：时效数据以新 epoch 重取、不读缓存，结论变化 → `CANCELED(CONDITION_CHANGED)` 并新建 run；硬约束不通过（Phase 1）→ `APPROVED → PENDING_APPROVAL` 换票重审 → ⑤`recordIntent` → ⑥调 Adapter → ⑦`recordResult`。
4. **outbox 去重键**：`{topic}:{order_id}:{ticket_id}:{n}`；通知类 `notify:{kind}:{ref_id}:{ticket_id}` 用 `INSERT IGNORE`，不得让通知失败回滚主事务。
5. **开关取值**：`feature_flag.value` 只有三种形态：布尔 `{"on":bool}`、三态 `{"mode":"off|shadow|on"}`、强制关 `{"force_off":bool}`。GLOBAL / TENANT 行只允许 `force_off`；`on` / `mode` 只在 FARM 行生效，没有 FARM 行取注册表默认值（默认关）。生效值 = `!force_off(GLOBAL) && !force_off(TENANT) && farm 值`。关停与恢复只改 `force_off`，不会把未灰度农场带开。预算降级等系统覆盖用独立开关 `budget_hold.<flag>` 叠加：取 `{"on":bool}`，可写在 FARM / TENANT 行，任一层 `on=true` 即暂停对应能力（EMERGENCY 级预警不受其影响），不改写人工设置的行。
6. **审批票据**：`approval_ticket.ticket_kind` 自 Phase 0 建表即有（`ORDER`，Phase 3 增 `PLAN`），所有按状态扫描、列表、决策的 SQL 都带 `ticket_kind`；隐私与取证类治理工单不进此表，Phase 4 新建 `governance_ticket`。
7. **事件类型**：`run_event.event_type` 一律大写下划线（`TOOL_CALL_END`、`VALIDATION_FAILED`），枚举表在 P0 L7 维护，新增类型先登记；离线评测、judge、专家结果不写 `run_event`。
8. **对象 key**：原文类对象（含压缩原文、影像、取证包、导出）统一走 `RawStore.Put`，key 为 `raw/{tenant}/{farm}/{yyyy}/{mm}/{dd}/{run_id 或 0}/{kind}/{id}`，并登记 `raw_object`；报告类产物放 `agri-steward-eval` 的 `report/` 前缀；客户端直传的临时对象放 `incoming/{tenant}/{farm}/{upload_id}`（1 天生命周期），校验后再经 `RawStore.Put` 转入 `raw/`；不同环境用不同 MinIO 部署，key 不带 env。
9. **时区**：存储一律 UTC；按日计数、静默时段、节气、计划日期按 `farm_profile.timezone`（默认 `Asia/Shanghai`）换算成农场本地日期。
10. **红线资产**：`internal/agent/calc`、`internal/agent/guardrail`、`policy/constraint_rules.yaml`、`croppacks/**` 的参数与阈值文件，改动需规则 owner + 农艺专家双签（CODEOWNERS 必需评审）并触发对应回归；红线组件只按 `kb_id@version` 精确读取知识，不走检索；已合入的拒收规则常开，不受开关控制。
11. **`WorkOrder` 合法迁移表**（P0 L1 冻结，各阶段只用不改）：

| from | 允许的 to |
|------|-----------|
| `DRAFT` | `PENDING_APPROVAL`、`CANCELED` |
| `PENDING_APPROVAL` | `APPROVED`、`REJECTED`、`EXPIRED`、`CANCELED` |
| `APPROVED` | `DISPATCHED`、`PENDING_APPROVAL`（复检退回 / 排程变更）、`EXPIRED`（超过 `dispatch_deadline_at`）、`CANCELED` |
| `DISPATCHED` | `EXECUTING`、`DONE`、`PARTIALLY_DONE`、`FAILED`、`CANCELED`（允许跳过 `EXECUTING`，处理乱序回执） |
| `EXECUTING` | `DONE`、`PARTIALLY_DONE`、`FAILED`、`CANCELED` |

`DONE / PARTIALLY_DONE / REJECTED / EXPIRED / FAILED / CANCELED` 为终态。下发失败只记在 `dispatch_attempt`，工单保持 `DISPATCHED`，最后一次尝试确认未执行才转 `FAILED`；无回执不转终态，标记待对账由人工确认。dry-run 单同样走到 `DONE`，以 `dispatch_mode=DRY_RUN` 区分，不写入作业记忆、不参与 PHI 推算与作业指标。
12. **已定的设计前提**（原评审待确认项，按最保守口径定，外部事实与此不符时只需放宽，不需返工）：
   - Eino 检查点按「只在中断时写入」设计：非中断点崩溃即整图重跑，所有带副作用的节点靠业务唯一键幂等；P0 L0 用实际 Eino 版本做一次验证。
   - 不依赖设备平台回写 `field.history`：`DONE / PARTIALLY_DONE` 由我方经 outbox 按 `order_id` 幂等回写；PHI 与重复作业检查取「本地在途及 PHI 窗口内完成的工单 ∪ `field.history`」。
   - 设备按租户级共享处理：排程互斥锁与冲突查询按 `(tenant_id, device_id)`；农场独占设备是其特例。
   - 隐私主体是用户（`user_id`），`agent_run` 自 P0 起记 `user_id`；按 `user_id` 删除个人数据，农场注销时按 `farm_id` 删除；`audit_log.actor_id` 保留，由外部账号体系去标识化。
   - 取证包为加密原文版（按接收方公钥加密），需法务 + 平台管理员双人审批；只有清单与索引过 `pii_clean`。最终以法务结论为准。
   - Milvus 用 2.5.x（原生 BM25 与中文分词），镜像固定到补丁版本。
   - HIGH 风险二次确认由第二审批人完成：第二人 ≠ 第一人，且同在 `required_role` 内；审批人 ≠ 发起人。

**表清单（按引入阶段）**

| 阶段 | 表 |
|------|----|
| P0 | `agent_run`、`run_step`、`run_checkpoint`、`run_event`、`tool_call`、`advice_card`、`advice_citation`、`work_order`、`work_order_transition`、`constraint_check`、`approval_ticket`、`approval_decision`、`farm_profile`、`field_profile`、`episodic_memory`、`audit_log`、`raw_object`、`outbox` |
| P1 | `dispatch_attempt`、`device_receipt`、`feature_flag`、`user_role`、`rejection_reason`、`audit_chain_head` |
| P2 | `kb_load_item`、`rule_shadow_diff`、`advice_feedback`、`attribution_issue`、`quality_metric_daily`、`online_judge_result` |
| P3 | `alert_event`、`alert_quota`、`farm_plan`、`farm_plan_item`、`diagnosis`、`expert_case`、`kb_index_manifest`；向量库（Milvus docker，P3 L1 引入） |
| P4 | `kb_entry`、`kb_revision`、`kb_review`、`cost_daily`、`governance_ticket` |

各表的字段与 SQL 写在引入它的阶段路线中。

**评测库**：同一 MySQL 实例上的独立库 `agri_eval`，迁移放 `migrations/eval/`（`cmd/migrate --target eval`）；每个 trial 的业务数据写临时 schema `agri_eval_t_{n}`（跑同一套业务 migrations，结束即回收进 schema 池），不用 SQLite，保证评测跑的是生产 SQL。P0 L1 建以下两张基础表，P2 起各阶段只加列或加表：

```sql
CREATE TABLE experiment (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY,
  suite varchar(32) NOT NULL, trigger_kind varchar(16) NOT NULL,  -- pr / nightly / release / manual
  git_sha char(40) NOT NULL, report_key varchar(255) NULL,       -- MinIO agri-steward-eval 下的对象 key
  status varchar(16) NOT NULL DEFAULT 'RUNNING',                 -- RUNNING / DONE（P2 L7 增 BUDGET_EXCEEDED / VOID）
  started_at datetime(3) NOT NULL, finished_at datetime(3) NULL
);
CREATE TABLE trial (
  id bigint unsigned AUTO_INCREMENT PRIMARY KEY,
  experiment_id bigint unsigned NOT NULL, sample_id varchar(64) NOT NULL, trial_index smallint NOT NULL,
  suite varchar(32) NOT NULL, run_id bigint unsigned NULL,
  status varchar(16) NOT NULL,  -- PASS / FAIL / INFRA_ERROR / GRADER_ERROR
  passed tinyint(1) NOT NULL DEFAULT 0, scores json NULL, latency_ms int NULL,
  created_at datetime(3) NOT NULL,
  UNIQUE KEY uk_trial (experiment_id, sample_id, trial_index)  -- resume 续跑去重
);
```

### 4.7 评测 Harness

详细设计见《智能农作物 Agent 管家 评测 Harness 设计方案》，这里只列要点与对生产代码的要求。

1. **自建 `evalkit`（Go，同仓）**，在进程内调用生产 harness 与建议主链路，不另写评测版 agent；`cmd/server` 与 `evalkit` 共用 `internal/app` 的装配逻辑，只替换依赖实现。
2. **生产代码必须预留注入接缝（L1 / L2 落实）**：`Clock`、`ToolProvider`、`DeviceAdapter`、存储、`KnowledgeSource`、`MemoryStore`、`UserChannel`、`Approver` 全部以接口注入。评测时分别替换为冻结时钟、按样本 fixture 返回的 MCP、只记录不执行的设备 Adapter、每个 trial 独立的存储、指定版本的知识库、脚本用户与脚本审批人。`ChatModel` 与生产同配置，不打桩。
3. **打分以代码为主**：两条红线全部由确定性的代码打分器与轨迹断言判定（引用有效、数值等于 `calc`、药剂在候选集合内、审批前设备调用数为 0、`KB_MISS` 后拒答、脱敏、状态机合法）。LLM-judge 只评事实一致性、结论一致性、解释质量，与专家校准达标前不进门禁。
4. **多次运行**：每个样本跑 n 次（PR 冒烟 n=3，夜间与发版 n=5）。红线按 pass^k 判定（k 次全过才算过），质量指标报均值与置信区间，baseline 与 candidate 按样本配对比较并列出翻转样本。
5. **套件**：`regression`（门禁）、`capability`（攻坚，稳定通过后毕业进 regression）、`redteam`（越权、诱导编造、工具返回中的提示词注入）、`holdout`（不对调提示词的人公开，防过拟合）。
6. **开发节奏：不单独排期，随层交付。** evalkit 不等管家做完再开发，也不作为独立项目排期。每一层交付时同时交付本层的打分器与样本，作为该层验收的一部分。下表的评测任务已写进各层的功能任务与验收。

| 阶段 / 层 | 管家交付 | 同期交付的评测能力 | 评测侧验收 |
|-----------|----------|--------------------|------------|
| P0 · L0（第 1-2 周） | 场景冻结、问题域 | 样本 YAML 格式与静态校验、`cmd/eval` 骨架、20 条样例 → 100 条 | `make eval` 对空实现出报告 |
| P0 · L1 | 五个契约 | 注入接缝接口、共用 `app.Build` | evalkit 能用评测依赖装配出主链路 |
| P0 · L2 | MCP 接入 | `FixtureMCP`（含故障注入、按调用次序返回） | 三类故障用例由样本驱动 |
| P0 · L3 | 知识库、`calc` | `citation_valid`、样本 `must_cite` / `product_in` 的加载期校验 | 引用了不存在的知识版本被判失败 |
| P0 · L4 | 主链路 | `schema_valid`、`numbers_match_calc`、`product_in_candidates`、`outcome_match`、`must_call` / `budget` | 20 个问题由 evalkit 回放并打分 |
| P0 · L5 | 护栏 | `no_model_actuate`、`stop_at_approval`、`pii_clean`、`refusal_correct`、`kb_miss_refuse`、`RecordingAdapter` | L5 验收 2、3、4 条改由样本自动判定 |
| P0 · L6 | HITL 与记忆 | 脚本审批人（通过 / 驳回 / 超时）、`FrozenClock.Advance()`、`state_machine_legal` | 恢复与时效数据重取用例由样本驱动 |
| P0 · L7 | 审计与 trace | 统一事件流导出为 transcript，报告链接到 `run_id` | 任一 trial 可从报告跳到 run 详情 |
| P0 · L8 | Gate 0 收口 | 多次运行（n=3）与 pass^k、报告、首个 baseline | Gate 0 第 1、2、5、6 条由 evalkit 判定 |
| Phase 1 | 真实下发、审批体系 | `redteam` 套件；下发前复检、`EXPIRED`、`PARTIALLY_DONE`、跨农场隔离的打分器 | 红队样本红线 pass^5 = 1 |
| Phase 2 | 质量优化 | LLM-judge 与校准、LLM 模拟用户、`holdout`、PR / 夜间 CI 门禁、配对对比、线上回流 | judge 校准达标；`make eval` 进 CI 并能阻断 |
| Phase 3 | 高级能力 | 图像子集、主动预警样本（`Clock.Advance()`）、RAG 召回打分器 | 每项新能力有独立套件与 baseline |
| Phase 4 | 规模化 | 知识回归集，知识变更自动触发 | 知识变更 PR 自动跑并阻断 |

**分工**：Phase 0 由 Harness RD 中 1 人兼任评测负责人（约 3.5 人周，摊在 6 周内），QA 负责样本工程化与报告解读，农艺专家从第 1 周起按样本格式写样本。打分器由开发该层的人写，评测负责人评审，保证「写功能的人同时写验收」。

---

## 5. 清晰实现路线（阶段化执行）

## Phase 0（P0）MVP 闭环可用期 —— 6 周

**目标**：让「提问 → MCP 取数 → 渐进式加载知识 → 生成带依据的建议 → 转作业单草稿 → 人工审批 → dry-run 执行 → 审计与 trace 可查」这一条完整链路在**一个作物、一个区域、一个示范农场**上真实跑通。

### 5.0.1 范围边界

**本阶段必须完成**

1. Agent Harness 七件套的最小可用版（§4.2 表格「Phase 0 最小实现」列全部落地）。
2. 4 个 MCP Server 接入（真实数据源优先，未就绪的用 mock 顶上但必须接口一致）。
3. 固定文件知识库 + 渐进式加载，覆盖首个作物的「施肥 / 植保 / 灌溉 / 灾害防备」四类问题。
4. `AdviceCard` / `WorkOrder` / `Citation` / `RunState` / `ApprovalTicket` 五个核心契约冻结。
5. 100 条评测问题 + 农艺专家标准答案，作为后续所有阶段的对照底座。
6. 一个能演示的最小交互界面（内部 Web 页即可）。

**本阶段明确不做**

1. 不做 RAG、不做向量检索（知识只用固定文件 + 渐进式加载）。
2. 不做真实设备下发（只 dry-run），不做设备故障处理与作业补偿。
3. 不做多模态图像诊断、不做主动预警推送、不做多机协同排程。
4. 不做多租户与精细权限（单农场单角色即可），不做多作物 / 多区域扩展。
5. 不做模型微调，不做自建推理集群（走百炼 API，形态待 D1 决策）。
6. 不做长对话压缩（农事问答以短会话为主，Phase 2 再做），`RunState` 仅预留字段。
7. 不追求建议准确率指标达标 —— Phase 0 只证明链路成立与依据可追溯，准确率优化是 Phase 2 的事。

### 5.0.2 目标与通过标准（Gate 0）

1. 演示 20 个真实问题，**100% 产出带 `Citation` 的建议或明确拒答**，无一条无依据结论。
2. 涉及设备动作的请求 **100% 被护栏拦下并转审批**，无一次绕过。
3. 审批点中断后 `resume(run_id)` 恢复成功率 ≥ 95%，且不重复执行已完成步骤。
4. 每个 run 可在审计日志与 trace 中完整还原（工具调用、知识加载、模型输入输出、审批人、执行结果）。
5. 脱敏用例全通过：日志与模型输入中无手机号、身份证、精确坐标明文。
6. 100 条评测集跑出 baseline 数据（不设达标线，只作为后续对照）。
7. 首个示范农场至少完成 1 次「建议 → 审批 → dry-run」真人参与的端到端演练，并留下书面反馈。

第 1、2、5、6 条由 `evalkit` 自动判定：红线类样本按 pass^3 计（每条跑 3 次全部通过才算通过），报告附各 trial 的 `run_id`。

### 5.0.3 实现路线总览（L0 → L8）

1. `L0`：场景冻结与评测底座
2. `L1`：核心契约与数据模型冻结
3. `L2`：MCP 数据接入层
4. `L3`：固定知识库与渐进式加载
5. `L4`：Agent 主链路与建议生成
6. `L5`：护栏层（审批 / 脱敏 / 输出校验）
7. `L6`：HITL 中断恢复 + 记忆
8. `L7`：审计日志与可观测链路
9. `L8`：最小交互界面 + 示范农场冒烟 + Gate 0 收口

建议顺序：`L0 → L1 → L2 + L3（并行）→ L4 → L5 + L6（并行）→ L7 → L8`

### 5.0.4 L0 场景冻结与评测底座

**目标**：把「智能管家」这个大词收敛成一个可验收的具体场景，并同步建立唯一的效果对照基线。

**功能任务**

1. 冻结 D2：确定作物、区域、示范农场、目标用户角色（农场主 / 农艺师 / 机手），写成一页《场景说明书》。
2. 冻结问题域：四类问题各挑高频子类，形成 `question_taxonomy`（如植保类 → 病虫害识别 / 防治时机 / 药剂选择 / 用量计算）。
3. 建 100 条评测集，一个样本一个 YAML 文件，放在 `eval/suites/regression/`（格式见评测设计方案 §5），字段：`问题、情境（日期+地块+生育期+天气）、期望结论、期望引用来源、是否应拒答`，另加每个样本的 fixture（冻结时间、各工具返回值、知识版本）与轨迹断言（必须调用 / 禁止调用 / 必须停在审批点）；其中**至少 20 条为「应拒答」负样本**（知识未覆盖、数据缺失、超出作物范围）。
4. 农艺专家逐条签署标准答案，形成可追责的标注记录。
5. 搭 `evalkit` 骨架：样本加载与静态校验（`cmd/eval validate`）、单样本多次运行、报告生成。

**验收**

1. 《场景说明书》与 `question_taxonomy` 经业务与农艺双方书面确认。
2. 100 条评测集入库，其中应拒答样本 ≥ 20 条，全部有专家署名，全部通过静态校验。
3. `make eval` 可一键加载并回放评测集，对空实现生成报告（不要求分数）。

### 5.0.5 L1 核心契约与数据模型冻结

**目标**：先把数据契约钉死，避免后续各层互相返工 —— 这是并行开发的前置条件。

**功能任务**

1. 定义 `AdviceCard`：`advice_id / run_id / tenant_id / farm_id / field_id / crop / phenology_stage / category（植保·施肥·灌溉·防灾）/ advice_type（作业类·观察类）/ conclusion / reasons[] / product_candidates[]（规则层给出的药剂候选集合）/ selected_product / numbers[]（由规则层计算）/ citations[] / confidence / suggested_window / next_action`。
2. 定义 `WorkOrder`：`order_id / advice_id / tenant_id / farm_id / field_id / work_type / device_type / payload（药剂·用量·航线·阀门开度等）/ time_window / constraint_checks[]（含审批前与下发前两次校验结果）/ status / approver / approval_expires_at / dispatch_result / done_area`。
3. 定义 `Citation`：`type（kb / measured）/ kb_id + version / data_source + collected_at + value`。
4. 定义 `RunState`：`run_id / tenant_id / farm_id / steps[] / completed_step_ids / pending_approval / context_snapshot / compaction_state（Phase 2 启用）/ created_at`。
5. 定义 `ApprovalTicket`：`ticket_id / run_id / tenant_id / farm_id / order_id / risk_level / snapshot / status / operator / expires_at / decided_at / reason`。
6. 建立 `WorkOrder` 状态机图与合法迁移表（含 `REJECTED / EXPIRED / FAILED / CANCELED / PARTIALLY_DONE` 分支及 `APPROVED → PENDING_APPROVAL` 复审迁移，见 §4.5），落成文档 + 代码常量。Phase 1 才用到的状态在 Phase 0 一并定义，避免冻结后返工。
7. 定义评测注入接缝（§4.7-2）：`Clock`、`DeviceAdapter`、存储、`KnowledgeSource`、`MemoryStore`、`UserChannel`、`Approver` 以接口形式注入 harness，生产代码不直接读系统时间、不直接连库。

**验收**

1. 五个契约以 schema 形式落库（JSON Schema 或等价定义），代码与文档一致。
2. 状态机非法迁移有单测覆盖，非法迁移一律报错而非静默通过。
3. 契约变更走版本号，冻结后变更需 Owner 批准并同步所有下游。
4. `cmd/server` 与 `evalkit` 通过同一个 `app.Build` 装配，只替换依赖实现。

### 5.0.6 L2 MCP 数据接入层

**目标**：让 Agent 的每个事实都来自工具返回，而不是模型记忆。

**功能任务**

1. 搭 `mcp-gateway`：工具注册表、schema 声明、鉴权、超时（默认 3s）、重试（幂等只读工具重试 2 次）、结果标准化与错误码归一。
2. `mcp-weather`：实时天气、3-7 日预报、历史同期、灾害预警；输出统一含 `风速 / 降雨量 / 温度 / 湿度 / 数据时间 / 数据源`。
3. `mcp-crop`：作物品种、生育期与物候判定（按播种期 + 积温 + 日期推算当前生育期）、病虫害谱、防治指标阈值。
4. `mcp-field`：地块边界与面积、土壤基础属性、当季种植档案、历史作业记录（近 N 次施药施肥灌溉）。
5. `mcp-device`：可用设备清单、在线状态、续航、作业能力（载药量 / 幅宽 / 作业效率）—— Phase 0 允许全 mock。
6. 每个工具提供 `mock` 与 `real` 双实现，用配置切换；未就绪数据源先走 mock，但**接口与字段必须与真实一致**，避免 Phase 1 返工。评测另有第三种实现 `FixtureMCP`：按样本 fixture 返回，样本未声明的调用一律报错并记录。
7. 工具返回统一带 `data_freshness`（数据新鲜度）与 `source`，供 `Citation` 与拒答判断使用。

**验收**

1. 四个 MCP Server 全部可被 Agent 发现并调用，schema 自描述完整。
2. 工具超时、返回空、返回异常三类故障各有单测，且失败时**不产生凭空建议**（走拒答路径）。
3. 每次工具调用在 trace 中可见 `tool_call_id / 入参 / 出参摘要 / 耗时 / 数据源`。
4. `mock → real` 切换只改配置，不改业务代码。
5. 三类故障用例另各有一条评测样本，由 `FixtureMCP` 的故障注入驱动；样本未声明的工具调用被拒绝并记录 `unexpected_call`。

### 5.0.7 L3 固定知识库与渐进式加载

**目标**：先用「写死的文件 + 按需加载」把知识层跑通，把 RAG 留到 Phase 3，避免一上来就背检索质量的债。

**功能任务**

1. 设计知识目录结构，按检索维度分文件，便于按问题精准加载：

```
knowledge/
├── index.yaml                      # 知识索引：条目 → 文件 → 适用维度，加载器只读这一份
├── <crop>/
│   ├── phenology.md                # 生育期与物候判定
│   ├── fertilization.md            # 施肥方案与用量公式
│   ├── irrigation.md               # 灌溉策略与阀门控制
│   ├── pest/<pest_name>.md         # 一病一虫一文件：识别特征 / 防治指标 / 药剂 / 用量 / PHI
│   └── disaster.md                 # 台风·暴雨·低温·高温·干旱防备措施
└── common/
    ├── operation_window.md         # 作业窗口硬约束（风速·降雨·温度阈值）
    ├── safety.md                   # 农药安全间隔期·混用禁忌·人员防护
    └── pesticide_registry.yaml     # 农药登记信息：适用作物·防治对象·登记用量范围·PHI
```

2. 知识条目统一 front-matter：`kb_id / crop / stage / region / category / authority_level（国标·地标·企业标准·专家经验）/ version / author / effective_from`。
3. 实现 `progressive_loader`：按 `question_taxonomy` 分类结果 + 情境（作物 / 生育期 / 问题类型）从 `index.yaml` 选片段加载，**单次注入上下文的知识片段数设上限**（建议 ≤ 5 条），避免上下文膨胀与无关知识干扰。
4. 实现 `calc` 规则层：把知识条目里的用量公式落成可执行函数（亩用量 → 总用量、稀释倍数、PHI 推算作业日），供 §4.4 数值回填使用；同时按「作物 × 防治对象 × 农药登记适用范围 × 区域」输出药剂候选集合 `product_candidates`。
5. 知识缺失显式化：加载器命中 0 条时返回 `KB_MISS`，主链路据此走拒答，而**不允许**回落到模型自身记忆。
6. **评测（§4.7-6）**：实现 `KnowledgeSource` 按 git 提交号加载指定知识版本；实现 `citation_valid` 打分器；样本静态校验补上「`must_cite` 在指定知识版本中存在」「`product_in` ⊆ `calc` 给出的候选集合」两条。

**验收**

1. 首个作物四类问题的知识条目齐备，每条有 `authority_level` 与署名专家。
2. 100 条评测集中每条问题的加载结果可打印，人工抽查 20 条确认「加载到的知识确实相关」。
3. `calc` 层用量计算有单测，与专家手算结果一致；药剂候选集合与农药登记信息一致，不含未登记于该作物的药剂。
4. 制造一条知识未覆盖的问题，链路稳定走 `KB_MISS` → 拒答，不出现编造。
5. `citation_valid` 有表驱动单测；100 条样本全部通过新增的两条静态校验。

### 5.0.8 L4 Agent 主链路与建议生成

**目标**：把编排跑起来 —— 模型负责理解与选择，事实来自工具，依据来自知识，数值来自规则层。

**功能任务**

1. 接入千问底座（形态依 D1）：通过 Eino `qwen` 组件接入，主链路用 `compose` Graph 固定编排（见 §4.6），模型只出现在「理解与分类 / 方案选择 / 解释生成」三个节点；模型档位与参数（温度、max_tokens、超时）写进配置，可一键切换与回滚。
2. 实现主链路编排 `run(question, context)`：
   `理解与分类 → 情境补全（MCP 取地块·作物·生育期·天气）→ 知识加载 → 规则层给出药剂候选集合 → 模型在集合内选方案 → 规则层数值计算 → 组装 AdviceCard → 引用、药剂与数值校验 → 落库`。
3. 系统提示词分层管理（角色 / 边界 / 输出格式 / 拒答规则），提示词纳入版本管理，变更可 diff、可回滚。
4. `AdviceCard → WorkOrder(DRAFT)` 转换器：把建议中的执行动作映射为设备作业参数。
5. 建议 → 作业单**不自动推进**：草稿生成即停，等 L5 审批。
6. 降级路径：LLM 不可用时，对「防治指标已明确 + 知识条目已命中」的场景，用规则层直出保守建议并标注 `degraded=true`；无法降级则明确报不可用。
7. **评测（§4.7-6）**：实现 `schema_valid`、`numbers_match_calc`（按字段约定精度比较）、`product_in_candidates`、`outcome_match`（忽略 ID 与时间戳）、`must_call` / `budget` 打分器；evalkit 能对单个样本跑一次完整主链路并出报告。

**验收**

1. 20 个真实问题端到端跑通，产出结构合法的 `AdviceCard`；由 evalkit 回放并打分，不靠手工检查。
2. 所有数值字段可追溯到 `calc` 层的计算过程，模型输出中的裸数值被校验层拒收。
3. 提示词与模型配置变更可通过配置回滚，不需改代码发版。
4. LLM 主动断连演练：链路按降级路径返回，不抛未捕获异常、不产生无依据建议。
5. 本层打分器全部有表驱动单测；20 条样例样本的参考解能通过全部打分器（`cmd/eval validate`）。

### 5.0.9 L5 护栏层（审批 / 脱敏 / 输出校验）

**目标**：把红线做成代码，而不是做成规范文档。

**功能任务**

1. `tool_policy.yaml` 工具分级：`read`（天气·作物·地块查询，直通）/ `write`（写业务数据，记录 + 留痕）/ `actuate`（下发设备，**不注册给模型**，只允许 `workorder` 状态机在 `APPROVED` 后调用；策略层对来自模型的调用兜底拒绝）。策略文件与工具注册表校验绑定，新增工具未声明等级则**启动失败**，不允许默认放行。
2. 审批闸门：主链路生成 `WorkOrder(DRAFT)` 后中断，产出 `ApprovalTicket` + `RunState` 快照，返回「待审批」而不是继续执行。
3. 脱敏中间件 `pii_masking`：手机号、身份证、账号、精确经纬度在①入模前②落日志前双向掩码；地块坐标入模时降精度为地块级标识。
4. 输出校验 `output_validator` 三层：
   - 结构校验：`AdviceCard` schema 合法性；
   - 引用校验：每条结论字段至少一条 `Citation`，`kb_id` 必须存在且版本有效；
   - 数值校验：建议中所有数值与 `calc` 层结果逐一比对，不一致即拒收并记录；
   - 药剂校验：`selected_product` 必须属于 `product_candidates`，集合外药剂一律拒收。
5. 校验失败处置：不修不补、不重试硬凑，直接转拒答 + 落 `validation_failed` 事件，供 Phase 2 归因。
6. **评测（§4.7-6）**：实现 `RecordingAdapter`（只记录不执行），以及 `no_model_actuate`、`stop_at_approval`、`pii_clean`、`refusal_correct`、`kb_miss_refuse` 打分器；补齐对应样本（设备动作请求、应拒答、含敏感信息的输入）。

**验收**

1. 新增一个未声明等级的工具，服务启动失败（有明确报错）。
2. 构造「帮我把这块地打药」类请求，100% 停在待审批，审批前设备 Adapter 无任何调用（由 `stop_at_approval` 基于 `RecordingAdapter` 判定）；启动自检断言模型可见工具集中不含任何 `actuate` 工具（由 `no_model_actuate` 判定）。
3. 脱敏用例集全通过，日志与模型入参中无敏感明文（由 `pii_clean` 判定）。
4. 人为注入一条无引用建议、一条数值篡改建议、一条集合外药剂建议，均被校验层拒收并落事件。
5. 本层打分器全部有表驱动单测，且每个打分器至少有一条「应失败」的单测用例，证明它能抓到违规。

### 5.0.10 L6 HITL 中断恢复 + 记忆

**目标**：审批与人工介入不能把已完成的工作全部作废 —— 断点续跑是 MVP 的硬要求。

**功能任务**

1. `RunState` 快照：在每个 step 边界持久化 `completed_step_ids` 与 `context_snapshot`（实现 Eino checkpoint 存储接口，落持久化存储）；快照体积设上限，超限时大对象（工具原始返回等）存 `raw_store`，快照只存引用 ID。
2. `resume(run_id)`：从 `pending_approval` 断点继续，按 `completed_step_ids` **跳过已完成步骤**；工具调用以 `tool_call_id` 幂等去重。**时效性数据（天气、设备状态）恢复时一律重新取数**，不使用快照中的旧值；重取后结论发生变化，则作废原作业单草稿并重新生成。
3. 中断类型覆盖三种：等审批（`WAIT_APPROVAL`）、等用户补充信息（`WAIT_INPUT`）、系统异常（`SYSTEM_ERROR` 可重入）。
4. 记忆三层最小实现：`profile`（农场 + 地块 + 常种作物 + 常用药剂偏好）、`episodic`（近 N 次建议与作业结果）、`working`（当前会话）；注入时按相关性截断，不全量灌入。
5. **评测（§4.7-6）**：实现脚本审批人（通过 / 驳回 / 超时）、`FrozenClock.Advance()`、`MemoryStore` 的 fixture 注入，以及 `state_machine_legal` 打分器；补齐审批超时转 `EXPIRED`、恢复时天气变化的样本。

**验收**

1. 审批中断 → 恢复用例：恢复成功率 ≥ 95%，已完成步骤零重复执行（用工具调用计数断言）。
2. 进程重启后仍可 `resume`（快照落持久化存储，不依赖内存）。
3. 时效性数据重取用例：中断后修改 mock 天气，恢复后的建议与作业单基于新数据；条件恶化时原草稿被作废（样本用 fixture 的按调用次序返回实现）。
4. 记忆注入可开关，关闭后链路仍可正常工作（避免记忆成为单点依赖）。
5. 审批超时样本中，作业单经 `FrozenClock.Advance()` 后转 `EXPIRED`，全程状态迁移通过 `state_machine_legal`。

### 5.0.11 L7 审计日志与可观测链路

**目标**：任何一条建议、任何一次审批、任何一次下发，事后都能原样还原。

**功能任务**

1. `audit_log` append-only 表：`audit_id / run_id / actor（user·agent·system）/ action / target / payload_digest / result / created_at`；覆盖提问、工具调用、知识加载、模型调用、校验结果、审批决策、dry-run 执行七类事件。
2. 审计表只落摘要或掩码值（与 L5 脱敏对齐）；完整回放所需的原文（模型输入输出、工具原始返回）写入加密、权限受控的 `raw_store`，按 `run_id` 关联，读取需授权并留审计记录。
3. trace 三级贯通：`run_id → step_id → tool_call_id`，含 LLM 调用的 token 数与耗时；trace 与指标只记 ID 和控制字段，不记内容，内容按 ID 回查 `raw_store`。
4. 基础指标埋点：会话完成率、工具成功率与 P95、拒答率、校验失败率、审批等待时长、恢复成功率、单会话 token 成本。
5. 最小看板：一屏看到上述指标的当日趋势；`run_id` 可一键检索出完整链路。
6. **评测（§4.7-6）**：统一事件流可导出为 transcript（JSONL，按「根节点 - Agent 步骤 - 原子步骤」三层组织）；evalkit 报告中每个 trial 带 `run_id` 链接；评测运行的数据写入独立存储，不混入生产看板。

**验收**

1. 任取一个历史 `run_id`，可在 1 分钟内还原「问了什么 → 调了什么 → 依据是什么 → 建议是什么 → 谁审批 → 执行结果」。
2. 审计完整率 100%（抽样 50 个 run，无缺事件）。
3. trace 覆盖率 100%，无「孤儿 step」。
4. 看板指标与日志统计口径一致（人工对账一次并留记录）。
5. 任取一个 `run_id`，经授权可从 `raw_store` 取回完整模型输入输出；未授权读取被拒绝并留审计记录。
6. 任一评测 trial 可从报告跳转到 run 详情；评测数据不出现在生产看板指标中。

### 5.0.12 L8 最小交互界面 + 示范农场冒烟 + Gate 0 收口

**目标**：让业务与领导能亲手点一遍，而不是看 curl 截图。

**功能任务**

1. 最小 Web 页三块：对话区（提问 + 建议卡展示，建议卡上直接展示 `Citation` 可点开）、待审批区（`WorkOrder` 快照 + 硬约束校验结论 + 通过/驳回）、run 详情区（trace + 审计）。
2. dry-run 执行结果可视化：展示将要下发的设备指令与预期作业参数，明确标注「未触达真实设备」。
3. 示范农场真人演练：农场主 / 农艺师各至少 1 人参与，覆盖四类问题各 2 个，收集书面反馈。
4. 用 evalkit 跑完 100 条评测集，每条 3 次（红线按 pass^3），产出 Phase 0 baseline 报告（红线通过率、引用覆盖率、拒答率、结论一致性人工抽检结果、P95、token 成本），并据实测值确定 §3.2 中 P95 等目标线；评测负责人阅读全部红线失败与 flaky 样本的 transcript 并签字。
5. 输出《Phase 0 验收报告》与《Phase 1 启动包》（任务分解 + owner 指派 + D4/D5 决策结论）。

**验收**

1. Gate 0 七条（§5.0.2）全部通过并留证据。
2. 真人演练反馈成文，问题按「必须改 / 可延后」分类，必须改项在 Gate 0 前闭环。
3. baseline 报告入库，成为 Phase 2 唯一对照基线；报告附实验配置快照（代码提交号、提示词版本、模型档位、知识版本）。
4. 评测 Phase 0 部分（§4.7-6 表中 P0 各行）全部交付；未交付项列入《Phase 1 启动包》并指定负责人。

### 5.0.13 完成后下一步

进入 Phase 1（先做生产可用性与真实下发，**不先做 RAG 与多模态**）。

---

## Phase 1（P1）生产可用期 —— 8 周

**目标**：从「能跑」升级到「能真下发、能上线、能扛住出错」。本阶段第一次让 Agent 的决策触达真实设备，因此安全与可回滚优先于任何功能扩展。

### 5.1.1 范围边界

**必须完成**：真实设备下发、作业硬约束拦截（审批前 + 下发前两次校验）、`WorkOrder` 状态机与幂等补偿、审批体系（角色 / 时效 / 二次确认）、多农场隔离、故障分类与降级、监控告警、审计合规化、灰度与一键关停、农药与航空作业法规准入。

**明确不做**：RAG、多模态诊断、主动预警推送、多机协同排程、自动规划 —— 全部留 Phase 3；准确率提升专项留 Phase 2。

### 5.1.2 要实现的功能

1. **设备下发 Adapter 真实通道**：按 D4 确认的接口，为植保无人机 `P 系列`、无人车 `R/RM 系列`、智能阀 `FBV/FGV`、`智能施肥系统` 各实现 Adapter；统一 `dispatch / query_status / cancel` 三个方法，未实现 `cancel` 的设备类型**不允许开放下发**。
2. **作业硬约束拦截器 `constraint_checker`**：风速上限、降雨、温度、农药安全间隔期 PHI、禁飞与敏感区、设备续航与载药量、相邻地块飘移风险；校验结论写入 `WorkOrder.constraint_checks[]`。校验执行两次：提交审批前任一硬约束不通过则**禁止提交审批**；`APPROVED → DISPATCHED` 前用实时数据**重新校验**，不通过则不下发、作业单退回 `PENDING_APPROVAL` 并通知审批人。
3. **`WorkOrder` 状态机落地**：全量状态迁移 + 幂等（同一 `order_id` 重复下发不产生二次作业）+ 超时未回执处理 + 失败重试策略 + `CANCELED` 补偿路径 + 中途中止转 `PARTIALLY_DONE` 并回填 `done_area`。
4. **审批体系**：角色（农场主 / 农艺师 / 机手 / 管理员）与权限矩阵；审批时效与超时自动失效（转 `EXPIRED`，不可执行）；高风险作业（用药 / 大面积 / 临界天气）二次确认，且用药类必须由农艺师角色签字；审批页高亮药剂、用量、时间窗等关键字段及与历史作业的差异，防止审批走过场；审批意见与驳回原因分类入库（供 Phase 2 归因）。
5. **多农场隔离**：`tenant_id / farm_id`（Phase 0 已进契约）在数据与查询层强制生效；跨农场访问一律拒绝并告警；知识库按区域可见性隔离。
6. **故障分类与降级矩阵**：LLM 不可用 → 规则层保守建议；MCP 超时 → 用最近一次缓存数据并标注新鲜度，超过阈值则拒答；知识缺失 → 转人工；设备离线 → 作业单保持 `APPROVED` 不下发并通知，超过审批时效转 `EXPIRED`。每类故障有明确错误码与用户可读文案。
7. **可观测与告警上线**：核心告警项 —— 建议无引用（P0 级）、护栏被绕过（P0 级）、下发失败率突增、审批积压、恢复失败率、token 成本异常、MCP 成功率跌破阈值。告警必须带 `run_id` 可直接下钻。
8. **审计合规化**：append-only 不可篡改（追加校验链或只追加存储）、可按农场/时间导出、保留期配置与 `raw_object` 到期清理（只追加表的归档删除在 Phase 4，见 §4.6.1-6）、敏感操作二次留痕。
9. **灰度与关停**：feature flag `enable_robot_dispatch`（真实下发总开关）、`enable_advice`（建议生成开关）按农场维度灰度；1 农场 → 3 农场 → 5 农场，每档 24h 观察窗；一键关停后退回 dry-run 模式，不影响建议能力。
10. **法规准入**：真实下发前完成农药与航空作业法规映射（无人机运行与空域规定、操控员资质、飞防用药与农药登记要求），落成 `constraint_checker` 规则或审批前置条件；法规清单由法务签字，Phase 4 只做持续更新。
11. **评测扩展（§4.7-6）**：建 `redteam` 套件（诱导越权下发、诱导编造用量、诱导集合外药剂、跨农场访问、工具返回中夹带指令）；新增下发前复检、`EXPIRED`、`PARTIALLY_DONE`、跨农场隔离的打分器；本阶段 2-8 项的演练用例同时沉淀为 `regression` 样本。

### 5.1.3 完成判定（Gate 1）

1. 5 个示范农场真实下发累计 ≥ 50 单，作业成功率 ≥ 95%，**零安全事故**。
2. 硬约束拦截演练：构造 6 类违规作业（大风 / 降雨 / PHI 未到 / 禁飞区 / 电量不足 / 超载），拦截率 100%。
3. 幂等演练：重复下发、网络重试、回执重放三类场景均不产生二次作业。
4. 一键关停演练：从下达关停到真实下发停止 ≤ 1 分钟，建议能力不受影响。
5. 降级矩阵逐项演练通过，每类故障有用户可读文案与告警。
6. 审计可导出且不可篡改，抽查 100 单完整率 100%。
7. 连续 2 周无 P0/P1 故障；主链路 P95 不劣于 Gate 0 确定的目标线。
8. 下发前复检演练：审批通过后人为改变天气条件，作业单不下发并退回重审；审批超时作业单转 `EXPIRED` 且不可执行。
9. 法规映射清单经法务签字，相关规则已落入 `constraint_checker` 或审批前置条件。
10. `redteam` 套件每个样本跑 5 次，红线全部通过（pass^5 = 1）；`regression` 套件红线不退化。

### 5.1.4 完成后下一步

进入 Phase 2（开始做建议质量与可信度的量化优化）。

---

## Phase 2（P2）质量优化期 —— 8 周（核心阶段）

**目标**：把「建议到底靠不靠谱」从主观讨论变成可度量、可提升、可守住不回退的工程指标。这是决定产品能否被种植者长期信任的阶段。

### 5.2.1 范围边界

**必须完成**：评测集扩容与分层、双轨标注、可信度指标体系、规则与模型职责再切分、知识加载策略优化、长对话压缩、成本与延迟门禁、回归门禁与归因数据采集。

**明确不做**：不引入 RAG（检索质量优化的前提是先有稳定评测集与指标口径）、不引入多模态、不做自动规划。

### 5.2.2 要实现的功能

1. **评测集扩容到 500+ 条**，按「作物 × 生育期 × 问题类型 × 难度」分层抽样；保持「应拒答」负样本占比 ≥ 20%；新增「陷阱样本」（相似病虫害混淆、超剂量诱导、跨作物误用）。
2. **双轨标注与评审**：农艺专家人工标注为金标准；LLM-judge 做规模化预打分，一个维度一个 judge，允许回答「无法判断」并转人工。每个维度用 ≥ 100 个专家标注的 trial 校准，Kappa 达阈值前 judge 分数只进报告、不进门禁；低于阈值时以专家为准并回修 judge 提示词。上线后每月抽检复核。另建 `holdout` 套件，不对调提示词的人公开。
3. **可信度指标体系落地**：引用覆盖率、事实一致率、数值准确率、幻觉率、拒答正确率、过度保守率 —— 六个指标进日报，全部可按 `run_id` 下钻到具体样本。
4. **规则与模型职责再切分**：以 Phase 0-1 的 `run_id` 回放与驳回归因为依据，找出模型输出中仍在隐式承担确定性判断的部分（如作业窗口筛选、混用禁忌判断、多病虫害并发时的优先级排序），逐项下沉到规则层（生育期判定、PHI 推算、药剂候选集合在 Phase 0 已由规则层承担，本阶段只做覆盖率核查）；模型职责收窄为「情境理解 + 方案选择 + 解释生成」。每下沉一项，记录指标变化。
5. **知识加载策略优化**：新增加载命中率、加载精度（加载条目中真正被引用的比例）、无关知识注入率三个指标；按问题类型定制加载策略（如植保类必加 `safety.md`）；加载上限与排序规则可配置、可 A/B。
6. **长对话压缩 `compaction`**：token 超阈值触发，产出结构化摘要（保留地块 / 作物 / 生育期 / 已确认结论 / 待办作业单 / 已引用知识 ID），关键事实白名单字段**禁止被压缩丢弃**；以 Phase 0-1 线上会话长度分布为依据设定阈值，做压缩前后成本与关键事实保留率评测。
7. **成本与延迟门禁**：单会话 token 成本、P95 延迟、压缩触发率进门禁；候选版本相比 baseline 成本上涨超阈值需专项说明才可上线。
8. **回归脚本与上线门禁**：`make eval` 一键跑全量评测集，每个样本跑 5 次，输出 `baseline vs candidate` 按样本配对的对照表与翻转样本清单；红线类样本 pass^5 < 1 或任一可信度指标下降超阈值（差值置信区间不跨 0）则**阻断上线**。PR 跑 regression 冒烟子集（n=3）并阻断合并，每晚跑全量。提示词、模型档位、加载策略、规则层、知识版本任一变更都必须过此门禁。多轮样本引入 LLM 模拟用户（τ-bench 做法），只进质量指标，不进红线门禁。
9. **归因数据采集**：人工改单与驳回原因结构化分类，按周聚合形成「Top 问题清单」，直接驱动下个迭代的知识条目补齐与策略调整。
10. **线上回流成样本**：差评、专家判错、护栏拦截、高频 `KB_MISS` 的 `run_id`，按 `raw_store` 中的当时数据自动生成脱敏的样本草稿，专家补写期望并署名后进入 `capability` 套件，稳定通过后毕业进 `regression`。

### 5.2.3 完成判定（Gate 2）

1. 评测集 ≥ 500 条且分层覆盖完整，应拒答样本 ≥ 20%，专家签署齐全。
2. 六个可信度指标全部上线并进日报；引用覆盖率与数值准确率保持 100%。
3. 事实一致率与幻觉率相比 Phase 0 baseline 有**显著且可复现**的改善（具体目标线由 Gate 1 后依 baseline 实测值确定，避免拍空数字）。
4. `make eval` 回归门禁生效，且至少拦下过 1 次真实的质量回退（有记录）。
5. 成本与延迟门禁生效，单会话成本与 P95 双双不劣于 baseline。
6. 归因 Top 问题清单连续产出 4 周，且有 ≥ 10 条知识条目因此被补齐或修正。
7. 长对话压缩上线：关键事实白名单字段 100% 保留，压缩前后建议结论不漂移，长会话成本下降幅度有实测数据。
8. 三个 LLM-judge 维度与专家的一致性（Kappa）达到阈值并进入门禁；`holdout` 套件建立；线上回流样本每周有产出。

### 5.2.4 完成后下一步

进入 Phase 3（在质量基线冻结后，才上高级能力）。

---

## Phase 3（P3）高级能力期 —— 14 周

**目标**：在 Phase 2 质量基线冻结后，解决三件 MVP 刻意回避的难题 —— 知识规模化（RAG）、看图诊断（多模态）、主动服务（不等人问）—— 并让多设备协同排程可用。

### 5.3.1 范围边界

**本阶段必须完成**

1. 知识层从固定文件升级为 RAG，且**保留固定文件路径作为回滚方案**。
2. 多模态病虫害诊断（拍照 + 巡田影像），带置信度阈值与专家兜底。
3. 主动预警与排程：按节气 / 气象 / 物候主动推送，带频控与预算。
4. 多设备协同排程与作业窗口滚动重排。
5. 一周农事计划（自动规划）小流量灰度，计划内每张作业单仍需执行前确认。
6. 每项能力独立开关、独立评测、独立回滚。

**本阶段明确不做**

1. 不做全自主执行 —— 红线二不变，动设备仍必须人审。
2. 不做模型微调与自建推理集群（除 D1 决策另有结论）。
3. 不做跨区域大规模放量（留 Phase 4）。
4. 不做产量预测与经营决策类高不确定性能力。

### 5.3.2 目标与通过标准（Gate 3）

1. RAG 路径在同一评测集上**不劣于**固定文件路径，且知识规模扩大 ≥ 5 倍后检索精度不下降。
2. 多模态诊断在标注图集上达到约定准确率，低置信度样本 100% 转专家，**不出现高置信度误诊导致的作业单**。
3. 主动预警的有效率（被采纳或被确认有用的比例）达标，用户关闭率低于阈值；无一次因预警刷屏导致投诉。
4. 多机协同排程在示范农场实测缩短总作业时长，且无设备冲突与航线重叠。
5. 自动规划灰度期间**零自动执行**，计划采纳率与人工改单率有数据。
6. 每项能力可单独关闭，关闭后系统退回 Phase 2 形态且指标不劣化。

### 5.3.3 实现路线总览（L0 → L8）

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

### 5.3.4 L0 基线冻结、开关与评测集扩展

**目标**：先把 Phase 2 的成绩钉成不可回退的基线，再动任何策略。

**功能任务**：冻结 Phase 2 指标快照为 `baseline_p2`；新增开关 `enable_rag / enable_vision_diagnose / enable_proactive_alert / enable_multi_device_schedule / enable_auto_plan`，默认全关；评测集扩到 1000+ 条并新增图像子集与主动预警场景子集。evalkit 同步扩展：图像样本的 fixture 支持图片输入；主动预警样本用 `FrozenClock.Advance()` 推进时间触发扫描；RAG 上线时新增召回率、引用命中率打分器（§4.7-6）。

**验收**：所有新开关默认关闭时系统行为与 Phase 2 完全一致（回归全绿）；`baseline_p2` 快照入库不可修改。

### 5.3.5 L1 知识底座升级（RAG）

**目标**：让知识从「几十个文件」扩到「几千条条目」仍能精准命中。

**功能任务**：知识条目切片策略（按语义段 + 保留 front-matter 元数据）；embedding 与向量索引；检索融合（向量 + 元数据过滤：作物 / 生育期 / 区域 / 权威等级）；权威等级优先排序（国标 > 地标 > 企业标准 > 专家经验）；知识版本与生效时间过滤；`enable_rag=false` 时无缝回落固定文件加载。

**验收**：同评测集上 RAG 路径可信度指标不劣于固定文件路径；知识规模扩 5 倍后加载精度不下降；一键回滚固定文件路径演练通过。

### 5.3.6 L2 多模态诊断链路

**目标**：农户拍一张叶片照片，或巡田无人机一次飞行影像，能给出病虫害候选与依据。

**功能任务**：接入千问视觉能力（`Qwen3-VL` 系 / `Qwen3.6-Plus` 原生视觉档位，档位经评测确定）；图像预处理与质量校验（模糊 / 过曝 / 主体缺失直接退回重拍）；诊断输出统一为「候选病虫害 + 置信度 + 判别特征 + 引用知识条目」；置信度低于阈值转专家工作台；影像类接入 `M 系列` 巡田数据与 `FC5` 定点相机。

**验收**：标注图集准确率达标；低置信度样本 100% 转专家；**诊断结论必须携带知识条目引用**，无引用不得转为作业单；图像与坐标数据脱敏合规。

### 5.3.7 L3 主动预警与排程调度

**目标**：从「问了才答」变成「该说就说」，同时不变成骚扰。

**功能任务**：调度器按日期 / 节气 / 物候进度 / 气象预警触发扫描；预警分级（紧急防灾 / 防治窗口临近 / 常规农事提醒）；频控与配额（按农场按日上限、同类事件去重与合并）、静默时段；预警同样走引用校验与拒答规则；预警可一键转 `AdviceCard`，再走既有审批链路。

**验收**：预警有效率达标；用户关闭率低于阈值；同一事件不重复推送；预警成本进成本看板；关闭 `enable_proactive_alert` 后无任何主动推送。

### 5.3.8 L4 风险控制与降级策略

**目标**：高级能力带来的新风险必须有对应的刹车。

**功能任务**：预警误报抑制（连续误报自动降频 + 需人工恢复）；多设备冲突消解（同地块同时段互斥、航线重叠检测、共享资源排队）；作业窗口滚动重排（天气变化时自动重算并通知，已审批作业单变更需重新确认）；RAG 检索异常（召回为空 / 相关度过低）回落固定文件；视觉模型不可用时退回纯文本问诊。

**验收**：每条降级路径有演练记录；已审批作业单在条件变化后不会被静默执行；冲突用例 100% 拦截。

### 5.3.9 L5 质量校验与结果增强

**目标**：让高级能力的产出经过比 Phase 2 更严的出口检查。

**功能任务**：数值二次校验（RAG 命中的公式与 `calc` 层实现一致性校验）；方案冲突检测（药剂混用禁忌、连续施药间隔、与近期已执行作业冲突）；多方案对比输出（给 2-3 个可选方案及取舍理由，由人选择）；解释增强（把「为什么现在做、为什么这个量」讲到农户能懂）。

**验收**：混用禁忌与间隔冲突用例 100% 拦截；多方案输出的采纳率与单方案对照有可观测提升；解释可读性经农户抽样评分达标。

### 5.3.10 L6 自动规划灰度（多步作业计划）

**目标**：从单次建议升级到「一周农事计划」，但绝不越过人审红线。

**功能任务**：多步计划生成（按生育期排布未来 7-14 天农事项）；计划级审批（整体审批 + 逐单二次确认，任一单仍需执行前确认）；计划变更追踪（天气 / 长势变化触发重排并标注变更点）；小流量灰度（`enable_auto_plan` 按农场开启，先 1 家）。

**验收**：计划中每个作业单执行前仍需确认，**零自动执行**；计划采纳率与人工改单率进指标；灰度期间无越权执行。

### 5.3.11 L7 调试视图、可观测性与回归门禁

**目标**：高级能力更难解释，因此调试能力必须同步升级。

**功能任务**：`run` 回放视图（按 `run_id` 重放每一步的输入输出、检索命中、图像诊断中间结果、校验结论）；检索与加载可视化（命中条目 + 相关度 + 是否被引用）；新增指标（检索精度、视觉置信度分布、预警有效率、计划采纳率）；回归门禁扩表纳入图像子集与预警子集。

**验收**：任一 run 可在 2 分钟内回放定位到出问题的具体步骤；回归门禁覆盖全部新增能力；至少拦下 1 次真实回退。

### 5.3.12 L8 灰度发布、回滚预案与验收收口

**目标**：五个新开关逐个放量，每个都能独立退回去。

**功能任务**：按 `RAG → 视觉 → 主动预警 → 多机排程 → 自动规划` 顺序逐项灰度，每项至少 1 周观察窗，同一时间只放量一项；某项能力开发完成即进入灰度，与后续能力的开发并行（节奏见 §5.3.13）；每项写明回滚触发条件与回滚步骤；输出《Phase 3 验收报告》与《Phase 4 启动包》。

**验收**：每项能力独立灰度记录与回滚演练齐全；Gate 3 六条全部通过。

### 5.3.13 推荐实施节奏

1. 第 1 周：L0 基线冻结与开关，评测集扩容启动（标注可并行至第 4 周）。
2. 第 2-5 周：L1 RAG 与 L2 多模态并行（两个独立小组，共用评测底座）。
3. 第 6-9 周：L3 / L4 / L5 三路并行 —— 但 L4 的降级路径必须与对应能力同 PR 提交，不允许后补；同期 L8 灰度 RAG（第 6 周）、视觉（第 7 周）。
4. 第 10-11 周：L6 自动规划开发 + L7 调试与门禁；同期 L8 灰度主动预警（第 10 周）、多机排程（第 11 周）。
5. 第 12-13 周：L8 灰度自动规划（先 1 家农场，2 周观察窗）。
6. 第 14 周：Gate 3 验收收口。

### 5.3.14 角色分工（建议）

1. 架构负责人：契约与开关一致性、跨路评审、Gate 判定。
2. 知识与检索小组：L1 + L5 数值校验。
3. 多模态小组：L2 + 图集标注协同。
4. 编排与调度小组：L3 + L4 + L6。
5. 平台与可观测：L7 + L8。
6. 农艺专家组：图集标注、方案冲突规则、预警有效性评审。
7. QA / SRE：回归门禁、灰度与回滚演练。

### 5.3.15 阶段验收模板（执行后填写）

1. 功能完成情况（按 L0~L8 逐项勾选）。
2. 离线评测与回归结果（`baseline_p2` vs candidate 全指标对照表）。
3. 指标快照（可信度六项 + 新增四项 + 成本与延迟）。
4. 灰度与回滚演练结果（五个开关逐项）。
5. 遗留问题与负责人、关闭时间。
6. 是否进入 Phase 4（是 / 否 + 理由）。

### 5.3.16 完成后下一步

进入 Phase 4（治理、规模化与持续优化）。

---

## Phase 4（P4）企业治理与规模化期 —— 持续

**目标**：让「再加一个作物 / 再加一个区域 / 再加一万个农场」变成流程化动作，而不是每次都靠专家救场；同时把成本、合规、运维形成可持续闭环。

### 5.4.1 要实现的功能

1. **作物与区域扩展工厂化**：《新增作物接入 SOP》（知识条目模板 + 必填清单 + 评测集最小规模 + 验收清单 + 专家签署流程）；目标是新增一个作物从「专家全程陪跑」降到「填模板 + 过验收」，并给出明确人日预算。
2. **知识生命周期与专家审核工作台**：知识条目提报 → 专家评审 → 生效 → 到期复核 → 失效 → 版本回滚全流程线上化；知识条目带「最近复核时间」，超期未复核自动降权并提醒。
3. **成本看板与预算治理**：按农场 / 作物 / 能力维度看 token 与调用成本；预算阈值告警；高成本会话归因（长会话 / 重复提问 / 检索过量）并给优化建议。
4. **合规与数据安全**：田块与影像数据的存储与出域策略、农户隐私、农药与航空作业法规映射的持续更新（首版已在 Phase 1 落地）、责任划分与保险方案落地、审计数据保留期与取证流程。
5. **运维闭环**：Runbook（按告警项一一对应处置步骤）、on-call 值班、季度故障演练（LLM 全挂 / MCP 全挂 / 误下发 / 知识污染四类）。
6. **效果经营机制**：采纳率与亩均成本看板按季度复盘，形成「指标 → 归因 → 知识与策略迭代」的固定节奏；把归因 Top 清单变成季度 OKR 输入。
7. **知识污染防护**：知识条目变更必须经评审，禁止无署名写入；建立知识回归集，知识变更后自动跑评测，防止「改一条知识、坏一批建议」。

### 5.4.2 完成判定（Gate 4）

1. 按 SOP 独立完成 ≥ 2 个新作物接入，且未占用核心研发排期（由扩展小组独立完成）。
2. 专家审核工作台承载全部知识变更，线下改文件的路径被关闭。
3. 成本看板上线，季度成本可预测（预测与实际偏差在约定范围内）。
4. 合规清单逐项有结论与责任人，保险方案落地。
5. 四类故障演练全部完成并留报告；Runbook 覆盖所有 P0/P1 告警项。
6. 季度复盘机制连续运行 2 个季度。

---

## 6. 阶段门禁（每一阶段结束后做什么）

每个阶段收尾必须做 4 件事，缺一件不得进入下一阶段：

1. **阶段验收**：功能演示（真人真机，不用录屏替代）+ 指标对照表（baseline vs 当前）。
2. **回归测试**：离线评测全量跑 + 主链路冒烟 + 降级路径演练。
3. **风险清单更新**：遗留问题、负责人、关闭时间；红线相关问题**不允许延期**。
4. **下一阶段启动包**：任务分解到 route 级、owner 指派、阻塞项决策结论。

---

## 7. 全面测试方案

### 7.1 测试分层

1. **单元测试**：`calc` 规则层用量计算、`constraint_checker` 每条硬约束、`WorkOrder` 状态机合法与非法迁移、脱敏函数、输出校验三层、压缩白名单保留、幂等去重。优先表驱动。
2. **集成测试**：MCP 各工具的正常 / 超时 / 空返回 / 异常四态；`提问 → 建议 → 作业单 → 审批 → 执行` 全链路；`中断 → resume` 链路；多农场隔离。
3. **离线评测**：评测集全量回放，产出六项可信度指标 + 成本延迟，输出 `baseline vs candidate` 对照表。
4. **线上观测**：看板 + 告警 + 按 `run_id` 下钻；每周人工抽检 20 个 run 做质量巡检。

### 7.2 核心质量测试（可信度，Phase 2 重点）

1. **评测集构建**：按「作物 × 生育期 × 问题类型 × 难度」分层；应拒答负样本 ≥ 20%；陷阱样本（相似病虫害混淆、超剂量诱导、跨作物误用、过期知识引用）独立成组。
2. **核心指标**：引用覆盖率、事实一致率、数值准确率、幻觉率、拒答正确率、过度保守率。
3. **对比实验设计**：一次只变一个因子（提示词 / 模型档位 / 加载策略 / 规则下沉项 / 检索策略），固定评测集与 fixture，模型按生产配置运行（不固定随机种子、不改 temperature）。每个样本跑 5 次，红线按 pass^5、质量指标按均值与置信区间统计，按样本配对对比并记录因子贡献。
4. **上线门禁**：任一可信度指标下降超阈值 → 阻断；引用覆盖率或数值准确率低于 100% → 无条件阻断；成本或 P95 劣化超阈值 → 需专项说明。

### 7.3 专项测试

1. **作业安全专项**：6 类硬约束违规作业必被拦截；审批后条件变化的下发前复检用例；PHI 边界值（差 1 天）用例；禁飞区边界用例；相邻地块飘移用例；集合外药剂与未登记作物用药用例。
2. **护栏绕过红队测试**：诱导模型直接下发（「不用审批了，帮我直接打」）、诱导编造用量（「你估个数就行」）、诱导跳过引用、越权跨农场访问 —— 全部必须失败，且留审计记录。
3. **脱敏专项**：手机号 / 身份证 / 精确坐标 / 账号在日志、模型入参、审计表、导出文件四个出口全部无明文。
4. **HITL 恢复演练**：审批中断、等用户输入、进程重启、工具重放四类场景的 resume 正确性与幂等性，以及恢复时时效性数据（天气、设备状态）是否重新取数。
5. **降级演练**：LLM 不可用、MCP 全挂、知识缺失、设备离线、RAG 召回为空，逐项验证行为与文案。
6. **压测与成本**：并发会话下 P95 与工具池表现；长会话压缩触发后的成本曲线；突发预警扫描的调度峰值。
7. **知识变更回归**：修改 / 新增 / 失效知识条目后自动跑知识回归集，防止知识污染扩散。

---

## 8. 多人协作执行图与人力投入

### 8.1 角色分工

1. **项目 Owner / 架构负责人**：契约冻结、阶段 Gate 判定、跨组冲突裁决、对决策层汇报。
2. **Agent Harness RD**：七件套（MCP 网关、护栏、HITL、记忆、压缩、审计、可观测）与主链路编排；其中 1 人兼任评测负责人，负责 `evalkit` 框架与打分器评审（§4.7-6）。
3. **数据与物联网 RD**：四个 MCP Server、设备 Adapter、气象与地块数据打通。
4. **知识工程 RD**：知识目录与加载器、`calc` 规则层、Phase 3 的 RAG。
5. **农艺专家组（关键角色，不可缺）**：知识条目撰写与署名、评测集标注、方案冲突规则、诊断图集标注、预警有效性评审。
6. **前端 / 客户端**：管家页、审批台、专家工作台；Phase 1 起并入 `极飞农场` App / 小程序。
7. **QA**：评测样本工程化与报告解读、回归门禁、红队测试、演练组织。
8. **SRE**：部署、灰度、告警、Runbook、容量与成本。
9. **法务合规**：数据出域、农药与航空法规、责任与保险。

### 8.2 人力投入（建议规模）

| 阶段 | 研发 | 农艺专家 | QA / SRE | 说明 |
|------|------|----------|----------|------|
| Phase 0 | 4-5 人 | 1-2 人（半投入） | 1 人 | Harness 2 人、数据接入 1-2 人、知识 1 人、前端 1 人（可兼） |
| Phase 1 | 5-6 人 | 1 人（半投入） | 1 QA + 1 SRE | 增设备 Adapter 与审批体系投入 |
| Phase 2 | 4-5 人 | 2 人（重投入，标注量大） | 1.5 人 | 专家标注是关键路径，需提前锁定档期 |
| Phase 3 | 6-8 人 | 2 人 | 2 人 | 三路并行，建议拆小组 |
| Phase 4 | 3-4 人常态 + 扩展小组 | 1-2 人 | 1 人 | 转入常态运营与扩展 |

### 8.3 协作原则

1. **契约先冻结**：§4.5 与 L1 的五个契约冻结后才允许并行开发，变更走版本与 Owner 批准。
2. **一个 PR 只做一件事**：不跨 route 大面积混改；PR 标题带 `Ln` 编号。
3. **每个 PR 必须带测试证明**：新增功能配单测，策略变更配评测对照。
4. **红线相关代码双人评审**：护栏、审批闸门、硬约束校验、脱敏四处改动必须两人签署。
5. **农艺专家是评审人而非事后顾问**：知识与建议逻辑相关 PR，专家签字后才可合入。

### 8.4 成本与预算（待补，立项评审前填写）

本文档目前只给出人力规模，立项评审前需由 Owner 会同财务补齐以下内容：

1. 各阶段人力成本（按 §8.2 人数 × 周期折算）。
2. 模型调用成本测算：按示范农场规模估算日均会话数 × 单会话 token 成本（Phase 0 baseline 后用实测值修正）。
3. 基础设施成本：服务部署、存储（审计、`raw_store`、快照）、气象等第三方数据授权费用。
4. 设备作业与保险成本（Phase 1 起）。
5. 收益假设与回本口径：收费模式（随设备 / 订阅 / 农服分成）及对应的采纳率、亩均成本节省假设。

---

## 9. 立即执行的首批任务（两周内启动）

**决策类（第 1 周必须闭环，否则 Phase 0 无法按期开工）**

1. 拍板 D1 模型形态与档位（法务对数据出域的正式结论可在 Phase 1 前闭环，期间按脱敏后入模的最严口径开发）。
2. 拍板 D2 首个作物 + 区域 + 示范农场，并拿到农场书面配合承诺。
3. 指定 D3 农艺知识签字专家与投入档期。
4. 启动 D6 气象数据源授权（第 2 周闭环，首批工程任务依赖真实天气工具）；启动 D4 设备接口清单梳理（可与开发并行，Phase 1 前闭环）。

**工程类（第 1-2 周）**

1. 按 §4.6 搭工程骨架与配置体系，`enable_*` 开关框架先落地（默认全关）；先打通最小链路 `runner → policy → mcp-weather → callbacks（审计与 trace）→ 断点恢复`，用它验证 harness 接口。
2. 完成 L1 五个契约的 schema 定义与状态机常量，发起契约评审会。
3. 搭 `mcp-gateway` 骨架 + 一个 `mcp-weather` 真实工具打通（验证链路可行性）。
4. 建 `knowledge/` 目录与 `index.yaml` 格式，专家产出首批 10 条知识条目样例（验证模板是否好写）。
5. 按 §4.7 建 `evalkit` 骨架与样本 YAML 格式，先入 20 条样例样本，`make eval` 跑通回放与报告。
6. 建立最小看板与 `run_id` 检索能力（后续所有调试都依赖它，越早越省事）。

**汇报类**

1. 第 2 周末做一次 15 分钟进度同步：契约图 + 一个真实天气工具调用 demo + 10 条知识条目样例，让决策层看到项目已落地而非停在 PPT。

---

## 10. 风险登记与应对

| 风险 | 影响 | 应对 |
|------|------|------|
| 农艺知识产出跟不上（专家档期是稀缺资源） | 直接卡死红线一，建议无法上线 | Phase 0 只锁一个作物四类问题；提前锁定专家档期并写入排期；知识模板做到「填空即可」降低专家成本 |
| 设备下发接口不开放或协议缺失（D4） | Phase 1 停在 dry-run，产品价值打折 | Phase 0 就用统一 Adapter 抽象 + mock，接口就绪即可替换；同时准备「只出建议 + 人工操机」的降级商业形态 |
| 模型幻觉造成错误用药，引发实际损失 | 最高风险，可能造成赔偿与品牌损伤 | 红线一 + 红线二 + 数值下沉规则层 + 药剂限定在登记候选集合 + 硬约束两次校验 + 人审；Phase 0 不开真实下发；保险方案在 Phase 1 前落地 |
| 数据出域与隐私合规（田块坐标、农户信息） | 可能阻断公有云方案 | D1 与法务同步决策；脱敏在 L5 前置实现；预留私有化部署路径 |
| 成本失控（长会话 + 高频工具调用 + 主动预警扫描） | 单位经济模型不成立 | 成本指标从 Phase 0 起进看板；长对话压缩在 Phase 2 落地；Phase 2 设成本门禁；预警加频控与配额 |
| 主动预警变成骚扰导致用户关闭 | 主动服务能力失效 | Phase 3 L3 频控 + 分级 + 去重 + 静默时段；关闭率进 Gate 3 |
| 评测集质量不足，导致优化「优化了但没变好」 | Phase 2 整阶段白做 | 评测集由专家签署；应拒答与陷阱样本强制占比；LLM-judge 与专家一致性校验 |
| 阶段边界被业务压力打破（要求提前上高级能力） | 质量与安全同时失守 | 阶段门禁写进项目章程，Gate 未过不放量；用 Phase 0 的可演示成果换取时间空间 |

---

## 11. 后续「一个个实现」的操作规则

1. **单阶段单目标**：同一时间只推进一个 Phase，不并行跨阶段抢进度。
2. **先功能后测试、但不允许不测**：功能合入当周必须补齐对应单测与评测样本。
3. **每个策略变更都要能回滚**：开关、配置、提示词版本三条回滚路径至少具备一条。
4. **阶段结果三件套**：功能清单完成情况、指标变化对照表、遗留风险清单。
5. **只有阶段门禁通过才切下一阶段**；门禁未过就地整改，整改结论写入风险清单。
6. **红线不参与权衡**：任何「为了进度先绕过审批 / 先不校验引用」的提案一律否决，不做例外记录。










