# 线上回流草稿（P2 L8）

`backflow-collect`（worker 每周任务，`lock:p2job:backflow-collect`）把差评、专家判错、护栏拦截、高频 KB_MISS 拒答的 run 生成样本 fixture 草稿，写到 `configs/app.yaml` 的 `attribution.backflow.drafts_dir`（缺省本目录下 `drafts/`）。

仓库只提交本 README 与 `drafts/.gitkeep`、`drafts/.gitignore`，**不提交真实草稿**（`drafts/.gitignore` 忽略目录内其余文件，防误提交）。

## 流程

1. 选样：上一 ISO 周本农场的差评、专家判错（`expert_score <= 1`）、护栏拦截（`VALIDATION_FAILED`），以及同问题类型一周达到 `kb_miss_min_hits` 次的 KB_MISS 拒答（阈值为草拟值，待线上数据）。驳回不进回流。每农场每周最多 `max_runs` 个，置 `agent_run.backflow_status=SELECTED`。
2. 取原文：以服务账号（`service_user_id`，auditor）逐条调 `AuditService.ReadRaw`，每条写一条 `RAW_READ` 审计；提问与时刻取 `audit_log` 的 ASK，`knowledge_ref` 取 KB_LOAD。
3. 脱敏：身份、手机号、证件号换随机合成值（`syn-…`），坐标粗化到 0.01°，`farm_id / field_id / user_id` 等数值 ID 换随机合成码（`farm-bf-…`、`field-bf-…`、`u-bf-…`）。真实 ID 与草稿的对应只写业务库 `agent_run.backflow_draft_ref`，草稿、评测库、评测仓都不留映射。
4. 兜底扫描：每个桩返回过 `piiclean.FirstLeak`，草稿逐行过 `piiclean.Detect`，再查真实 ID 是否残留。通过则写 `drafts/{draft_ref}.yaml` 并置 `DRAFTED`；不过则置 `PII_FAILED`，不写文件。取原文或出草稿出错（原文过期除外）记一次失败尝试，第 3 次置终态 `FAILED`，不再占每周名额。
5. 入评测库：`go run ./cmd/eval backflow import --dir eval/backflow/drafts` 按样本格式严格解码每份草稿、再跑一次 pii_clean，为通过的草稿在评测库建 `review_task(BACKFLOW_EXPECT)`（按 `draft_ref` 幂等）。
6. 专家补写 `expect`、`context.role` 并署名后移入 `eval/suites/capability/`，状态推进到 `MERGED`；判定不入库则 `DROPPED`。这一步由专家流程触发（待专家）。

## 草稿格式

字段与 `internal/evalkit/sample` 的样本一致：`id` 即 `draft_ref`，`suite: capability`，`review_status: pending_expert`，`expect: {}`，`fixtures.clock` 为提问时刻（农场时区），`fixtures.mcp` 每个工具一个桩（`match.field` 为合成田块码；同一工具多次调用按次序写 `returns`）。
