# 一键关停（enable_robot_dispatch 全关）值班手册

依据：P1 执行手册 §8.3 回滚触发条件、§8.4 回滚步骤、§6.9 技术实现第 4 条。演练记录写 `docs/p1/一键关停演练记录.md`。
状态：**待演练**（预发未执行，Prometheus 加载规则与截图未确认）。

## 0. 何时下达（§8.3，任一命中）

| 触发 | 动作 |
|------|------|
| 任一 P0 级告警（建议无引用、护栏被绕过） | 立即全关 |
| 任一安全事故或疑似误下发 | 立即全关，冻结放量直到复盘结论出具 |
| 下发失败率 / 恢复失败率 / MCP 成功率超阈值且 1h 未恢复 | 回退一档（只改对应农场 FARM 行 `{"on": false}`） |
| 审计链校验失败（`auditctl verify` / `anchor-check` 退出码 1） | 立即全关，排查前不恢复 |
| 夜间 `redteam` 出现红线失败 | 停止放量（不必全关），修复并重跑通过后再继续 |

由 SRE 值班执行，Owner 事后确认；全程只走管理接口（L0 流程），**不得直接改 `feature_flag` 表**（直改不写 FLAG_CHANGE，报告找不到 T0）。

## 1. 下达全关

```
PUT /api/v1/admin/flags/enable_robot_dispatch/scopes/0/0
{"value": {"force_off": true}, "comment": "<触发项 + 告警 / 事故编号>"}
```

- 需 platform_admin。GLOBAL 行只接受 `force_off`，写 `{"on": true}` 返回 400 `FLAG_VALUE_INVALID`。
- 记下请求完成的时刻（墙钟，精确到秒），作为下一步 `--since` 的下界（取稍早一点即可）。

## 2. 确认 FLAG_CHANGE

```sql
SELECT id, created_at, result, JSON_EXTRACT(summary, '$.new') AS new_value
FROM audit_log
WHERE tenant_id = 0 AND farm_id = 0 AND action = 'FLAG_CHANGE' AND target = 'flag:enable_robot_dispatch'
ORDER BY created_at DESC LIMIT 3;
```

最新一行 `result = OK`、`new_value = {"force_off": true}`，其 `created_at` 即 T0。没有这一行：重新执行第 1 步，不要继续。

## 3. 跑报告（只读）

```
go run ./cmd/auditctl kill-switch-report --since <T0 前的 RFC3339 时刻> [--expect-held N]
```

- 密钥只从环境变量取（`AS_MYSQL_PASSWORD` 等），不读 `deploy/.env`。演练时加 `--drill`，额外写一条平台链 KILL_SWITCH_DRILL 审计；
  `--drill` 必须同时给 `--expect-held N`（N ≥ 1，预置且未发出的 REAL 单数），否则参数错退出 2，不写审计。
- T0 = `--since` 之后第一条成功的 GLOBAL 开 → 关（关 → 关的重复写不算）；其后第一条 `force_off:false` 为解除时刻，
  三项计数都只统计 `[T0, 解除)`，解除后重跑报告不会把恢复后的正常下发算进来。未解除时计到当前。
- 输出：`max(sent_at WHERE mode='REAL') − T0`（≤ 60000ms）、T0 后发出的 REAL 条数、其中 `flag_read_at >= T0` 的条数（必须 0）、
  各灰度农场 T0 后 DISPATCH_HELD 之和（`--expect-held` 给出时须 ≥ N）。T0 与 `flag_read_at` / `sent_at` 分别来自 API 与 worker 的时钟，
  演练前在两台主机执行 `chronyc tracking`（或 `timedatectl timesync-status`），把 NTP 同步状态与偏差（System time offset）贴进演练记录；
  偏差 > 100ms 先校时再演练，否则 `flag_read_at >= T0` 的判定不可信。
- 续跑：T0 前已写 SENT、未拿到 device_job_id 的尝试，T0 后被重投时下发器重读开关，读到关闭即转 `FAILED(UNKNOWN)` 并写
  DISPATCH_MANUAL（reason 含 `resend blocked because enable_robot_dispatch is off`），不重发；按第 4 步第 4 条人工对账。
- 退出码：0 全部成立；1 有断言不成立（`RESULT FAIL`，逐条 `FAIL …`）；2 参数错；3 运行失败（连库等）。
- 退出码 1：保持全关，按 §8.3 第 2 条视为疑似误下发，升级 Owner，把输出贴进复盘。

## 4. 在途作业单（§8.4 第 2 条）

关停**不会**自动取消在途单，逐单处理：

1. 未下发（`APPROVED`）：不处理。下发器已写 DISPATCH_HELD、outbox 置 DONE；到 `dispatch_deadline_at` 由扫描转 `EXPIRED`，解除后不补发。
2. 重试中：下发器重试时读到关闭，转 `FAILED`（`RETRY_BLOCKED`），不处理。
3. 已下发（`DISPATCHED`，有回执）：逐单 `POST /api/v1/farms/{farm_id}/orders/{order_id}/cancel`，按 L4 路径回填 `done_area_mu`。
4. 无回执：保持 `DISPATCHED`，走 L4 人工对账（`make reconcile` 由对账值班执行，不在本手册内）。

列在途单（按农场）：

```sql
SELECT id, status, dispatch_mode, dispatch_deadline_at FROM work_order
WHERE tenant_id = ? AND farm_id = ? AND dispatch_mode = 'REAL' AND status IN ('APPROVED', 'DISPATCHED');
```

## 5. 确认降级正常（§8.4 第 3 条）

- 新建作业单 `dispatch_mode = DRY_RUN`。
- 建议照常生成（`enable_advice` 不受影响），审批照常。

## 6. 解除

前提：触发原因已排除，Owner 批准；**重新执行 §6.9 灰度前检查**：

```
go run ./cmd/auditctl gray-preflight --farm <id> [--tenant <id>] [--redteam-max-age 36h]
```

任一 `FAIL` 不得解除。redteam 项要求最近一次 redteam 实验在 `--redteam-max-age`（默认 36h）内完成、覆盖 `eval/suites/redteam/**`
全部样本且每样本 ≥ 5 次 trial 全过、无隔离样本，输出带 git_sha。全部 PASS 后：

```
PUT /api/v1/admin/flags/enable_robot_dispatch/scopes/0/0
{"value": {"force_off": false}, "comment": "<复盘编号>"}
```

解除后只有 FARM 行为 `{"on": true}` 的农场恢复，不需要逐场重开；被 HELD 的单不补发。再按第 2 步确认 FLAG_CHANGE。

## 7. 复盘（§8.4 第 4 条）

- 24h 内按 `run_id` / `order_id` 拉取相关链路，输出复盘，结论写入风险清单。
- 附：第 3 步报告输出、FLAG_CHANGE 审计 id、在途单处理清单、截图位置。
