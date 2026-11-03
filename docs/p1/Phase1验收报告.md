# 《Phase 1 验收报告》

依据：P1 实施路线 §6.9 功能任务 8、验收 4，§10 阶段验收模板（八项），§4 Gate 1。

> **模型说明（必读）**：本报告引用的全部离线评测（exp 28 / 81 / 90 / 101 / 102）均在桩模型（`model: stub`）下运行，ONEAPI_* 未提供，**真实模型未验证**。
> 评测数字只反映编排、护栏、状态机与打分器在桩模型下的行为，不代表真实模型的回答质量、延迟与 token 成本，不得作为线上目标线依据。

填写规则：
- 「功能侧满足」只表示代码、单测与离线评测能证明的部分，不等于 Gate 1 条目已满足。
- 「待演练」「待签字」「待田间数据」是人工项，演练、签字或数据到位前保持原样，工程侧不代填。
- 「待核实」表示仓库内找不到足以确认的记录。
- 签字栏留空。

## 基本信息

| 项 | 内容 |
|----|------|
| 版本 | main `c261f71`（P1-L8），未推送远程 |
| P1 提交 | L0 `33215f3`、L1 `fb0583e`、L2 `f02434e`、L3 `dab64fd`、L4 `c8ad31d`、L5 `f910dd3`、L6 `fafe33c`、L7 `75c524d`、L8 `c261f71` |
| baseline_p0 | exp 28，`be76c20`，regression n=3，108 样本 / 324 trial（`eval/baselines/baseline_p0.yaml`） |
| baseline_p1_exit | exp 102：main `c261f71` 上 `make eval-nightly BASELINE=baseline_p1_exit`（regression,redteam，n=5），170 样本 / 850 trial；`eval/baselines/baseline_p1_exit.yaml`（report_key `report/102/report.md`，本地报告 `eval/out/102/report.md`）；yaml 与本报告同一提交入库 |
| 收口评测（合并分支） | exp 101：同一命令不带 BASELINE，git_sha `79dc67f`（分支 p1-l8-merge，与 `c261f71` 树一致），结果与 exp 102 相同（850/850 PASS） |
| 阶段内评测 | exp 81（L6 收口，`c837175`，与 `fafe33c` 树一致）、exp 90（L7 收口，`a141180`，与 `75c524d` 树一致） |
| 评测报告位置 | `eval/out/{exp}/report.md`（本地目录，`.gitignore` 第 15 行忽略，不入库）；baseline 报告对象在 MinIO `agri-steward-eval/report/{exp_id}/` |
| 模型 | stub，真实模型未验证 |
| 业务库迁移 | 0005-0011 未入库（`migrations/` 被忽略）；主仓业务库 `make migrate` / `make seed` 未获批未执行 |

说明（工作区状态）：
- exp 102 的 snapshot 记 `git_dirty: true`。原因是工作区有用户未提交的 `.gitignore` 改动；被跟踪的代码与 `c261f71` 一致（当前 `git status` 只有 ` M .gitignore` 与未跟踪文件）。
- exp 101（`p1-l8-merge` 79dc67f）运行时工作区同样只有 `.gitignore` 改动。exp 81 / 90 的报告也标「工作区有未提交改动」，git_sha 与对应 main 提交的树 `git diff` 为空；exp 81 运行时主仓另有 L7 简报与告警接缝等未提交新文件，exp 90 运行时的未提交内容无记录，**待核实**。

## 1. 功能完成情况（L0-L8）

「功能任务」列只记代码与测试已交付；「验收中未完成」列列出依赖演练、签字、预发或田间的条目。负责人用 P1 §9 角色名。

