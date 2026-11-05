# P2 第 4 批简报：L7-b ∥ L8（两个子 agent 并行，分别提交 P2-L7b / P2-L8）

先读：`docs/agent-brief-common.md`、`docs/briefs/P1-common.md`、`docs/briefs/P2-common.md`；`docs/dev-log.md` 中 `## P2-L0`、`## P2-L2`、`## P2-L7a`、`## P2-L3` 各节（尤其「待办」）；L8 另读 `## P2-L5`（`kb_load_gaps.json`）与 `## P2-L4`（驳回原因待办）。
基线分支：`p2-b4-base`（= main 上第 3 批 P2-L3 / L4 / L5 / L6 提交 + 本简报）。开工先 `git merge --ff-only p2-b4-base`。
迁移：业务库 `0016_feedback_attribution`（L8，照 §6.9 ② 定版 SQL）已由调度方写在主仓 `migrations/`（gitignored）。开工把主仓 `migrations/` 整个复制到 worktree。L7-b 不加迁移（`experiment_gate` 的四列 L7-a 已加）。
命名空间：L7-b 用 `AS_TEST_NS=p2l7b`，L8 用 `AS_TEST_NS=p2l8`。集成测试只写 `{ns}_` 前缀的隔离库，不写共享 `agri_steward` / `agri_eval`，不碰其他前缀的库。
上下文：你的上下文有限。读大文件用 offset / grep；分小步提交。
跑测试不要设 `GIT_DIR` / `GIT_WORK_TREE`（cmd/eval 测试会建临时 git 仓，曾经因此写坏主仓）。`cmd/eval` 集成测试要 `-timeout 30m`。

## 共享文件（只追加，各写各的 case / 段落 / 新文件，调度方合并）
`cmd/eval/main.go`（子命令 switch 各加 case，实现放新文件）、`Makefile`、`docs/dev-log.md`（`## P2-L7b …` / `## P2-L8 …`）、`eval/README.md`、`internal/evalkit/store`（各开新文件）、`configs/app.yaml` 与 `internal/config/config.go`（各加自己的段与 setXxxDefaults）、`internal/app/app.go` 的 Deps（各加一个字段）、`policy/permission_matrix.yaml`（L8 加动作）。

---

## L7-b：质量门禁、成本延迟门禁、配对报告、模拟用户 —— P2 §6.8（670-716 行）功能任务 5-10、技术实现 ① 4-6
文件归属：`cmd/eval gate`（新子命令，`cmd/eval/gate*.go`）、`internal/evalkit/gate/`（新包：判定规则注册表）、`internal/evalkit/compare` 的配对报告扩展、`internal/evalkit/usersim/`（新）、`eval/gates/` 下新配置、`eval/signoffs/`、`.github/CODEOWNERS` 与 workflows 中 gate 步骤、capability 下 `user_sim` 样本。

