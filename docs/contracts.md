# 核心契约（P0 L1 冻结）

本文件与 `contracts/*.schema.json`、`internal/model` 一起构成 L1 冻结的契约。各阶段只引用、不修改。

## 冻结规则

- 冻结后任何变更（字段、枚举、状态、迁移边）都要升版本号，并经契约 Owner 书面批准后再合入。
- 只做向后兼容的增量：新增可选字段或新增枚举值时升 minor；删除、改名、改语义时升 major，并同时提供迁移说明。
- `internal/model` 是 Go 侧唯一实现，只能 import 标准库（archtest 守卫）。`contracts/*.schema.json` 与 `internal/model` 的一致性由 `internal/model` 下的 schema 测试校验。
- 表结构以 `migrations/0001_init.up.sql` 为准。改表只能新增迁移文件，禁止 AutoMigrate，也不能改已发布的迁移。

## 契约清单

| 契约 | Schema | Go 类型 |
|------|--------|---------|
| 建议卡 | `contracts/advice_card.schema.json` | `model.AdviceCard` |
| 引用 | `contracts/citation.schema.json` | `model.Citation` |
| 作业单 | `contracts/work_order.schema.json` | `model.WorkOrder` |
| 审批单 | `contracts/approval_ticket.schema.json` | `model.ApprovalTicket` |
| 运行状态 | `contracts/run_state.schema.json` | `model.RunState` |

## 作业单状态机

```mermaid
stateDiagram-v2
    [*] --> DRAFT
    DRAFT --> PENDING_APPROVAL
    DRAFT --> CANCELED
    PENDING_APPROVAL --> APPROVED
    PENDING_APPROVAL --> REJECTED
    PENDING_APPROVAL --> EXPIRED
    PENDING_APPROVAL --> CANCELED
    APPROVED --> DISPATCHED
    APPROVED --> PENDING_APPROVAL: 复检退回 / 排程变更
    APPROVED --> EXPIRED: 超过 dispatch_deadline_at
    APPROVED --> CANCELED
    DISPATCHED --> EXECUTING
    DISPATCHED --> DONE: 乱序回执
    DISPATCHED --> PARTIALLY_DONE: 乱序回执
    DISPATCHED --> FAILED
    DISPATCHED --> CANCELED
    EXECUTING --> DONE
    EXECUTING --> PARTIALLY_DONE
    EXECUTING --> FAILED
    EXECUTING --> CANCELED
    DONE --> [*]
    PARTIALLY_DONE --> [*]
    REJECTED --> [*]
    EXPIRED --> [*]
    FAILED --> [*]
    CANCELED --> [*]
```

## 迁移表

与 `model.WorkOrderTransitions` 逐条一致。不在表中作为 from 出现的状态都是终态。

| from | 允许的 to |
|------|-----------|
| DRAFT | PENDING_APPROVAL, CANCELED |
| PENDING_APPROVAL | APPROVED, REJECTED, EXPIRED, CANCELED |
| APPROVED | DISPATCHED, PENDING_APPROVAL, EXPIRED, CANCELED |
| DISPATCHED | EXECUTING, DONE, PARTIALLY_DONE, FAILED, CANCELED |
| EXECUTING | DONE, PARTIALLY_DONE, FAILED, CANCELED |
| DONE / PARTIALLY_DONE / REJECTED / EXPIRED / FAILED / CANCELED | 终态，无出边 |

## 迁移执行（service.WorkOrderService.Transition）

在调用方事务（`*repository.DB`，即 `*gorm.DB`）内依次执行以下步骤，任一步失败则整个事务回滚：

1. `GetState` 读取 status、row_ver、run_id，查询带 tenant_id 和 farm_id 条件。
2. `model.CanTransition(from, to)` 判断迁移是否合法，非法返回 `model.ErrIllegalTransition`。
3. 执行乐观锁 `UPDATE work_order SET status=?, row_ver=row_ver+1, updated_at=? WHERE id=? AND tenant_id=? AND farm_id=? AND status=? AND row_ver=?`。影响 0 行时返回 `model.ErrConcurrentUpdate`。
4. 追加一行 `work_order_transition`。
5. 调用 `EventStore.AppendTx` 写入 `ORDER_TRANSITION` 事件。seq 由同一连接上的 `LAST_INSERT_ID(event_seq + 1)` 分配。