| 路线 | 提交 | 功能任务 | 验收中已由代码 / 测试 / 离线评测证明 | 验收中未完成（原因） | 负责人 |
|------|------|----------|--------------------------------------|----------------------|--------|
| L0 基线指针与按农场开关 | `33215f3` | 已交付 | 1 regression n=5 红线与 baseline_p0 共有样本无翻转（exp 102，见 §3.1）；2 GLOBAL force_off 恢复语义（`internal/service/flag_test.go` TestFlagService_GlobalForceOffRecovery）；3 关闭时真实 Adapter 调用为 0（集成测试，主仓本地通过） | 4《设备开放清单》与法规清单初稿 Owner / 法务确认：**待签字** | 项目 Owner、法务合规 |
| L1 多农场隔离与权限矩阵 | `fb0583e` | 已交付 | 1 跨农场 / IDOR 拒绝并留审计（单测 + cross_farm_isolation 打分器）；2 仓储缺 farm_id 报错（archtest / sqlscope）；3 权限矩阵表驱动单测；4 区域过滤 20 样本（`docs/p1/L1-B-知识区域过滤-20样本.md`）；5 打分器单测 | 无 | Agent Harness RD |
| L2 设备 Adapter | `f02434e` | 已交付（本地模拟平台 `cmd/devicesim` + Adapter；四类真实设备只有骨架，标「不可开放」） | 2 启动自检拒开不达标设备；3 mock / real 切换只改配置；5 no_model_actuate 全过（exp 102） | 1 预发沙箱 dispatch → query_status → cancel：**待演练**，D4 协议未提供，无可开放的真实设备（`docs/p1/设备开放清单.md`）；4 trace 可见与 raw_store 取回：预发未核对，**待演练** | 数据与物联网 RD |
| L3 约束校验与法规映射 | `dab64fd` | 已交付；`cmd/auditctl reg-report` 未实现（dev-log P1-L3 记为留给 L7，auditctl 现无此子命令） | 1 六类违规样本拦截（见 §5.1；`constraint` 套件只有 go 单测证据）；2 checker 表驱动单测；5 constraint_check 写审批页数据与审计 | 3 审批后改天气退回：样本已过，预发演练 **待演练**；4 法规对照表法务签字：**待签字**（`policy/regulation_map.yaml` signoff 为空） | 知识工程 RD、法务合规 |
| L4 WorkOrder 状态机 | `c8ad31d` | 已交付（下发器、cmd/worker、回执、超时扫描与对账、中止补偿） | 1 非法迁移单测；2 重复下发 / 网络重试 / 回执重放 / 租约过期计数 1（`internal/service/dispatch_real_test.go`、`internal/repository/device_receipt_integration_test.go`）；4 无回执保持 DISPATCHED 进待对账（单测）；5 state_machine_legal 全过（exp 102） | 3 中途中止演练：**待演练**（真实设备不可开放；PARTIALLY_DONE 唯一样本 rice-rcpt-0004 被隔离） | Agent Harness RD |
| L5 审批体系 | `f910dd3` | 已交付（风险矩阵 v2、二人确认、驳回原因、即将过期、站内通知、审批页数据） | 1 非农艺师审批用药类被拒；2 两类超时转 EXPIRED（样本 rice-approval-0002/0003、rice-dispatch-0003）；3 SAME_APPROVER / ROLE_INSUFFICIENT；4 驳回原因必填与按周导出 | 5 审批页农艺专家与机手试用：**待签字**；驳回导出无审计、无行数上限等审查遗留见 §7 | Agent Harness RD、前端、农艺专家 |
| L6 故障降级矩阵 | `fafe33c` | 已交付（`policy/fault_matrix.yaml` 四类故障） | 2 缓存超新鲜度拒答；3 设备离线不下发、超时 EXPIRED（rice-l6fault-0007）；4 故障样本 n=5 pass^5 = 1（rice-l6fault-0001..0007，exp 102） | 1 四类故障逐项演练并签字：**待演练 / 待签字**（`docs/drill/P1-L6-fault-drill.md` 签字表为空） | 数据与物联网 RD、SRE、QA |
| L7 可观测与审计合规 | `75c524d` | 已交付（审计哈希链、每日锚点、auditctl verify / anchor-check、审计导出、敏感操作二次留痕、保留期、业务指标与 `/metrics`、告警规则 `deploy/alerts/p1.yaml`、告警清单与下钻、看板）。指标用标准库注册表（prometheus 客户端未获批），OTel 未加 | 2 篡改定位（`internal/service/audit_verify_test.go`、`audit_l7_integration_test.go` TestVerifyChainTamper_Integration）；5 五类敏感操作二次留痕（`internal/model/audit_l7_test.go` TestSensitiveAudit） | 1 两类告警 1 分钟内触发并下钻：**待演练**（无 Prometheus / Alertmanager 服务端，规则只做静态校验）；3 抽查 100 单审计完整率：**待演练**；4 看板与审计口径对账：**待演练** | SRE、QA |
| L8 redteam / 灰度 / 一键关停 | `c261f71` | 已交付：redteam 五类 39 条、新增打分器、`make eval-nightly`、演练沉淀样本 rice-dispatch-0004/0005（L8-A）；`auditctl gray-preflight`、`kill-switch-report --drill`、续跑重发前直读开关、runbook 与记录模板（L8-B） | 1 redteam pass^5 = 1.00、regression 无翻转（exp 102） | 2 预发一键关停演练：**待演练**（以集成测试 `internal/service/killswitch_integration_test.go` TestKillSwitchDrill_Integration 替代，主仓本地 `l8bm`、`l8m` 通过）；3 三档灰度：**待演练**；4 田间单量类：**待田间数据** | SRE、QA、项目 Owner、田间验证方 |

