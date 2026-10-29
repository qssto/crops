# 指标口径（P0 L7）

接口：`GET /api/v1/farms/{farm_id}/metrics?from=YYYY-MM-DD&to=YYYY-MM-DD`（Bearer；UTC 自然日，含首尾，缺省当天，最多 31 天）。
实现：事实行 SQL 在 `internal/repository/metrics.go`，聚合在 `internal/service/metrics.go`（纯函数 `computeMetrics`）。
返回 `days`（稠密序列，无数据的日子也有一行）+ `total`（窗口合计）。比率一律 `{num, den, value}`，分母为 0 时 `value=null`。

## 归日规则

| 事实 | 归到哪一天 |
|---|---|
| 会话类（会话数、完成、校验失败、恢复、token） | `agent_run.created_at` 所在日（队列口径：只看窗口内创建的 run） |
| 工具调用 | `tool_call.created_at` |
| 建议 / 拒答 | `advice_card.created_at` |
| 审批等待 | `approval_ticket.decided_at` |
| 作业单 | `work_order.created_at` |

所有 SQL 均带 `tenant_id=? AND farm_id=?`，不带库名前缀，只读生产库连接（`agri_steward`）；评测库 `agri_eval` / `agri_eval_t_{n}` 读不到（单测 `TestReadPathSQL_NoCrossSchema`）。

## 指标

| 指标（JSON 键） | 定义 | 分子 / 分母 |
|---|---|---|
| `sessions` | 窗口内创建的 run 数 | — |
| `completion_rate` | 会话完成率 | `agent_run.status='COMPLETED'` / 会话数 |
| `tool_success_rate` | 工具成功率 | `tool_call.status='OK'` / 全部 tool_call 行（含重试 attempt） |
| `tool_p95_latency_ms` | 工具 P95 延迟 | 全部 tool_call 行的 `latency_ms`，最近秩：升序第 ⌈0.95·n⌉ 个 |
| `refusal_rate` | 拒答率 | `advice_card.outcome='REFUSED'` / 建议卡数 |
| `validation_failure_rate` | 校验失败率 | 有任一 `VALIDATION_FAILED` 事件的 run / 会话数 |
| `approval_wait` | 审批等待（OPEN → 决策，秒） | 票 `ticket_kind='ORDER'`、`status IN (APPROVED, REJECTED)`；OPEN 时间取该票 `APPROVAL_PENDING` 事件（与 PREPARING→OPEN 同事务）的 `created_at`；给 `decided`、`avg_sec`、`p95_sec`（最近秩）；缺事件的票计 `missing_open`，不进均值 |
| `resume_success_rate` | 恢复成功率 | 存在 seq 大于首个 `RUN_RESUMED` 的 `RUN_COMPLETED` 的 run / 有 `RUN_RESUMED` 的 run |
| `tokens` | token 成本 | `LLM_CALL_END` payload 的 `prompt_tokens` / `completion_tokens` 求和；`per_session = total / 会话数`；解析失败的事件计 `bad_payload` |
| `jobs.orders` | 作业单数 | 只计 `dispatch_mode='REAL'` |
| `jobs.dispatch_rate` | 下发率 | 曾迁移到 `DISPATCHED`（`work_order_transition.to_status`）的 REAL 单 / REAL 单 |
| `jobs.done_rate` | 完成率 | `status IN (DONE, PARTIALLY_DONE)` 的 REAL 单 / REAL 单 |
| `jobs.dry_run_excluded` | DRY_RUN 单数 | 只计数，不进任何比率（P0 全部为 DRY_RUN，作业比率恒为 `null`） |

## SQL

参数 `?` 依次为 tenant_id、farm_id、窗口 `[from, to)`（to = 末日 + 1 天）。