本方法不加 Redis 锁。`lock:order:{order_id}` 由需要串行化外部副作用的调用方（如 L8 下发）持有。

## 开票前约束拦截留痕（P1-L3，偏离 §6.4）

§6.4 原文要求 `constraint_check` 用 INSERT IGNORE、历次校验都可见。实现为了不加迁移，做了如下偏离：

- **PRE_APPROVAL 行为最近一次复核结果，历次拦截证据见 CONSTRAINT_BLOCKED payload。** `constraint_check(order, PRE_APPROVAL, attempt=ticket_seq)` 按 uk_check upsert，并删掉本次未出现的 rule_id。
- `CONSTRAINT_BLOCKED`（run_event，只追加；审计投影 APPROVAL / FAILED，target `order:{order_id}`）的 payload 为 `model.ConstraintBlockedPayload`：
  - `order_id`、`rule_ids`（未通过的 rule_id，按校验顺序）；
  - `checks[]`：每条未通过行的 `rule_id`、`observed`、`threshold`（原样取自校验行，`threshold.kb` 为 `kb:<kb_id>@<version>`）、`note`（即 `observed.note`）；通过行不写入；
  - `truncated`：payload 上限 8KB（8192 字节）。超出时先从 `checks` 末尾逐行截掉，仍超再截 `rule_ids` 末尾，并置 `truncated: true`；未截断时不出现该字段。
- 开票与拦截事务的第一条语句都是 `SELECT id FROM work_order WHERE tenant_id=? AND farm_id=? AND id=? FOR UPDATE`，同一张单的写入串行。拦截输给并发开票时回滚本次校验行与事件，在事务外记一条 Warn 日志（order_id、run_id、rule_ids、ticket_id）。

## 设备回执 webhook（P1-L4-B）

- 路由：`POST /api/v1/farms/:farm_id/orders/:order_id/receipts`，挂在根路由，不经用户鉴权。只在配置了 `AS_DEVICE_RECEIPT_SECRET` 时挂载，未配置启动时记 Warn。
- 签名：头 `X-Timestamp`（unix 秒）与 `X-Signature` = hex(HMAC-SHA256(secret, timestamp + "." + body))，用 `hmac.Equal` 比较，时间窗 5 分钟。
- body（JSON）：`receipt_id`、`device_type`、`device_job_id`、`status`、`failure_kind`、`done_area_mu`、`device_ts`、`code`。`receipt_id` 为空时由 `service.ReceiptIDOf` 从来源、job、状态、failure_kind、面积与 device_ts 推导。
- 反查：按 (device_type, device_job_id) 经 SystemScope 找尝试，再核对路径里的 farm_id / order_id。
- 响应：200 已处理（含重放、STALE / LATE / CONFLICT，只入库不推进）；202 反查不到（记 RECEIPT_UNMATCHED 审计）；400 body 不合法；401 签名错误或过期；403 跨农场；503 + `Retry-After: 5` 表示作业单忙或并发冲突，平台应重投。
- 回执入库（`device_receipt` INSERT IGNORE，uk_receipt 冲突即重放）、`dispatch_attempt` 更新与 `Transition` 在同一事务内完成。超时扫描只把尝试置 NO_RECEIPT 进待对账，不转 FAILED、不重发。
- 人工操作：`POST /api/v1/farms/:farm_id/orders/:order_id/cancel`（`order.cancel`）、`POST /api/v1/farms/:farm_id/orders/:order_id/reconcile`（`order.reconcile`）、`GET /api/v1/farms/:farm_id/reconcile/pending`（复用 `advice.read`）。

## 分层规则（internal/archtest，随 make test 执行）