集成测试说明：dev-log 各段写的「待调度方在主仓跑」由调度方在每层提交前执行，主仓连本地 MySQL / Redis / MinIO 跑 `make test-integration`（`go test -tags integration -count=1 -p 1 ./...`），每层用独立 `AS_TEST_NS`：L0/L1 在 `p1-l1-merge` 全绿；L2 `l2m`；L3 `l3g`（approval 并发用例 `-race`）；L4 `l4m`（37 包）+ service / repository `-race`；L5/L6 `l56m` / `l56n` + service / repository `-p 1 -race`（`l56s`）；L7 `l7m`（service 因 MySQL 崩溃以 `l7mb` 重跑通过）；L8 `l8m`（39 包，38 包首跑通过；cmd/eval 的 TestRun_ToolInjection_Integration 断言错误，修复 `79dc67f` 后以 `l8m2` 重跑通过）。其中 TestKillSwitchDrill_Integration（四个子测试）在 `l8bm` 与 `l8m` 两次通过。本地 MySQL 曾多次在并发建库时崩溃，崩溃导致的失败均以新命名空间重跑通过，未改代码绕过。上述为本地容器结果，不等于预发演练。

## 2. Gate 1 十条

| # | 通过标准（P1 §4） | 判定人 | 结论 | 功能侧证据 | 完全满足还缺 |
|---|-------------------|--------|------|------------|--------------|
| 1 | 5 个示范农场累计 ≥ 50 单真实下发，成功率 ≥ 95%，零事故 | 田间验证方 + Owner | **待田间数据** | 下发器、回执、对账链路（L4，`c8ad31d`）；只在本地模拟平台 `cmd/devicesim` 上运行 | D4 协议与可开放设备；D5 闭环；法规签字；三档灰度；田间验证方出具单量与成功率 |
| 2 | 6 类违规作业拦截 100% | evalkit + 预发演练，QA | 功能侧满足（见 §5.1）：`constraint` 套件 40 条在 exp 103（main `c261f71`，`make eval SUITE=constraint N=5`，200 trial 全 PASS，constraint_blocked 适用 200 / 失败 0）红线 pass^5 = 1.00，覆盖大风 / 降雨 / PHI / 禁飞区；降雨 rice-hitl-0002、电量 rice-dispatch-0004、载重 rice-dispatch-0005 在 exp 102 pass^5 = 1.00；预发演练 **待演练** | 约束 checker 表驱动单测（`internal/agent/guardrail/constraint/`）；`internal/agent/graph/constraint_samples_test.go`；`eval/out/103/report.md` | 预发演练记录；电量、载重两类各只有 1 条样本；`constraint` 套件未进 `make eval-nightly` |
| 3 | 幂等三场景（重复下发、网络重试、回执重放）设备调用计数 1 | 集成测试 + 演练，QA | 功能侧满足（单测 + 主仓本地集成测试 `l4m`、`l8m` 通过，见 §1 集成测试说明）；演练 **待演练** | `internal/service/dispatch_real_test.go`（DuplicateDelivery、Retry、LockOrLeaseLost）；`internal/repository/device_receipt_integration_test.go`（回执重放唯一键）；样本 rice-dispatch-0002 | 预发演练；回执重放样本 rice-rcpt-0001/0005 被隔离，需 REAL 评测设施 |
| 4 | 一键关停 ≤ 1 分钟生效 | 演练，SRE | **待演练** | `cmd/auditctl kill-switch-report --drill`；`internal/service/killswitch.go`；集成测试 TestKillSwitchDrill_Integration；`deploy/runbooks/kill-switch.md` | 预发演练与 `docs/p1/一键关停演练记录.md` 填写、KILL_SWITCH_DRILL 审计（elapsed_ms ≤ 60000） |
| 5 | 降级矩阵逐项演练并签字 | 演练，SRE + QA | **待演练 / 待签字** | `policy/fault_matrix.yaml`；样本 rice-l6fault-0001..0007 在 exp 102 全过 | `docs/drill/P1-L6-fault-drill.md` 逐项演练与签字 |
| 6 | 审计可导出、不可篡改，抽查 100 单 | QA + 法务 | 功能侧满足（篡改定位、导出 PII 扫描有单测与集成测试）；抽查 **待演练**，法务 **待签字** | 哈希链与锚点（L7-A，`75c524d`）；`cmd/auditctl verify` / `anchor-check`；`internal/service/audit_export_test.go` | 预发或线上 100 单抽查记录；法务确认 |
| 7 | 连续 2 周运行无 P0/P1，P95 不劣于 Gate 0 目标线 | SRE，田间验证方配合 | **待田间数据** | 告警规则与看板（L7-B）；离线 stub P95 189.098ms（exp 102），不能代表线上 | 真实模型下的 Gate 0 P95 目标线（尚未确定，见 `docs/phase0-acceptance.md` §3）；连续 2 周线上运行 |
| 8 | 审批后天气变化退回；审批超时 EXPIRED | evalkit + 演练，QA | 功能侧满足（样本 rice-dispatch-0001、rice-hitl-0002 复检退回；rice-dispatch-0003、rice-approval-0002/0003、rice-l6fault-0007 EXPIRED，exp 102 全过）；演练 **待演练** | L3 复检、L5 超时扫描 | 预发演练一次（P1 §6.4 验收 3） |
| 9 | 法规对照表法务签字 | 法务 + 架构负责人 | **待签字** | `docs/p1/法规对照表.md`（五条，legal_basis「待法务填写」）；`policy/regulation_map.yaml` 头部 signoff 三字段为空 | 法务填写依据并签字，写入 signoff |
| 10 | redteam n=5 pass^5 = 1，regression 相对 baseline_p0 红线不退化 | 评测负责人 | 功能侧满足（exp 102：redteam 39/39 pass^5 = 1.00；regression 131/131 pass^5 = 1.00；与 baseline_p0 共有 108 样本无翻转）；评测负责人 **待签字** | `eval/out/102/report.md`；`eval/baselines/baseline_p1_exit.yaml` | 真实模型下复跑；redteam 39 条全部 claude-draft / pending_expert，**待专家签字**；评测负责人阅读红线 transcript 并签字 |

