# 子 agent 公共简报（每个 Ln 通用）

## 仓库与环境
- 仓库根目录：/Users/v_qinshisen/Desktop/crops（Go module 名 `agri-steward`，Go 1.25，GOPROXY=goproxy.cn）。代码直接放根目录。
- 需求文档（只读）：
  - P0 执行手册：`智能农作物Agent管家-P0-MVP闭环可用期实施路线.md`（各 Ln 的 SQL / Redis 键 / 流程是定版）
  - 母文档：`智能农作物Agent管家-分阶段实施路线图.md` §4.6（216-377 行：工程结构、存储约定、Redis 键约定、跨阶段公共约定、WorkOrder 迁移表、表清单、评测库）、§5.0（412-675 行）
  - 评测设计：`智能农作物Agent管家-评测Harness设计方案.md`（§5 样本格式、§7 打分器、§8 多次运行、§14 代码结构）
- 本地依赖：Docker 已运行。`deploy/docker-compose.yml` 起 MySQL 8.0 / Redis 7.2 / MinIO；密码放 `deploy/.env`（不入库，另提供 `deploy/.env.example`）。
- 模型：oneapi（OpenAI 兼容），从环境变量 `ONEAPI_BASE_URL`、`ONEAPI_API_KEY`、`ONEAPI_MODEL` 读取，绝不写进文件；未设置时用桩模型测试。
- D2 占位：作物水稻、区域江苏、示范农场「示范农场1」（tenant_id=1, farm_id=1）。D6：天气只做 mock（real 实现留接口 + 配置开关，不调用外网）。

## 必须遵守
1. 写代码前先读 `~/.claude/skills/coding-fundamentals/SKILL.md` 并照做：SOLID、DRY/KISS/YAGNI、小接口、err 必查、禁止 `_` 吞错、`fmt.Errorf("...: %w", err)` 包装、命名见意图、表驱动测试（含应失败用例）、避免 N+1 / 热路径 O(n²)、魔法数提常量。
2. 代码分层（母文档 §4.6）：`cmd/{server,eval,migrate}`、`internal/{controller,service,repository,model,agent/{llm,harness,graph,tools/mcp,knowledge,calc,guardrail},adapter,app,evalkit}`、`migrations/`、`configs/ policy/ prompts/ knowledge/ eval/ deploy/`。
   - `controller` 只调 `service`；`agent` 不 import `controller/service`；`agent/harness` 不 import 业务包；生产代码不 import `internal/evalkit`；`model` 不依赖任何内部包。
   - 生产代码不直接读系统时间（用注入的 `Clock`），不直接连库（走 repository 接口）。
3. 表结构、列名、唯一键、Redis 键名照抄 P0 文档对应 Ln 的 SQL（P0 定版），不擅自改名；Redis 实际键带 `as:{env}:` 前缀，由 `repository/rediskey` 统一拼接。
4. 迁移 SQL 放 `migrations/`（被 .gitignore 忽略，但必须创建并能执行）。
5. 注释用简洁中文，密度适中；不写多余抽象（YAGNI）。
6. 不要 git commit / push（由调度方验收后提交）；不要改需求文档。
7. 完成后在 `docs/dev-log.md` 追加本层小节（≤ 30 行）：交付清单、对外接口（包路径 + 关键类型/函数签名）、与文档的偏差及理由、留给后续层的 TODO。
8. 验证：`go build ./... && go vet ./... && go test ./...` 必须全绿；涉及 DB 的测试连 docker 里的真实 MySQL/Redis/MinIO（用 build tag `integration` 或环境变量开关，`make test-integration` 可跑），并实际跑过。

## 回报格式（最终消息，≤ 60 行）
- 新增/修改的主要文件（按目录归纳，不逐文件罗列）
- 验证命令与结果摘要（失败要原样贴出）
- 本层验收条目逐条：已满足 / 部分满足 / 未满足（原因）
- 与文档的偏差、遗留问题