1. `cmd/eval gate --exp <candidate> --baseline <name|id>`（技术实现 4）：按顺序判定 `BLOCK_REDLINE`（红线 pass^k < 1）→ `BLOCK_QUALITY`（引用覆盖率或数值准确率 < 100% 无条件；或 `all` 切片任一可信度指标下降超 `thresholds.yaml` 阈值且配对 bootstrap 区间不跨 0）→ `NEED_SIGNOFF`（成本 / P95 / 压缩触发率劣化超阈值）→ `PASS`；结果写 `experiment.gate_verdict`。判定步骤用注册表（名字 → 判定函数，有序），以后加规则只加注册项。切片结果只出报告不判定。`compare_mode=PHASE` 的实验不写 `gate_verdict`；VOID / BUDGET_EXCEEDED 拒绝给结论（沿用 L7-a）；多因子差异（L0 因子检查）→ `NOT_ATTRIBUTABLE`。flaky 样本按样本级均值参与配对并在报告单列。复用现有 compare / reduce 的配对与 bootstrap，不另写一套。
2. judge 维度（任务 7、降级规则 3）：`eval/gates/judges.yaml` 中校准达标（L2 的 `cmd/eval judge calibrate` 报告区间下限 ≥ 阈值）的维度才进门禁；judge 整体不可用或维度「未评」时跳过该维度，结论上限 `NEED_SIGNOFF`，不得为 `PASS`。没有真实模型与专家：三维度均未达标，门禁里 judge 全部「未评」，用构造数据测试达标与不达标两条路径。标「待真实模型」「待专家」。
3. 成本与延迟（任务 8）：单会话 token 成本（L6 的 `prompt_tokens + completion_tokens` 口径，以报告 / trial 里已有字段为准）、P95 延迟、压缩触发率三项，阈值放 `eval/gates/cost_latency.yaml`（草拟值，标「待真实模型」，signed_by 留空）。桩模型 token 为 0、延迟不稳：判定逻辑用构造的 trial 数据测试；P95 用 stub 实测时阈值要宽到不误报（或在 stub 下只出报告，写清楚取舍）。
4. 放行（技术实现 5）：`NEED_SIGNOFF` 时读 `eval/signoffs/{experiment_id}.md`，作者须在 `.github/CODEOWNERS` 中（本地用 `git log` 取提交作者，CI 同样），校验通过后写 `signed_by`。CODEOWNERS 新建，只放占位的评测负责人 / Owner 账号，标「待真实账号」。
5. 配对报告（任务 9）：门禁结论放第一行；按样本列翻转（退化 / 改进）、flaky 清单、`unexpected_call` 清单；holdout 样本只给 ID（沿用 L2）。
6. 发版门禁（任务 5）：`make eval MODE=release` 跑完后调 `gate`（对 `BASELINE`，缺省 baseline_p1）；非 PASS / 未签字的 NEED_SIGNOFF → 非 0 退出。`release.yml` 同步；PR 冒烟保持 L7-a 的红线判定不变。
7. LLM 模拟用户（任务 10、技术实现 6）：`internal/evalkit/usersim` 按评测设计方案 §9.2 实现 `persona / instruction / max_turns`、输出 `###STOP###` 结束，作为与脚本用户同接口的用户通道注入 runner；模拟用户模型独立配置（`eval.usersim.model` / tier，与智能体、judge 分开）。没有真实模型：录制回放客户端（同 L2 judge 的做法，按 `(sample_id, turn, 对话哈希)` 读录制，缺录制即该 trial 记 infra_error 而不是编造回复）。首批 3-5 条多轮样本放 `capability`，`tags` 含 `user_sim`，`n ≥ 5`；样本 `notes` 留「入库前人工阅读 3 次 trial 对话」记录位，标「待人工阅读」。红线门禁（smoke 清单、gate 的红线判定）排除 `user_sim` 样本，加测试（验收 4）。
8. 验收（§6.8 L7-b 部分）：第 2 条用构造的退化 candidate（例如在隔离库里写一组人为降分的 trial，或注入一个劣化打分结果）证明 gate 判 `BLOCK_QUALITY` 且报告列出翻转样本；第 4 条测试；第 5 条标「待真实 PR」。门禁判定属红线代码，每项先写应失败测试。
9. 不做：线上灰度、Gate 2 收口文档。

---

## L8：驳回结构化、采纳 / 差评、归因周报、线上回流 —— P2 §6.9（717-800 行）功能任务 1-5、技术实现 ① 1-4、② SQL
文件归属：`internal/service` 中驳回写入处（只改 kb_ids 一处）与新文件 `feedback*.go` / `attribution*.go` / `backflow*.go`、`internal/repository` 新文件（`feedback.go`、`attribution.go`、`backflow.go`）、`internal/model` 新文件、`internal/controller` 新 handler 与路由一行、`internal/app/attribution_l8.go`（worker 任务登记，照 L3 / L4 调 `internal/app/worker_p2.go` 的 `registerP2Loops`）、`contracts/`（反馈请求 / 回流草稿 schema）、`eval/backflow/`、`cmd/eval` 的 `backflow import` 子命令。L8 不碰门禁、usersim、compare。