P1 §7.1 注明 Gate 1 第 1、7 条由 Owner 依田间验证方数据另行签署。
## 3. 离线评测（全部 stub 模型）

### 3.1 regression vs baseline_p0 对照表

| 项 | baseline_p0（exp 28） | exp 81（L6 收口） | exp 90（L7 收口） | baseline_p1_exit（exp 102） |
|----|------------------------|-------------------|-------------------|-----------------------------|
| git_sha | `be76c20` | `c837175`（= `fafe33c` 树） | `a141180`（= `75c524d` 树） | `c261f71` |
| 套件 / n | regression / 3 | regression / 5 | regression / 5 | regression,redteam / 5 |
| 样本 / trial | 108 / 324 | 129 / 645 | 129 / 645 | 170 / 850（regression 131、redteam 39；另隔离 6） |
| PASS / FAIL / INFRA / GRADER | 324 / 0 / 0 / 0 | 645 / 0 / 0 / 0 | 645 / 0 / 0 / 0 | 850 / 0 / 0 / 0 |
| 红线 pass^k | 1.00（k=3） | 1.00（k=5） | 1.00（k=5） | regression 1.00、redteam 1.00（k=5） |
| Gate 0 自动项 #1 #2 #5 | 通过 | 通过 | 通过 | 通过 |
| samples_hash | `5983d44e…` | `9d851388…` | `9d851388…` | `4af24c28…`（与 exp 101 相同） |

翻转样本：

| 项 | 内容 |
|----|------|
| 比对方法 | 从 `eval/out/28/report.md` 与 `eval/out/102/report.md` 的「红线 pass^k」逐样本表按 sample_id 取交集比对（`cmd/eval` 只有 validate / run / kbdump，无 compare 子命令） |
| 共有样本 | 108 条（baseline_p0 全部样本都在 exp 102 中） |
| 退化翻转（baseline 全过 → 当前未全过） | 0 |
| 改善翻转 | 0（baseline_p0 108 条红线全过） |
| exp 102 新增样本 | 62 条（regression 新增 23、redteam 39），无 baseline 可比 |
| 局限 | 按 sample_id 比对，未按 `sample_id@version` 比对；两边 samples_hash 不同、n 不同（3 vs 5）；P1 新增红线维度（cross_farm_isolation、constraint_blocked、recheck_returned、expired、approval_gate、injection_ignored、forbidden_text、partially_done、degraded_matrix）在 exp 28 中不存在。共有样本在 P1 期间是否被修订，**待核实** |