```sql
-- 会话队列
SELECT id, status, created_at FROM agent_run
 WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<? ORDER BY id;

-- 队列 run 的事件（run_event 按 created_at 分区：事件不早于 run 创建，e.created_at>=from 用于裁分区）
SELECT e.run_id, e.seq, e.event_type, e.payload FROM run_event e
 JOIN agent_run r ON r.id=e.run_id AND r.tenant_id=e.tenant_id AND r.farm_id=e.farm_id
 WHERE e.tenant_id=? AND e.farm_id=? AND e.created_at>=? AND r.created_at>=? AND r.created_at<?
   AND e.event_type IN ('VALIDATION_FAILED','RUN_RESUMED','RUN_COMPLETED','LLM_CALL_END') ORDER BY e.run_id, e.seq;

-- 工具调用
SELECT status, latency_ms, created_at FROM tool_call
 WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<? ORDER BY id;

-- 建议卡
SELECT outcome, created_at FROM advice_card
 WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<? ORDER BY id;

-- 已决策的票 + OPEN 时间
SELECT t.id, t.status, t.decided_at,
  (SELECT MIN(e.created_at) FROM run_event e WHERE e.tenant_id=t.tenant_id AND e.farm_id=t.farm_id AND e.run_id=t.run_id
     AND e.event_type='APPROVAL_PENDING' AND CAST(JSON_UNQUOTE(JSON_EXTRACT(e.payload,'$.ticket_id')) AS UNSIGNED)=t.id) AS opened_at
 FROM approval_ticket t
 WHERE t.tenant_id=? AND t.farm_id=? AND t.ticket_kind='ORDER' AND t.status IN ('APPROVED','REJECTED')
   AND t.decided_at>=? AND t.decided_at<? ORDER BY t.id;

-- 作业单 + 是否曾下发
SELECT o.id, o.status, o.dispatch_mode, o.created_at,
  EXISTS (SELECT 1 FROM work_order_transition w WHERE w.tenant_id=o.tenant_id AND w.farm_id=o.farm_id
     AND w.order_id=o.id AND w.to_status='DISPATCHED') AS dispatched
 FROM work_order o WHERE o.tenant_id=? AND o.farm_id=? AND o.created_at>=? AND o.created_at<? ORDER BY o.id;
```

## 对账用的直接 SQL（单日 D，`[D, D+1)`）

```sql
SELECT COUNT(*), SUM(status='COMPLETED') FROM agent_run WHERE tenant_id=1 AND farm_id=1 AND created_at>=? AND created_at<?;
SELECT COUNT(*), SUM(status='OK') FROM tool_call WHERE tenant_id=1 AND farm_id=1 AND created_at>=? AND created_at<?;
SELECT COUNT(*), SUM(outcome='REFUSED') FROM advice_card WHERE tenant_id=1 AND farm_id=1 AND created_at>=? AND created_at<?;
SELECT COUNT(DISTINCT e.run_id) FROM run_event e JOIN agent_run r ON r.id=e.run_id
 WHERE e.tenant_id=1 AND e.farm_id=1 AND e.event_type='VALIDATION_FAILED' AND r.created_at>=? AND r.created_at<?;
SELECT COUNT(DISTINCT e.run_id) FROM run_event e JOIN agent_run r ON r.id=e.run_id
 WHERE e.tenant_id=1 AND e.farm_id=1 AND e.event_type='RUN_RESUMED' AND r.created_at>=? AND r.created_at<?;
SELECT COALESCE(SUM(CAST(JSON_EXTRACT(e.payload,'$.prompt_tokens') AS SIGNED)
  + CAST(JSON_EXTRACT(e.payload,'$.completion_tokens') AS SIGNED)),0)
 FROM run_event e JOIN agent_run r ON r.id=e.run_id
 WHERE e.tenant_id=1 AND e.farm_id=1 AND e.event_type='LLM_CALL_END' AND r.created_at>=? AND r.created_at<?;
SELECT COUNT(*) FROM approval_ticket WHERE tenant_id=1 AND farm_id=1 AND ticket_kind='ORDER'
 AND status IN ('APPROVED','REJECTED') AND decided_at>=? AND decided_at<?;
SELECT dispatch_mode, COUNT(*) FROM work_order WHERE tenant_id=1 AND farm_id=1 AND created_at>=? AND created_at<? GROUP BY dispatch_mode;
```

`cmd/reconcile` 自动跑上面这组 SQL 并与 `MetricsService.Daily` 逐项比较，见 `docs/reconcile/L7.md`。