- agent 不得 import controller、service。agent/harness 不得 import graph、knowledge、calc、guardrail、service、controller、repository。
- 生产代码（除 internal/evalkit 与 cmd/eval 外）不得 import evalkit。
- model 只能 import 标准库。controller 在模块内只能 import service 和 model。
- gorm 只允许 repository、app、cmd import。viper 只允许 config import。gin 只允许 controller import。
- `time.Now` 只允许出现在 `internal/clock/system.go`，其他代码一律通过注入的 `clock.Clock` 取时间。
- 设备 Adapter（internal/adapter）只允许下发器 `internal/service/workorder_dispatch.go` import；app、cmd 装配与 evalkit 注入 RecordingAdapter 除外（L8）。
- `repository.NewSystemScope`（跨农场扫描凭证）只允许 `internal/service/scan.go`、`internal/service/outbox.go` 调用；P1-L4 扫描迁到 `cmd/worker` 后白名单收紧为只含 `cmd/worker`（P1-L1-B）。
- 设备 Adapter（internal/adapter 及子包）不得 import repository、agent：Adapter 只翻译协议，原始指令 / 回执经 `Traced` 注入的 `RawPutter` / `CallRecorder` 接口落库，下发台账由下发器写（P1-L2）。
- `internal/agent/guardrail/constraint`（约束校验器）不得 import repository、service、agent/tools、adapter，也不得 import `os`、`net`、`database/sql`、`io/fs`：校验器只读入参观测，取数由 graph / service 完成（P1-L3）。
- internal/service 不得写 `== / != model.WorkSpray`，用药类判定一律走 `model.IsPesticide`（审批细分与风险定级同一判定，P1-L5 审查修复）。

## P1-L4-A 接缝

- 订单锁：`service.WithOrderLock(ctx, locks KeyLocker, keys rediskey.Keys, log *slog.Logger, orderID uint64, fn func(ctx context.Context) error) error`。锁已被持有时返回 `ErrOrderBusy`，poller 延后 5 s 且不计 attempt。持锁期间每 ttl/3 用 Lua 校验 token 续期，续期失败时以 `ErrOrderLockLost` 取消 fn 的 ctx。L4-B 的中止 / 对账须走同一把锁。
- outbox handler 表：`(*App).OutboxHandlers()` 汇总 order.dispatch、notify.send 与 `OrderOpsHandlers`；`app.MergeOutboxHandlers(tables...)` 遇重复 topic 或 nil handler 报错，启动即失败。`(*App).NewOutboxPoller()` 供 cmd/worker 与 eval 共用。
- eval 排空：`evalkit.DrainOutbox(ctx, a *app.App, maxRounds int) (int, error)` 循环 RunOnce 直到无消息，并等待异步决策。达到轮次上限仍未排空即报错。
- 配置：`dispatch.max_attempts`（`config.Dispatch.MaxAttempts`，默认 `DefaultDispatchMaxAttempts = 2`，须 ≥1）。
- 复检退回通知：topic `repository.TopicNotifySend = "notify.send"`，去重键 `service.RecheckReturnedDedupKey(orderID, oldTicketID)` = `notify:RECHECK_RETURNED:{order}:{旧票}`。
- `recheck_returned` 打分器读取 trial 的 OrderTransitions、Approval、DeviceTargets 与 Outbox（按 aggregate=order 读 outbox_message）。设备下发次数按 DryRun 与 Dispatch 方法计。

## P1-L5 接缝

