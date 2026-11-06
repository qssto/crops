# agri-steward 功能执行流程

本目录按代码的真实调用顺序描述每个功能做了什么，读者不看代码也能理解流程。内容以代码为准；代码与注释或路线文档不一致的地方，在对应位置以「注：」标出。

## 系统总览

系统由两个常驻进程和若干命令行工具组成，共用同一套配置、密钥和 `app.Build` 装配：

- `cmd/server`：HTTP 服务。处理问答、审批、工单操作、审计导出、看板查询、反馈与专家评审接口，推送运行事件（SSE），订阅功能开关。
- `cmd/worker`：后台任务。轮询 outbox 下发工单，运行审批过期与卡单扫描、分区维护、工单运维、质量日报、归因周报、线上回流等循环。
- 命令行工具：`cmd/eval`（离线评测与质量门禁）、`cmd/auditctl`（审计校验与灰度预检）、`cmd/reconcile`（人工对账）、`cmd/devicesim`（本地设备模拟平台）、`cmd/migrate`、`cmd/devtoken`。

一次典型业务的走向：

1. 用户在 `/ask` 提问，接口立即返回 run_id，主链路在后台运行。
2. 主链路生成建议卡；需要作业时写一张 DRAFT 作业单，开出审批票，run 停在 INTERRUPTED(WAIT_APPROVAL)。
3. 审批人在 `/approvals/:ticket_id/decide` 选择 APPROVE 或 REJECT。通过时在同一个事务里写入下发 outbox(order.dispatch)，提交后再异步让 run 从检查点续跑到结束。
4. worker 领取 outbox，做准入、开关检查和下发前复检，再走 DRY_RUN 模拟或 REAL 设备通道。
5. 设备回执经 webhook 进来，推进作业单状态；超时、中止和人工对账负责收尾。
6. 用户对建议给出采纳或差评；worker 汇总质量指标、出归因周报，并把线上样本回流到评测。

HTTP 路由大多挂在 `/api/v1/farms/:farm_id/` 下并经用户鉴权。例外有三类：`/api/v1/admin/...` 走平台级鉴权；设备回执 webhook 只校验 HMAC 签名；`/dashboard`、`/steward` 网页外壳不鉴权。详见 01 章「路由挂载总表」。

## 章节

| 章节 | 内容 |
|---|---|
| [01-启动与请求入口](01-启动与请求入口.md) | 进程启动与退出、app.Build 装配、HTTP 鉴权与权限矩阵、功能开关与一键关停、指标端口、网页外壳、migrate / devtoken |
| [02-问答主链路](02-问答主链路.md) | `/ask` 受理与异步执行、agent graph 各节点、知识加载、计算器、约束与护栏、LLM 与工具、checkpoint 与长对话压缩、运行事件 SSE、运行详情 |
| [03-审批与工单下发](03-审批与工单下发.md) | 审批列表 / 详情 / 决策与续跑、扫描任务、outbox、工单下发与设备平台、回执、工单运维、中止与对账、驳回导出 |
| [04-审计与观测](04-审计与观测.md) | 审计哈希链写入、锚定、校验、保留与分区、审计导出、auditctl、告警、指标与积压 |
| [05-线上质量闭环](05-线上质量闭环.md) | 可信度日报、专家评审、用户反馈、归因周报与问题处置、线上回流、影子数据与知识加载记录清理 |
| [06-评测命令](06-评测命令.md) | `cmd/eval` 各子命令、evalkit 执行顺序、评测资产目录、Makefile 与 CI |

## 读前须知

以下是代码当前的实际行为，和注释或路线文档的说法不同，读各章时容易误解：

- 通知 `notify.send` 只写审计和日志，不会送达用户（03 章）。
- 地块作业记录回写 `field.history.append`：real 模式必然失败进 DEAD，mock 模式只存在进程内存（03 章）。
- 主链路是线性图，没有条件边，拒答靠后续节点开头提前返回（02 章）。
- 模型调用没有装配限流器，`configs/model.yaml` 的 rate_per_min 不生效（02 章）。
- 审计旧分区没有自动删除（04 章）。
- 线上回流只把草稿写到本地目录，review_task 由 `eval backflow import` 创建（05 章）。
- `eval/baselines/release.yaml` 不存在，不带 `--baseline` 的 `eval gate` 会失败；`eval/usersim/recordings` 不存在，模拟用户 trial 全部记 INFRA_ERROR（06 章）。
