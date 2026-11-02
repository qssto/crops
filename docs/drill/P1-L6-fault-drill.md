# P1-L6 故障降级演练清单（§6.7 验收第 1 条）

依据：P1 实施路线 §6.7 验收 1「四类故障逐项演练通过，每类有用户可读文案与对应告警（演练清单签字）」。
本清单只列操作与核对项，不预填结果。每次演练复制本文件为 `P1-L6-fault-drill-{日期}.md` 填写。

声明来源：`policy/fault_matrix.yaml`（故障 → 行为 → 拒答原因 → 文案 key → 告警），文案在 `configs/texts/zh-CN.yaml`。
服务启动时校验矩阵，缺文案 / 缺告警即启动失败；演练前先确认 server、worker 正常启动。

告警规则由 L7 接入。L7 合入前，告警项只核对「矩阵中已声明且与故障一一对应」，告警是否真实触发记为「待 L7」。

## 通用核对（每类故障都做）

- run 详情：`run_event` 中有且仅有一条 `event_type='DEGRADED'`、`actor='system'`，payload 的 `fault` / `action` / `text_key` 与下表一致。
- 审计：`SELECT action, target, actor_type, result FROM audit_log WHERE run_id=? AND action='DEGRADED'`，应为一行 `target='fault:{故障码}'`、`actor_type='system'`、`result='OK'`。
- 页面文案与表中文案逐字一致，文案不含数字；降级建议卡显示「降级」标注（`degraded=true`）。
- 非故障拒答（空数据、非法返回、校验失败）不写 DEGRADED。

## 1. LLM_UNAVAILABLE（告警 `as_fault_llm_unavailable`，critical）

- 降级条件：分类（classify）成功、知识已命中且规则层有阈值结论，之后选药（choose）或解释（explain）重试 1 次仍不可用。
  分类节点本身不可用时尚无知识与规则结论，必然拒答。
- 1a 可降级：field-001，问「1号田稻飞虱到防治指标了吗？要不要打药？」，只让 explain 不可用。
  - 注入：现场没有按任务关停模型的开关，整体断开模型（下方 1b）只能演练拒答。1a 以评测样本 rice-l6fault-0001
    （`fixtures.model.unavailable: [explain]`）为证据；若现场需要真人演练，须在模型网关侧按任务拦截，演练前约定方式。
  - 期望：ADVICE，观察类，不选药剂；`degraded=true`；文案 `fault.llm_unavailable.degraded`
    「模型暂不可用，按规则层判定给出保守建议：{decision}；请人工复核后再安排作业」；DEGRADED `action=RULE_FALLBACK`；输出校验照常执行。
- 1b 不可降级：`model.provider=openai`，把 `ONEAPI_BASE_URL` 指向不可达地址后重启 server，问同一问题。演练后恢复。
  - 期望：REFUSED / `LLM_UNAVAILABLE`；文案 `fault.llm_unavailable.refused`「模型暂不可用且规则层无明确结论，请稍后再问」；`action=REFUSE`。
- 对照样本：rice-l6fault-0001（规则层降级）、rice-l6fault-0002（全部任务不可用，拒答）。

## 2. MCP_TIMEOUT（告警 `as_fault_mcp_timeout`，warning）

- 触发：只读工具返回 `TIMEOUT` 或 `UPSTREAM`。天气 `max_staleness=1h`，作物 `24h`；field / device 不缓存。
- 2a 缓存内：先正常提问一次写入缓存，在 1h 内让天气源超时再问。
  - 期望：ADVICE，`degraded=true`；引用 `data_source=cache`，`collected_at` 为原采集时间（不是回落时刻）；DEGRADED `action=CACHE_FALLBACK`，`staleness_sec` 与实际间隔一致，`tool=weather.forecast`。
- 2b 缓存超限或无缓存：超过 1h 后再次超时，或对无缓存工具（如 field.pest_survey）超时。
  - 期望：REFUSED / `DATA_MISSING`；文案 `fault.mcp_timeout.refused`「所需田块数据缺失或已过期，暂无法给出建议，请补充数据后再问」；`action=REFUSE`，payload 带 `tool`；无凭空建议。
- 注入方式：mock 模式没有运行时超时开关。现场若无法在真实源上制造超时，以评测样本 rice-l6fault-0003（UPSTREAM 无缓存）、0004（恰 1h）、0005（超 1s）与 `internal/agent/tools/mcp/fallback_test.go` 作为证据，并在签字栏注明。

## 3. KB_MISS（告警 `as_fault_kb_miss`，warning）

- 操作：field-005 提问知识库未覆盖的问题（如 drill/README.md 的 D2 台风准备）。
- 期望：REFUSED / `KB_MISS`，转人工；文案 `fault.kb_miss.refused`「知识库未覆盖该问题，不凭经验作答，请咨询农技专家」；`action=REFUSE`。
- 对照样本：rice-l6fault-0006。

## 4. DEVICE_OFFLINE（告警 `as_fault_device_offline`，warning）

- 注入：在 `configs/mock/device.yaml`（模拟平台与 mcp-device mock 共用）把目标设备 `status.online` 改为 `false`、`state: offline`，重启模拟平台。
- 操作：生成并审批一张作业单（DRY_RUN 或 REAL 均可），等待 worker 下发。
- 期望：
  - 设备 0 次调用（模拟平台无 Dispatch 请求）；作业单保持 `APPROVED`。
  - `outbox` 中 `topic='notify.send'`、`dedup_key='notify:DEVICE_OFFLINE:{order_id}:{ticket_id}'` 恰一行，多轮推后不重复；文案 `fault.device_offline.notify`「作业设备离线，作业单暂缓下发，设备恢复后自动重试；超过审批时效将自动过期」。
  - 下发消息 `next_run_at` 每次推后 1 分钟，`last_error='DEVICE_OFFLINE'`，`attempts` 不变；DEGRADED `action=HOLD_AND_NOTIFY` 只在首次通知时写一次。
  - 过 `dispatch_deadline_at` 后作业单转 `EXPIRED`（重试中的 DISPATCHED 单转 FAILED，reason `DEVICE_UNAVAILABLE`）。
- 恢复：设备改回在线，确认未过期的单能正常下发。
- 对照样本：rice-l6fault-0007（需连库评测）。

## 签字

| 故障 | 结果（通过 / 不通过） | run_id / order_id | 文案一致 | 告警（已触发 / 待 L7） | 问题记录 |
|------|----------------------|-------------------|----------|------------------------|----------|
| LLM_UNAVAILABLE 1a | | | | | |
| LLM_UNAVAILABLE 1b | | | | | |
| MCP_TIMEOUT 2a | | | | | |
| MCP_TIMEOUT 2b | | | | | |
| KB_MISS | | | | | |
| DEVICE_OFFLINE | | | | | |

- 演练日期：
- 环境（commit / 配置）：
- 执行人：
- 田间验证方签字：
- 本团队签字：