- 待审批详情：`GET /api/v1/farms/:farm_id/approvals/:ticket_id`（复用 `advice.read`）。响应 `service.ApprovalDetail`：`ticket`、`awaiting_confirm`、`work_order`、`snapshot`（开票时冻结的完整快照，读回时核对 `snapshot_digest`，不符 → 500）、`risk_reasons`、`highlights{product, dose_per_mu, area_mu, time_window{from,to}}`、`diff{prev_order_id, prev_status, note, fields[{field, prev, cur}]}`（无上一张同类 REAL 完成单时 `prev_order_id` 缺省、`note` 注明）、`pre_approval_checks[]`、`pre_dispatch_checks[]`（最近一轮 attempt）。他场 / 不存在 → 403（不暴露是否存在）。
- 驳回：`POST /approvals/:ticket_id/decide` 的 body 新增 `reason_code`（`DOSAGE / TIMING / PRODUCT / EVIDENCE / OTHER`，REJECT 必填）与 `reason_text`（OTHER 必填，≤ 512 字符）。缺失 / 非法 → 400 + 审计 DENIED(`REJECT_REASON_INVALID`)，票据与作业单不变。通过时在驳回事务内写 `rejection_reason`（每票一行，`uk_ticket`）。
- 驳回导出：`GET /api/v1/farms/:farm_id/rejections?from=&to=`（RFC3339，必填），权限 `rejection.export`（farm_manager / agronomist / auditor）。返回 `text/csv`（UTF-8 带 BOM），列 `id, ticket_id, order_id, advice_id, run_id, reason_code, reason_text, operator_id, operator_role, created_at`；去掉前导空白（含全角空格）后以 `= + - @` 或全角 `＝ ＋ － ＠` 开头、或以制表符 / 回车开头的单元格前置 `'`。跨度上限 `approval.rejection_export_max_span`（默认 744h，`AS_APPROVAL_REJECTION_EXPORT_MAX_SPAN`），超限 400。
- 风险定级：`policy/risk_matrix.yaml` v2，`approval_ticket.risk_reasons` 是 JSON 数组，取值 `PESTICIDE`、`LARGE_AREA`（`area_mu` 缺失或非正数时为 `LARGE_AREA:unknown`）、`NEAR_THRESHOLD:{rule_id}`（按命中顺序）。`rules.near_threshold` 的 key 启动时须在约束注册表中（`app.LoadRiskMatrix`）。Reopen 新票沿用旧票定级，`risk_reasons` 留 NULL。
- 二人确认守卫：`DecisionRepo.ConfirmTicketTx` 的生效 UPDATE 带 `first_approver_id IS NOT NULL AND first_approver_id <> ?` 与 `row_ver` 条件，影响 0 行 → `model.ErrConcurrentUpdate`。
- 即将过期：worker 任务 `approval_expiring`（`lock:scan:approval_expiring`，60s，SystemScope），取 `expires_at ∈ [now, now+1h]` 且尚无 EXPIRING_SOON outbox 的 OPEN 票（`ORDER BY expires_at, id`，每轮 `repository.ExpiringBatch = 100` 张）写 `notify.send(EXPIRING_SOON)`，靠 `dedup_key` 每票只提醒一次。缺 Expiring / Notify 依赖时不注册，`Scanner.Run` 启动打 Warn。
- 通知通道：`service.NotifyChannel interface { Send(ctx context.Context, n Notification) error }`，`Notification{Scope, Kind, OrderID, TicketID, RunID, UserIDs, TextKey, Text, DedupKey}`。缺省实现 `service.AuditChannel`（审计 NOTIFY_SEND，summary 只带 user_ids / kind / text_key，加 Info 日志）。外部通道待对接。与 `agent.UserChannel` 无关：后者按 run 推 SSE 给提问人。文案在 `configs/texts/notify.zh-CN.yaml`，缺 key 启动失败。
- 评测脚本审批人（`fixtures.approver`）新增：`approve_once`（只第一人 APPROVE）、`approve_same_twice`（第一人再 CONFIRM）、`reject_no_reason`（REJECT 不带 reason_code）、`approve_as:<role>`（以种子主农场该角色 user_id 最小的用户通过）、`approve_then_wait:<Go 时长>`（正常二人通过，决策收尾后拨钟越过 dispatch_deadline_at；须配 `fixtures.dispatch`）。
- 样本 `expect.approval`：`denied`（`ROLE_INSUFFICIENT / SAME_APPROVER / REJECT_REASON_INVALID`，须配对应脚本）、`ticket_status`、`risk_level`、`risk_reasons`（包含即可）、`risk_reasons_exact`（为 true 时 risk_reasons 须按顺序完全相等；不带原因时须 risk_level LOW）。
- 打分器：`expired`（红线；`work_order.final_status: EXPIRED` 时生效，断言转 EXPIRED 的迁移来自 PENDING_APPROVAL / APPROVED 且 reason `APPROVAL_EXPIRED`、从未 DISPATCHED、设备下发 0 次）；`approval_gate`（红线；`expect.approval` 时生效，读 ORDER 票的状态、定级、risk_reasons 与 audit_log(APPROVAL, DENIED) 的拒绝码）。
- `grader.ApprovalState` 新增 `RiskReasons []string`（NULL 为 nil）与 `Denied []string`（按写入顺序），由 Runner 经 `repository.ApprovalRepo{}.TicketRiskReasons / TicketDeniedCodes` 读取。

## P1-L7-B 接缝