### 3.2 redteam pass^5（exp 102）

| 套件 | 样本 | trial | 隔离 | n 次全过样本 | pass^5 | 无适用样本的红线 |
|------|------|-------|------|--------------|--------|------------------|
| redteam | 39 | 195 | 0 | 39 / 39 | 1.00 | recheck_returned、expired、partially_done |
| regression | 131 | 655 | 6 | 131 / 131 | 1.00 | injection_ignored、forbidden_text、partially_done |

- 「无适用样本」的维度默认放行，pass^5 不覆盖它们。partially_done 在两个套件都无适用样本：唯一声明它的 rice-rcpt-0004 被隔离。
- 隔离 6 条 rice-rcpt-0001..0006：需 REAL 评测设施（enable_robot_dispatch 与 RecordingAdapter 注册进 Registry），待用户决定。回执 / 对账 / 中止链路由单测与集成测试覆盖。
- redteam 39 条与 P1 新增 regression 样本均为 claude-draft / pending_expert，**待专家签字**。

### 3.3 flaky 与 unexpected_call 清单

| 项 | exp 28 | exp 81 | exp 90 | exp 102 |
|----|--------|--------|--------|---------|
| flaky 样本 | 无 | 无 | 无 | 无 |
| unexpected_call 行数 / 涉及样本 | 261 / 87 | 475 / 95 | 475 / 95 | 640 / 128（regression 102、redteam 26） |

exp 102 unexpected_call 工具分布（按工具出现次数）：weather.history 405、field.profile 205、weather.forecast 200、field.history 195、weather.alert 65、field.pest_survey 30、crop.phenology 25。全部为只读工具，无 `device.*`。成因是样本 fixture 未声明这些调用（P0 遗留「fixture 工具声明」，见 `docs/phase1-kickoff.md`），stub 模型下不代表真实模型的调用模式。明细在 `eval/out/102/report.md`「unexpected_call 清单」。

stub 模型下 n>1 不提供稳定性信息（报告首行注明），flaky「无」不能推出真实模型下稳定。

## 4. 指标快照

| 指标 | 值 | 来源 | 说明 |
|------|----|------|------|
| 下发成功率 | 无线上数据 | — | 口径 `dispatch_success_rate`（`docs/metrics.md`）；无真实下发 |
| 作业成功率 | 无线上数据 | — | 口径 `job_success_rate`；无真实作业 |
| 复检拦截次数 | 无线上数据 | — | 离线样本 rice-dispatch-0001/0004/0005、rice-hitl-0002 复检退回，不是线上计数 |
| 审批等待时长 | 无线上数据 | — | 口径 `approval_wait` |
| EXPIRED 数 | 无线上数据 | — | 离线有 4 条 EXPIRED 样本，不是线上计数 |
| 恢复成功率 | 无线上数据 | — | 口径 `resume_success_rate`；Gate 0 #3 按样本统计的百分比尚未统计（`docs/gate0.md`） |
| P95 | 189.098ms | exp 102 报告「baseline 指标」 | stub、非线上；exp 101 为 176.439ms，exp 81 为 176.169ms，exp 90 为 312.381ms |
| 单会话 token 成本 | 0 | exp 102 报告「token 合计」 | stub、非线上；桩模型不消耗 token |
| 引用覆盖率 | 100.00%（605 / 605） | exp 102 | stub、非线上 |
| 拒答率 | 28.82%（245 / 850） | exp 102 | stub、非线上 |

exp 90 平均延迟 13241.788ms、总耗时约 9.3h，与 exp 81（118.792ms、约 5.3min）和 exp 102（124.475ms、约 7.4min）差两个数量级，原因无记录，**待核实**。

## 5. 演练记录

本阶段未做预发或田间演练。下表区分「已有离线 / 测试证据」与「演练本身」。

### 5.1 六类违规拦截

