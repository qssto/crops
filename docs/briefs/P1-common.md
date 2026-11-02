# P1 子 agent 公共简报补充（在 `docs/agent-brief-common.md` 基础上，冲突以本文为准）

## 文档
- P1 执行手册（只读）：`智能农作物Agent管家-P1-生产可用期实施路线.md`。各 Ln 的「技术实现」流程 / SQL / Redis 键是定版，照抄，不擅自改名。
- P0 已完成的接口与偏差记录在 `docs/dev-log.md`（P0 各层小节），开工前读与本层相关的部分；P0 定版表结构见 P0 手册 §6 与 `migrations/`。
- 母文档 §4.6.2（312-377 行）是跨阶段公共约定：Scope、下发器 ①-⑦、outbox 去重键、开关取值、WorkOrder 迁移表等。

## 环境
- Go：本机 `/opt/homebrew/bin/go` 版本不匹配，一律用 `export GOROOT=/usr/local/go PATH=/usr/local/go/bin:$PATH`；make 加 `GO=go`。
- 依赖：docker 里 MySQL / Redis 已在运行；MinIO 是本机 brew 进程（127.0.0.1:9000），已在运行，不要重启或停止。
- **不要对共享库做迁移或写数据**：不要跑 `make migrate`、`make seed`、`make reconcile`、`make -n`（会展开 deploy/.env 密钥）。业务库 `agri_steward` 由调度方在合并时迁移。
- 集成测试与评测必须设命名空间，避免并行 worktree 互相弄脏库：`export AS_TEST_NS=<你的简称，如 l0 / l1a>`，再跑 `make test-integration GO=go` 或 `make eval GO=go N=3`（库名变成 `{ns}_agri_it_*` / `{ns}_agri_eval_t_*`）。
- 迁移文件不入 git：开工先把主仓库 `/Users/v_qinshisen/Desktop/crops/migrations/` 整个目录复制到自己 worktree 的 `migrations/`（含 `eval/` 子目录）。新迁移只用简报分配给你的编号。
- 新增第三方依赖需调度方批准（简报里写明已批准的才可以 `go get`）。

## 提交
- 在自己的 worktree 分支上可以 `git commit`（中文提交信息），**不要动 main、不要 push、不要 merge 别的分支**（简报要求的 `git merge --ff-only <基线分支>` 除外）。调度方验收后 squash 进 main。
- 不要提交 `migrations/**/*.sql`（已 gitignore）。

## 验收证据
- 回报里写清楚：`go build ./... && go vet ./... && go test ./...` 结果；集成测试（带 AS_TEST_NS）实际跑过的包与结果；本层验收条目逐条「已满足 / 部分满足 / 未满足（原因）」。
- 依赖人工 / 外部的验收项（法务签字、Owner 确认、预发演练、田间数据）不要写成已满足，统一标「待签字 / 待演练」，并给出准备好的材料位置。
- 本阶段红线代码（权限矩阵、开关读取、constraint_checker、审批闸门与路由、下发路径、pii_masking）改动要有应失败的测试用例。