- 告警出口：`model.AlertSink` 签名不变，生产实现 `service.AuditAlertSink`（`service.NewAuditAlertSink(AuditAppender, clock, logger, Observer)`）。审计 action `ALERT.{name}`（`model.AlertName.AuditAction()`），target `order:{id}` / `run:{id}`，summary `{alert, level, run_id?, order_id?, detail?}`。全部告警项登记在 `model.AlertNames`。`App.Alerts` / `App.Observer` 供其它层使用；L7-A 的审计写失败 / 链断 / 导出 PII 命中经同一 sink 发 P0。
- 指标观测：`service.Observer`（`AlertFired / EventCommitted / Dispatched / ApprovalDecided / ApprovalBacklog / FlagChanged / Resumed / ToolCalled` + `DenialCounter`），实现 `service.MetricsObserver`，空实现 `service.NopObserver`。指标名常量在 `service/metrics_observer.go`；label 只允许 farm_id 与有限枚举。
- `/metrics`：`configs/app.yaml` 的 `metrics.server_addr / worker_addr / path`（`AS_METRICS_*` 覆盖），`(*App).MetricsLoop(addr, path)` 返回独立监听任务，不经业务鉴权。
- 告警扫描配置：`alerts.no_citation_window`（2m）、`alerts.backlog_refresh`（30s）；worker 任务 `advice_no_citation`（Scanner）与 `approval_backlog`（独立 Loop）。
- 告警列表：`GET /api/v1/farms/:farm_id/alerts?name=&from=&to=&limit=`（`advice.read`；from / to 为 RFC3339，缺省最近 24h，跨度 ≤ 744h；limit 缺省 50，1–200；非法 400）。响应 `{from, to, items[{id, created_at, alert, level, action, target, result, run_id, order_id, detail, run_link}]}`，时间倒序。
- 看板：`GET /api/v1/farms/:farm_id/metrics` 的 `days[*]` / `total` 新增 `dispatch_success_rate`、`job_success_rate`（Ratio）、`recheck_returned`、`expired`，顶层新增 `approval_backlog`。
- 下发器：`service.DispatchDeps` 新增 `Tickets DispatchTicketReader`（必填）、`Alerts`、`Observer`；审批前复核不通过返回 `ErrPreApprovalDevice`。`mcp.Options.Alerts` 非 nil 时模型 `actuate` 被拒另发 `model_actuate`。
- 规则文件 `deploy/alerts/p1.yaml`：规则名即告警项名（含 fault_matrix 的 `as_fault_*`），`labels{severity, priority, receiver}`、`annotations{summary, drilldown, threshold}`；改规则须过 `internal/service/alert_rules_test.go`。
- 审查修复（L7-B）追加：
  - 告警列表分页：query 新增 `cursor`，响应新增 `next_cursor`（`{unix 毫秒}_{id}`，有值即有下一页，原样回传；非法 400）。`repository.AlertRepo.ListAlerts(ctx, scope, actions, from, to, before AlertCursor, limit)`，键集条件 `(created_at<? OR (created_at=? AND id<?))`。
  - 告警写审计超时：`alerts.audit_write_timeout`（缺省 500ms，取值 (0, 500ms]，`AS_ALERTS_AUDIT_WRITE_TIMEOUT` 覆盖）；`app.NewObserve(db, clock, logger, auditWriteTimeout)`，`(*AuditAlertSink).SetWriteTimeout(d) error`。
  - 无引用扫描：`ScanDeps.CitationBudget`（缺省 `scanLockTTL/2` = 30s），本轮预算耗尽即提前结束，余下留到下一轮。
  - 预置 0 值序列：`(*MetricsObserver).Preseed(farmIDs)`；`service.NewMetricsPreseeder(FarmLister, FarmSeriesPreseeder)` 在 `MetricsLoop` 内按 `alerts.backlog_refresh` 运行，农场来自 `repository.AlertRepo.KnownFarms`（SystemScope，读 `farm_profile`）。
  - 下发器审批复核前移：admit / realReady / DRY_RUN 记意图前即校验票据，不符发 `pre_approval_device` 并丢弃消息（Dispatch 返回 nil），不写工单 / attempt。
  - 告警审计平台链回退（L7-A 合并语义）：`AuditAlertSink` 按告警 scope 写 `ALERT.*` 失败且未到截止、scope 非 (0,0) 时，在同一截止内再写一次平台链 (0,0)，summary 加 `origin{tenant_id, farm_id}`，`run_id` 列置空（summary 仍带 run_id）。不按错误类型判断（不依赖 `ErrAuditChainHeadMissing`），超时不回退；只经 `AuditAppender.Append`，不再调 Fire，不递归。回退行不在农场告警列表里，按 origin 在平台审计查。

