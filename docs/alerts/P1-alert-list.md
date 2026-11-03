# 告警清单（P1-L7-B）

规则文件 `deploy/alerts/p1.yaml`（Prometheus 规则格式），指标由 cmd/server 与 cmd/worker 的 `/metrics` 暴露（`configs/app.yaml` 的 `metrics.server_addr` / `metrics.worker_addr`）。
`internal/service/alert_rules_test.go` 校验：规则引用的指标与 label 都已登记、取值在枚举内、`model.AlertNames` 与 `policy/fault_matrix.yaml` 的告警名都有规则且级别一致、每条规则带 `drilldown` 与「待实测」阈值注解。

级别口径对齐 Gate 1 第 7 条：P0 = `severity: critical`，电话告警，按 §8.3 第 1、4 条处置（全关 `enable_robot_dispatch`）；P1 = `severity: warning`，值班群通知，§8.3 第 3 条（超阈值且 1 小时内未恢复回退一档）。Gate 1 第 7 条的「连续 2 周无 P0/P1 故障」按本清单计。

**所有阈值、窗口与 `for` 均为草拟值，待实测**：依 Phase 0 baseline 与灰度首档数据回填，改动随规则文件版本化。

下钻：`GET /api/v1/farms/{farm_id}/alerts?name=<告警项>&from=&to=&limit=&cursor=`（读权限 `advice.read`；缺省最近 24h、50 行，跨度上限 744h、limit 上限 200）。按 `(created_at, id)` 倒序列出 `ALERT.*` 审计行，每行带 `run_link`（`/api/v1/farms/{farm_id}/runs/{run_id}`）或 `order_id`。`cross_farm_denied` / `flag_changed` 另列业务审计 `CROSS_FARM_DENIED` / `FLAG_CHANGE`，这两类行的 summary 不对外返回。
响应带 `next_cursor` 时还有下一页：其余参数不变、把它作为 `cursor` 传回（键集分页，`{unix 毫秒}_{id}`，非法游标 400）。

**没有文档级下钻的告警**：只有经 AlertSink 落 `ALERT.*` 审计的 6 条红线与有业务审计的 `cross_farm_denied` / `flag_changed` 指向告警列表。纯指标告警（四个 `as_fault_*`、`dispatch_failure_rate`、`approval_backlog`、`recheck_returned`、`expired_surge`、`resume_failure_rate`、`token_cost_anomaly`、`mcp_success_low`）不写审计行，告警列表对它们恒为空，drilldown 指向看板 `GET /api/v1/farms/{farm_id}/metrics`，再按看板的作业单 / 迁移明细逐条查。`alert_rules_test.go` 校验这一分界。

**新序列首样本**（审查修复 P0）：计数器序列按需创建时首个样本即为 1，`increase()` 只看窗口内两点之差会漏掉第一次。两层兜底：① `/metrics` 进程启动及每 `alerts.backlog_refresh` 按已登记农场（含 farm_id=0）预置 0 值序列（`MetricsObserver.Preseed`）；② 「任一次即告警」的规则（6 条红线、`as_fault_llm_unavailable`、`as_fault_device_offline`、`flag_changed`）另加 `or (X > 0 unless X offset <窗口>)` 分支，覆盖预置前新登记的农场。

## 接收角色

| 规则 `receiver` | 角色（P1 路线 §9） | 说明 |
|---|---|---|
| `sre_oncall` | SRE 值班 | 告警、灰度执行、一键关停；P0 电话第一接收人 |
| `farm_manager` | 农场管理员 | 本场审批积压、过期、设备离线、开关变更 |
| `agronomist` | 农艺师 | 复检退回、知识库未覆盖 |

审计链类 P0 同时抄送 `auditor`（审计员）与法务合规；P0 一律抄送项目 Owner。

## P0（critical，电话）

| 告警项 | 触发（草拟，待实测） | 接收人 | 初步处置 |
|---|---|---|---|
| `advice_no_citation` 建议无引用 | worker 扫描（30s 一轮，回看 2m）发现非 REFUSED 且无引用的建议卡，`as_alert_total` 2m 增量 > 0 | SRE 值班 | §8.3-1 立即全关（GLOBAL `force_off`）；下钻 run 看 `ADVICE_READY` 前的引用与校验；在途单按 §8.4-2 |
| `model_actuate` 模型发起设备动作 | 网关策略拦截模型的 `actuate` 调用即告警 | SRE 值班 | 立即全关；查 run 的工具调用与提示词版本，确认无设备调用（拦截已生效） |
| `pre_approval_device` 审批前设备调用 | 下发器在任何写入前（admit / realReady / DRY_RUN 记意图前）与调 Adapter 前校验票据 APPROVED 且属本单，不符即拒绝并丢弃消息 | SRE 值班 | 立即全关；下钻作业单看票据状态与迁移序列，确认设备调用计数为 0、工单状态与 attempts 未变 |
| `audit_write_failed` 审计写入 / 封链失败 | L7-A 经 AlertSink 触发，任一次 | SRE 值班 | 业务已回滚；查库与 `audit_chain_head` 锁等待，恢复前不放量。本场 head 缺失时，这条告警的审计回退写平台链 (0,0)，summary `origin` 带原 tenant / farm，农场告警列表看不到，到平台审计里查 |
| `audit_chain_broken` 审计链断 | L7-A 每日锚点 / verify 失败，任一次 | SRE 值班（抄送审计员、法务） | §8.3-4 立即全关，排查前不恢复；`cmd/auditctl verify` 定位首个异常位置 |
| `audit_export_pii_leak` 导出命中敏感明文 | L7-A 导出 `pii_clean` 命中，任一次 | SRE 值班（抄送审计员、法务） | 确认对象已删、outbox 置 FAILED；排查 `pii_mask` 规则后重导 |
| `as_fault_llm_unavailable` LLM 不可用 | `as_degraded_total{fault="LLM_UNAVAILABLE"}` 5m 增量 > 0 且持续 2m | SRE 值班 | 确认建议走规则层降级 / 拒答文案；查模型供应商与限流；不需关下发 |