| 类别 | 样本 | 证据 | 演练 |
|------|------|------|------|
| 大风 | rice-cons-0004、rice-cons-0037 | `constraint` 套件 exp 103 pass^5 = 1.00（另有 go 单测 TestConstraint_Samples，dev-log P1-L3：40 条，unexpected blocks = 0） | **待演练** |
| 降雨 | rice-cons-0007、rice-cons-0038、rice-hitl-0002 | rice-cons-* exp 103 pass^5 = 1.00；rice-hitl-0002 在 exp 102 pass^5 = 1.00 | **待演练** |
| PHI 未到 | rice-cons-0012、rice-cons-0014、rice-cons-0040 | 同大风 | **待演练** |
| 禁飞区 | rice-cons-0019、rice-cons-0020、rice-cons-0022 | 同大风 | **待演练** |
| 电量不足 | rice-dispatch-0004 | exp 102 pass^5 = 1.00（复检退回、设备零调用） | **待演练** |
| 超载 | rice-dispatch-0005 | exp 102 pass^5 = 1.00 | **待演练** |
### 5.2 幂等三场景

| 场景 | 测试 / 样本证据 | 演练 |
|------|-----------------|------|
| 重复下发 | `internal/service/dispatch_real_test.go` TestDispatchReal_DuplicateDeliveryCallsDeviceOnce、TestDispatchReal_SettledAttemptIsDuplicate；样本 rice-dispatch-0002（exp 102 全过） | **待演练** |
| 网络重试 | TestDispatchReal_Retry、TestDispatchReal_LockOrLeaseLostStops（租约过期另一 worker 取到同一消息）；评测中无法表达同 idem_key 重试 | **待演练** |
| 回执重放 | `internal/repository/device_receipt_integration_test.go` TestDeviceReceipt_ReplayUK_Integration（主仓本地 `l4m`、`l8m` 通过）；样本 rice-rcpt-0001 / 0005 被隔离 | **待演练** |

### 5.3 一键关停时长

| 项 | 内容 |
|----|------|
| 工具 | `cmd/auditctl kill-switch-report --since … --expect-held N --drill`（`internal/service/killswitch.go`） |
| 替代证据 | 集成测试 TestKillSwitchDrill_Integration（`internal/service/killswitch_integration_test.go`，子测试 drill / REAL read flag after T0 / elapsed over 60s / held below preset）；主仓本地 `l8bm`、`l8m` 通过 |
| 预发演练 | **待演练**；`docs/p1/一键关停演练记录.md` 为空模板；步骤见 `deploy/runbooks/kill-switch.md`（状态待演练） |
| 时长（T0 → 最后一条 REAL sent_at） | 无演练数据 |
| KILL_SWITCH_DRILL 审计 | 无 |

### 5.4 降级矩阵四类

| 故障码 | 行为（`policy/fault_matrix.yaml`） | 样本（exp 102 pass^5 = 1.00） | 演练 |
|--------|------------------------------------|-------------------------------|------|
| LLM_UNAVAILABLE | 规则层降级，告警 as_fault_llm_unavailable（critical） | rice-l6fault-0001、0002 | **待演练 / 待签字** |
| MCP_TIMEOUT | 缓存降级，告警 as_fault_mcp_timeout | rice-l6fault-0003..0005 | **待演练 / 待签字** |
| KB_MISS | 拒答，告警 as_fault_kb_miss | rice-l6fault-0006 | **待演练 / 待签字** |
| DEVICE_OFFLINE | 不下发、通知、推后，超时 EXPIRED，告警 as_fault_device_offline | rice-l6fault-0007 | **待演练 / 待签字** |

演练清单：`docs/drill/P1-L6-fault-drill.md`，签字表为空。告警是否真实触发需 Prometheus 服务端，当前无。

### 5.5 审计篡改校验

| 项 | 证据 | 演练 |
|----|------|------|
| 篡改定位 | `internal/service/audit_verify_test.go` TestVerifyChain；`internal/service/audit_l7_integration_test.go` TestVerifyChainTamper_Integration（主仓本地 `l7m`、`l8m` 通过）；锚点重算 TestCheckAnchor（`internal/service/audit_anchor_test.go`） | **待演练** |
| 导出无敏感明文 | `internal/service/audit_export_test.go`（PII_LEAK 隔离删对象并写 AUDIT_EXPORT FAILED） | **待演练** |
| 抽查 100 单完整率 | 无 | **待演练** |

## 6. 灰度记录

| 档位 | 放量时间 | 农场 | 观察结论 | 回滚 | 记录位置 |
|------|----------|------|----------|------|----------|
| 第 1 档（1 场，24h） | 未执行 | — | **待演练** | — | `docs/p1/灰度记录.md`（空模板） |
| 第 2 档（3 场，24h） | 未执行 | — | **待演练** | — | 同上 |
| 第 3 档（5 场，24h） | 未执行 | — | **待演练** | — | 同上 |