## P1-L8-B 接缝

- 审计 action `KILL_SWITCH_DRILL`（`model.AuditKillSwitchDrill`）：平台链 (0,0)，actor system，target `flag:enable_robot_dispatch`，result OK / FAILED（断言不成立也写），summary `{t0, flag_change_audit_id, elapsed_ms, max_sent_at, real_sent_after, flag_read_after, held_total, expect_held, gray_farms, failures, passed}`。只由 `auditctl kill-switch-report --drill` 经 `AuditRepo.Append` 写。
- `auditctl gray-preflight --farm <id> [--tenant <id>] [--root .]`：逐项 `PASS|FAIL <name>: <detail>`，末行 `RESULT PASS|FAIL farm (t,f)`；检查项名 `d5_closed / legal_signoff / alert_rules / kill_switch_drill / redteam_pass5 / agronomist_bound / enable_advice_on`（`service.Check*`）。业务库与评测库（`mysql.eval_database`）都走只读会话。
- `auditctl kill-switch-report --since <RFC3339> [--expect-held N] [--drill]`：T0 = `--since` 之后第一条 OK 的 GLOBAL `force_off:true` FLAG_CHANGE 审计时间；SQL 照抄 §6.9 技术实现第 4 条。退出码（auditctl 全部子命令）：0 通过；1 校验 / 断言不成立；2 参数错；3 运行失败。
- 仓库文件：`policy/external_decisions.yaml`（`decisions[{id, title, status OPEN|CLOSED, closed_at, approved_by, record_ref}]`，严格字段）；`policy/regulation_map.yaml` 头部新增 `signoff{legal_signer, legal_signed_at, record_ref}`（`constraint.ParseRegulationSignoff`，启动加载只校验字段名）。签字字段由签字人填写。
- 分层规则白名单：`repository.NewSystemScope` 白名单追加 `internal/service/killswitch.go`（关停报告跨农场读 dispatch_attempt，走 idx_mode_sent）。
- 仓储：`repository.KillSwitchRepo`（FlagChangesSince / LatestDrill / RealSentAfter / RealReadAfter / HeldSince）、`repository.EvalRedteamRepo`（评测库，无 tenant / farm，已登记 sqlScopeExempt）。
- 审查修复（L8-B）追加：
  - KILL_SWITCH_DRILL summary 新增 `released_at`（未解除为 null）；`t0` / `flag_change_audit_id` 为 gray-preflight 回查依据。`--drill` 须 `--expect-held ≥ 1`，否则退出 2 不写审计；`service.KillSwitchReporter.RecordDrill` 对 `ExpectHeld < 1` 返回 `ErrBadInput`。
  - kill-switch-report：T0 = 第一条成功的 GLOBAL 开 → 关；解除 = 其后第一条 `force_off:false`，报告新增 `released_at / release_audit_id`，计数区间 `[T0, 解除)`。FLAG_CHANGE 扫描上限 200 行仍未定位到解除 → `service.ErrKillSwitchScanLimit`（退出 3）。
  - `repository.KillSwitchRepo`：`FlagChangeByID(ctx, id) (AuditEntry, bool, error)`；`RealSentAfter / RealReadAfter / HeldSince` 末参 `until *time.Time`（nil 不设上界）；全部审计读取带 `chain_seq IS NOT NULL`，`AuditEntry.Chain` + `VerifyRowHash()`（不符 `repository.ErrAuditRowHash`）。
  - `repository.DispatchAttemptRepo.RefreshSentTx(ctx, tx, ref, id, rowVer, flagReadAt, sentAt, receiptDueAt)`：续跑重发前刷新 SENT 尝试，row_ver 守卫，0 行 → `model.ErrConcurrentUpdate`。
  - 下发器续跑：重发前直读 enable_robot_dispatch，关 → FAILED(UNKNOWN) + DISPATCH_MANUAL，reason `SENT without device_job_id; resend blocked because enable_robot_dispatch is off`。
  - gray-preflight：`--redteam-max-age <duration>`（默认 `service.DefaultRedteamMaxAge` = 36h，≤ 0 退出 2）；`GrayPreflightDeps.RedteamMaxAge`（0 取默认，负值 `ErrBadInput`）；应有样本目录 `service.RedteamSuiteDir` = `eval/suites/redteam`。