## P1（warning）

| 告警项 | 触发（草拟，待实测） | 接收人 | 初步处置 |
|---|---|---|---|
| `as_fault_mcp_timeout` MCP 超时 | 10m 内 DEGRADED(MCP_TIMEOUT) > 3 次，持续 5m | SRE 值班 | 查对应 MCP server 上游；确认缓存回落在 max_staleness 内 |
| `as_fault_kb_miss` 知识库未覆盖 | 1h 内 DEGRADED(KB_MISS) > 5 次 | 农艺师 | 整理未覆盖问题，补知识条目 |
| `as_fault_device_offline` 设备离线 | 15m 内任一次 DEGRADED(DEVICE_OFFLINE) | 农场管理员 | 现场确认设备在线 / 空闲；单子按时效转 EXPIRED，不补发 |
| `dispatch_failure_rate` 下发失败率突增 | REAL 下发 30m 失败率（rejected / manual / error / blocked）> 20% 且量 ≥ 5 | SRE 值班 | 1h 内未恢复回退一档（§8.3-3）；manual 进 L4 人工对账 |
| `approval_backlog` 审批积压 | OPEN 票 > 20 张持续 30m | 农场管理员 | 催审；必要时调整排班，过期单不补发 |
| `recheck_returned` 复检退回 | 1h 内 RECHECK_FAILED > 3 次 | 农艺师 | 核对天气 / 约束变化是否合理，重审退回单 |
| `expired_surge` EXPIRED 突增 | 1h 内转 EXPIRED > 5 张 | 农场管理员 | 区分审批超时与下发时效超限；查开关是否被关 |
| `cross_farm_denied` 跨农场拒绝 | 10m 内拒绝 > 10 次（按拒绝码） | SRE 值班（抄送审计员） | 下钻 CROSS_FARM_DENIED 审计看用户与路径；疑似越权扫描时封禁账号 |
| `flag_changed` 开关变更 | 任一次 | 农场管理员（抄送 SRE 值班） | 核对 FLAG_CHANGE 审计的操作人与审批记录 |
| `resume_failure_rate` 恢复失败率 | 30m 续跑失败率 > 10% 且量 ≥ 5 | SRE 值班 | 1h 内未恢复回退一档；查 checkpoint 与 run 锁 |
| `token_cost_anomaly` token 成本异常 | 1h 用量 > 昨日同时段 3 倍且 > 200000 | SRE 值班 | 查是否循环调用 / 提示词回归；必要时关 `enable_advice` |
| `mcp_success_low` MCP 成功率低 | 按 server 15m 成功率 < 95% 且调用 ≥ 20，持续 5m | SRE 值班 | 1h 内未恢复回退一档；查上游与网关错误码分布 |

无 `farm_id` label 的规则（`cross_farm_denied`、`flag_changed`、`resume_failure_rate`、`mcp_success_low`）的 drilldown 带 `{farm_id}` 占位，值班按审计行逐场填写（`resume_failure_rate`、`mcp_success_low` 填看板 `/metrics`）。

## 附录：演练步骤（待演练）

对应 P1 §6.8 验收第 1 条（告警 1 分钟内触发并可下钻到 `run_id`）与第 4 条（看板与审计口径对账）。在预发环境执行，不在业务库 `agri_steward` 上制造数据；每步记录时间戳截图，签字后归档到 `docs/drill/`。

1. **建议无引用（待演练）**
   1. 前置：worker 运行，Prometheus 抓取 worker `/metrics`（`metrics.worker_addr`），规则文件已加载，评估周期 ≤ 15s。
   2. 在预发库对一个测试农场插入一张 `outcome='ADVICE'`、无 `advice_citation` 的建议卡（或用评测夹具关闭引用校验生成一张），记录时刻 T0。
   3. 预期：T0 + 30s 内 worker 写 `ALERT.advice_no_citation`（target `run:{id}`）与 Error 日志；`as_alert_total{alert="advice_no_citation"}` +1；T0 + 60s 内 Prometheus 告警 firing。
   4. 打开注解里的 drilldown，列表第一行 `run_link` 指向该 run；再等一轮扫描，确认不重复告警（审计行仍为 1）。
2. **审批前设备调用（待演练）**
   1. 构造一张 REAL 作业单进入下发，在下发器调 Adapter 前把票据改回 OPEN（或注入工单状态不符），记录 T0。
   2. 预期：设备调用计数 0，`ALERT.pre_approval_device`（target `order:{id}`）落审计，`as_dispatch_total{result="blocked"}` 与 `as_alert_total{alert="pre_approval_device"}` 各 +1；T0 + 60s 内告警 firing。
   3. drilldown 列表行带 `order_id` 与 `run_link`。
3. **看板与审计口径对账（待演练）**
   1. 选一个有 REAL 单的测试农场与单日 D，调 `GET /api/v1/farms/{farm_id}/metrics?from=D&to=D`。
   2. 按 `docs/metrics.md`「P1-L7-B 运营指标」的对账 SQL 逐项查库，比对 `dispatch_success_rate`、`job_success_rate`、`recheck_returned`、`expired`、`approval_backlog`、`approval_wait`。
   3. 记录差异与原因（预期为 0 差异）；结果写入对账记录并签字。