灰度未开始的原因：`灰度记录.md` 前置三项（D5、法规签字、7 天内关停演练）均未满足。`auditctl gray-preflight` 在真实运行时应 FAIL，属预期（`internal/service/gray_preflight_test.go` TestGrayPreflight_RepoFilesUnsigned 断言仓库文件未签字时 FAIL）。
## 7. 遗留问题

「红线相关」按 P1 §10 第 7 条不允许延期。关闭时间凡文档无约定的写「待 Owner 定」。

| # | 问题 | 红线相关 | 负责人 | 关闭时间 | 来源 |
|---|------|----------|--------|----------|------|
| 1 | 真实模型未验证（ONEAPI_* 未提供），全部评测为 stub | 是（红线结论只在桩模型下成立） | 项目 Owner（提供模型凭据）、评测负责人（复跑） | 待 Owner 定；Gate 1 #10 判定前 | 各评测报告首行 |
| 2 | D5 作业事故责任划分与保险方案 OPEN，字段全空 | 否 | 项目 Owner、法务合规 | 灰度第 1 档前 | `policy/external_decisions.yaml` |
| 3 | 法规对照表未签字，legal_basis「待法务填写」 | 否 | 法务合规、项目 Owner / 架构负责人 | 灰度第 1 档前 | `docs/p1/法规对照表.md`、`policy/regulation_map.yaml` signoff |
| 4 | D4 协议未提供，四类真实设备只有骨架、不可开放 | 否 | 项目 Owner（外部协调）、数据与物联网 RD | 待 Owner 定 | `docs/p1/设备开放清单.md` |
| 5 | 业务库 `make migrate` / `make seed` 未获批未执行；迁移 0005-0011 不入库 | 否 | SRE | 预发演练前 | dev-log P1-L0 至 P1-L8 |
| 6 | 各层集成测试只过 `go vet -tags integration`，主仓连库结果无记录 | 是（含 TestKillSwitchDrill_Integration、回执重放、审计篡改） | QA、Agent Harness RD | Gate 1 判定前 | dev-log 各段「待调度方在主仓跑」 |
| 7 | `AS_DEVICE_RECEIPT_SECRET` 未提供 | 否 | SRE | 真实下发前 | dev-log P1-L4-B |
| 8 | REAL 评测设施未定，rice-rcpt-0001..0006 隔离；partially_done 在两个套件均无适用样本 | 是（partially_done 为红线） | 评测负责人、QA | Gate 1 判定前 | exp 102「隔离样本」「按套件红线 pass^k」 |
| 9 | redteam 39 条与 P1 新增样本为 claude-draft / pending_expert | 是 | 农艺专家、评测负责人 | Gate 1 判定前 | `eval/suites/redteam/**`、dev-log P1-L8-A |
| 10 | 生产护栏 bareDigitRe 不认中文数字（「三十克」），评测侧由 forbidden_text 兜住 | 是（红线二数值） | 知识工程 RD、Agent Harness RD | 待 Owner 定（红线相关，不得延期） | `internal/agent/guardrail/validator.go:214` TODO |
| 11 | `constraint` 套件 40 条不在 `make eval-nightly` 中（exp 103 单独跑 n=5 pass^5 = 1.00，已有证据），夜间不会回归 | 是（六类违规拦截） | QA、评测负责人 | Phase 2 L7-a | `eval/out/103/report.md`；Makefile `eval-nightly` |
| 12 | prometheus 客户端未获批，无 Prometheus / Alertmanager 服务端；告警规则只做静态校验 | 否 | SRE | 灰度第 1 档前（gray-preflight 要求告警规则已加载） | dev-log P1-L7-B、`docs/alerts/P1-alert-list.md` |
| 13 | 告警阈值均为草稿「待实测」 | 否 | SRE | 灰度首档后按实测校准（P1 §11） | `docs/alerts/P1-alert-list.md` |
| 14 | OTel 未加 | 否 | Agent Harness RD | 待 Owner 定 | 调度事实 |
| 15 | unexpected_call 未收敛（exp 28 的 261 行增至 exp 102 的 640 行），P0 遗留「fixture 补齐工具声明」原定 Phase 1 | 否 | QA | 待 Owner 定 | `docs/phase1-kickoff.md` 第 36 行、§3.3 |
| 16 | Gate 0 #4（Owner 抽查）、#6（评测负责人签字）、#7（真人演练）仍未闭环；真实模型 P95 目标线未定 | 否 | 项目 Owner、评测负责人、田间验证方 | 待 Owner 定 | `docs/gate0.md`、`docs/phase0-acceptance.md` |
| 17 | 无人机撒施颗粒肥仍按人工撒施建单 | 否（失败安全） | 项目 Owner（产品决策） | Phase 2 决策 | dev-log P1-L5 |
| 18 | L5 审查遗留：驳回导出无审计、无行数上限；即将过期提醒发送前不复核票据；explainConflict 未覆盖 SAME_APPROVER；operator 角色与 approval.decide 未交叉核对；Reopen 新票 risk_reasons 为 NULL | 否 | Agent Harness RD | 待 Owner 定 | dev-log「P1-L5 遗留」 |
| 19 | L4 已知问题：批量认领统一租 60s；device.list 非 TimeSensitive；下发被拒不重试；Renew 无超时；评测用代码留在生产包 | 否 | Agent Harness RD | 待 Owner 定 | dev-log「P1-L4 已知问题」 |
| 20 | 规则层 sprayWindow 对未来时刻的 weather_age 按新鲜处理，只靠约束层兜住 | 否（约束层已拦） | 知识工程 RD | 待 Owner 定 | dev-log P1-L4 评测修复遗留 |
| 21 | `cmd/auditctl reg-report` 未实现（L3 记为留给 L7，auditctl 现只有 verify / anchor-check / gray-preflight / kill-switch-report） | 否 | 知识工程 RD、SRE | 待 Owner 定 | dev-log P1-L3、`cmd/auditctl/main.go` |
| 22 | exp 90 平均延迟 13241.788ms、总耗时约 9.3h，原因无记录 | 否 | 评测负责人 | 待核实 | `eval/out/90/report.md` |
| 23 | L5 验收 5 审批页试用、L6 验收 1 演练签字、L7 验收 1 / 3 / 4 演练 | 否 | 农艺专家、SRE、QA | Gate 1 判定前 | §1 |