1. 驳回 kb_ids（功能任务 1）：审批驳回写 `rejection_reason` 时在**同一事务**按 `advice_citation` 取该建议卡引用的 kb_id 写 `kb_ids`（JSON 数组，去重排序）；无引用写 `[]`。「其他」须填文字已有（`service/decision.go`），加测试确认不回退。先写应失败测试。
2. 采纳 / 差评 API（技术实现 1）：`POST /api/v1/farms/{farm_id}/advices/{advice_id}/feedback`，body `{verdict: ADOPT|DISLIKE, category?, comment?, request_id}`；DISLIKE 时 category 必填且属驳回五类；`request_id` 冲突返回首次结果（`uk_request`）；`comment` 落库前 `pii_mask`；advice 须属同 tenant / farm（跨农场 404，沿用 P1 的隔离写法）；`run_id` 从建议卡取，不信请求。`policy/permission_matrix.yaml` 加动作（如 `advice.feedback`），角色按现有建议查看权限给；未声明即拒绝的测试。请求 schema 放 `contracts/`。
3. 归因周报 `attribution-weekly`（技术实现 2）：worker 任务，每周一（按注入时钟、`configs/app.yaml` 新增 `attribution` 段：`enabled: false` 缺省、时区、Top N），取 `lock:p2job:attribution-weekly`（照 L3 job 的锁与 done 标记写法）。来源：上周 `rejection_reason`、`advice_feedback` DISLIKE、`VALIDATION_FAILED` 事件、`refuse_reason=KB_MISS` 拒答、专家判错（`online_judge_result`；`review_task` 在评测库，业务 worker **不连评测库**，这一来源标「待调度方决定 server 连评测库」，接口留好、本期不接）。`signature = md5(原因 | 问题类型 | 主 kb_id)`；upsert 照 ② 的 `ON DUPLICATE KEY UPDATE`（`reopen_count` 写在 `status` 前，加测试证明 RESOLVED → 再命中 → OPEN 且 reopen_count+1）；平台行 `tenant_id=0, farm_id=0`，archtest SystemScope 白名单只追加 L8 job 文件一行。`evidence_refs` 只留最近 50 条。周报 HTML 存 `agri-steward-eval/report/weekly/{week}.html`（week = ISO 周 `2026W45` 形如 char(8)），内容：Top 问题（负责人 / 处置 / 状态）、上期处置回填、重开问题单列、本周回流量与入库量。L5 的 `kb_load_gaps.json` 作为离线输入在周报附一节（有就读，无就写「无」）。
4. 处置更新（技术实现 3）：service + controller 接口 `PATCH .../attribution-issues/{id}`（平台级，运营角色，permission_matrix 加动作），`disposition / owner_id / resolution_ref / status`，带 `row_ver` 乐观锁（`WHERE id=? AND row_ver=?`，0 行 → 409）。处置枚举照 ②。不做 amis 页面（amis JSON 不提交）。
5. 回流 `backflow-collect`（技术实现 4）：worker 任务每周，取 `lock:p2job:backflow-collect`。选 run：差评、专家判错（同 3 的限制）、护栏拦截（`VALIDATION_FAILED`）、高频 KB_MISS（阈值放配置，草拟值标「待线上数据」）；置 `backflow_status=SELECTED`。以服务账号调 `service.AuditService.ReadRaw`（`internal/service/audit.go:88`，每次写 `RAW_READ` 审计，测试断言审计条数），与 `audit_log` 的 `clock`、`knowledge_ref` 生成样本 fixture 草稿（字段对齐 `internal/evalkit/sample` 的 fixtures，`expect` 留空待专家）。脱敏：身份 / 手机号随机合成值，坐标粗化到 0.01°，`farm_id / field_id` 换随机合成码；映射只写 `agent_run.backflow_draft_ref`。草稿过 `pii_clean`（复用 `internal/piiclean`，即样本扫描 `sample/piiscan.go` 与打分器共用的检测，不另写）→ `DRAFTED`，不过 → `PII_FAILED`。草稿写 `eval/backflow/drafts/{draft_ref}.yaml`（任务产出到配置的目录，测试写 tmp 目录；仓库只提交 README 与 .gitkeep，不提交真实草稿）。`review_task(BACKFLOW_EXPECT)` 不由业务 worker 写：`cmd/eval backflow import --dir eval/backflow/drafts` 读草稿、再跑一次 pii_clean、在评测库建 review_task（`draft_ref` 列已有）。`MERGED / DROPPED` 状态推进留接口，由专家流程触发，标「待专家」。
6. 时间跨度（验收 1-3）：用注入时钟在隔离库连续跑 4 个周（构造驳回 / 差评 / 拦截数据），证明每周出周报、问题跨周只一行、处置回填、重开计数；验收 2（≥ 10 条知识条目因清单补齐）标「待线上数据 / 待专家」，以 4 周构造数据里 Top 清单能定位到 kb_id 代替。
7. 红线：跨租户 / 跨农场隔离（反馈、回流选 run）、回流草稿无真实 ID / 手机号 / 精确坐标（pii_clean 与专门断言）、评测库与评测仓无映射。每项先写应失败测试。
8. 迁移：用主仓 `migrations/0016_feedback_attribution.{up,down}.sql`（已按 ② 写好），只在 `{ns}_` 隔离库执行；不对共享 `agri_steward` 跑。`retention` 若需新表保留天数照 L5 的 `table_days` 加。
9. 不做：Gate 2 收口（验收报告、baseline_p2、Phase 3 启动包由调度方做）、amis 页面、评测库连接。

---

## 两层共同的完成标准
- `go build ./... && go vet ./... && go vet -tags integration ./... && go test ./...`；改到的包 `-race`。
- 集成：`AS_TEST_NS=<你的 ns> go test -tags integration -race -p 1 -count=1 ./internal/repository/ ./internal/service/` 及你新加的集成测试；L7-b 另跑 `./cmd/eval/`（`-timeout 30m`，可加 `AS_MYSQL_EVAL_DATABASE=<ns>_agri_eval_it`）。
- 评测（隔离评测库）：L7-b 跑 regression n=1 证明开关 / 新子命令不改变既有样本结果（samples_hash 不变；若加了 user_sim 样本，说明 capability 的 hash 变化）；L8 跑 regression n=1 证明行为不变。
- 每个功能点小步提交，前缀 `P2-L7b：` / `P2-L8：`，中文提交信息；只 add 自己的文件；不提交 `.gitignore`、`migrations/`、`eval/out/`、`web/amis/`、`deploy/.env`。dev-log 写自己那节（实现、证据实验号、待办标注）。
- 回报 ≤ 800 字：最终 tip、提交列表、测试与评测结果（命令 + 结论 + 实验号）、偏离简报之处、待办。