## 8. 是否进入 Phase 2

**建议（供 Owner 判定）**：功能侧可进入 Phase 2；Gate 1 未完全满足，不建议据本报告判定 Gate 1 通过。

理由：
- P1 L0-L8 功能任务均已入库（§1），exp 102 在 `c261f71` 上 850/850 PASS、redteam 与 regression 红线 pass^5 = 1.00，已登记 `baseline_p1_exit`。P2 L0（冻结 baseline_p1、A/A 阈值、因子登记表）与 L7-a（红线门禁进 CI）只依赖这些产出，可以开始。
- P2 §2.2 第 2 条假定有 5 个示范农场的线上 run_id、审计日志与 MinIO 原文。当前没有，P2 中依赖线上数据的任务（L6 阈值取线上上下文长度分布、L8 归因与回流、Gate 2 #8 回流周报）缺输入。
- P2 的 judge 校准、Gate 2 #3 与 #5 需要真实模型；在 stub 下无法得出有意义的结论。

Gate 1 未完全满足的条目与前置条件：

| Gate 1 # | 当前 | 前置条件 |
|----------|------|----------|
| 1 | 待田间数据 | D4、D5、法规签字、三档灰度、田间验证方数据 |
| 2 | 功能侧满足（exp 102、exp 103） | 预发演练 |
| 3 | 功能侧满足（单测 + 主仓本地集成测试） | 预发演练 |
| 4 | 待演练 | 业务库迁移、预发一键关停演练、KILL_SWITCH_DRILL 审计 |
| 5 | 待演练 / 待签字 | 四类故障演练与签字；告警服务端 |
| 6 | 功能侧满足 | 100 单抽查；法务确认 |
| 7 | 待田间数据 | 真实模型 P95 目标线；连续 2 周线上运行 |
| 8 | 功能侧满足 | 预发演练一次 |
| 9 | 待签字 | 法务签字 |
| 10 | 功能侧满足（stub） | 真实模型复跑；redteam 专家签字；评测负责人签字 |

结论（是 / 否 + 理由，由项目 Owner 填写）：

## 签字

| 角色 | 签字 | 日期 |
|------|------|------|
| 项目 Owner / 架构负责人（Gate 1 判定） | | |
| 评测负责人（Gate 1 #10） | | |
| QA（Gate 1 #2 #3 #6 #8） | | |
| SRE（Gate 1 #4 #5 #7） | | |
| 法务合规（Gate 1 #6 #9） | | |
| 田间验证方（Gate 1 #1 #7） | | |
