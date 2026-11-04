# P2 第 1 批续：L1 样本撰写（四个子 agent 按一级分类并行，并入 P2-L1 提交）

先读：`docs/briefs/P2-common.md`、`eval/README.md`（含 L1-T 新增的「分层维度」「陷阱组」「覆盖报表」段）、`eval/taxonomy.yaml`、`eval/coverage/targets.yaml`、`docs/p2/L1-覆盖报表-初版.md`（第 3 节补写清单汇总、第 4 节逐格明细）、`knowledge/`（全部条目与 `index.yaml`）、`knowledge/common/pesticide_registry.yaml`。挑 10 条以上现有样本当模板，覆盖 ADVICE、REFUSED、degraded、work_order 四种期望（grep `outcome:`、`work_order:`），弄清 must_cite / numbers / product_in / must_call 的写法与参考解规则（`internal/evalkit/grader/code/reference.go`、`internal/evalkit/sample/rules*.go`）。
执行手册：P2 §6.2 L1（183-230 行），尤其功能任务 3、4、6、7 与技术实现 5（holdout）。
基线分支：`p2-l1s-base`（= `p2-l1t` 820d78e + 本简报）。开工先 `git merge --ff-only p2-l1s-base`。只新增样本 YAML，不改代码、taxonomy、targets、知识库与现有样本；发现工具或知识库缺陷就记在回报里，由调度方决定。

## 分工（按一级分类）
| agent | 分类 | 补写清单缺口 | ID 前缀（公开 / holdout） | 公开目录 | 额外承担 |
|-------|------|--------------|---------------------------|----------|----------|
| S-prot | protect | 129 | `rice-p2-prot-` / `rice-ho-prot-` | `eval/suites/capability/protect/` | `trap.similar_pest` 15 条；`trap.overdose` 6 条；`trap.cross_crop` 4；`trap.stale_kb` 4；KB_MISS 6、DATA_MISSING 3、OUT_OF_SCOPE 3 |
| S-fert | fertilize | 95 | `rice-p2-fert-` / `rice-ho-fert-` | `eval/suites/capability/fertilize/` | `trap.overdose` 5 条；`trap.cross_crop` 4；`trap.stale_kb` 4；KB_MISS 5、DATA_MISSING 3、OUT_OF_SCOPE 3 |
| S-irri | irrigate | 92 | `rice-p2-irri-` / `rice-ho-irri-` | `eval/suites/capability/irrigate/` | `trap.cross_crop` 4；`trap.stale_kb` 4；KB_MISS 4、DATA_MISSING 2、OUT_OF_SCOPE 3 |
| S-disa | disaster | 79 | `rice-p2-disa-` / `rice-ho-disa-` | `eval/suites/capability/disaster/` | `trap.cross_crop` 3；`trap.stale_kb` 3；KB_MISS 4、DATA_MISSING 2、OUT_OF_SCOPE 3 |

陷阱和拒答样本都落在本分类的缺口格子里（不额外加条数）。编号从 0001 起连续。

## 数量与 holdout
1. 写满本分类补写清单的全部缺口（逐格 `stage × topic × difficulty` 对齐第 4 节明细）。其中约 20% 按 `topic × difficulty` 分层留作 holdout（每个 topic 的 easy / medium / hard 各自约 1/5，四舍五入，至少覆盖每个 topic），其余进公开 capability。
2. holdout 样本**绝不写进仓库或 worktree**：只写到 `/Users/v_qinshisen/Desktop/crops-holdout/<分类>/`（独立私有 git 仓，不要在那里 commit，调度方统一提交），`suite: holdout`。公开样本 `suite: capability`。
3. 所有样本 `version: 1`、`author: claude-draft`、`review_status: pending_expert`、`signed_at: 2026-11-03`、`knowledge_ref: WORKTREE`，四个分层字段齐全（`crop / stage / tags 里首个子类 / difficulty`）。

## 质量要求（这是评测集，不是凑数）
1. 一格之内的样本措辞、情境要有实质差异：不同地块、生育期细分、天气、历史作业、用户角色、口语程度、追问方式；禁止同一模板换词。
2. 期望结论必须以知识库为依据：ADVICE 样本的 `must_cite` 指向真实存在的 `kb_id@version`、药剂在 `pesticide_registry` 候选集合内、数值由 calc 能算出（与现有样本的 numbers 写法一致）；知识库没有依据的问题只能写成 KB_MISS 拒答，不能编结论。
3. 比例：每个分类 ADVICE（含 degraded）约 70%，拒答约 30%；拒答样本 `refuse_reason` 与来源一致（知识未覆盖 KB_MISS / 数据缺失 DATA_MISSING，用 fixtures 让工具返回空或 error / 超出作物范围 OUT_OF_SCOPE）。
4. 陷阱样本（`tags` 加对应 `trap.*` code）在 `notes` 写清陷阱点与期望的拒答或纠正结论：
   - `trap.similar_pest`：症状描述贴近 A 实为 B（如稻瘟病 vs 胡麻斑病、纹枯病 vs 菌核病、二化螟 vs 大螟），期望按 fixtures 中的识别结果给 B 的方案或因证据不足拒答；
   - `trap.overdose`：用户要求加倍、超上限、缩短间隔、超次数，期望只给 calc 值或拒答，并用 `expect.must_not_contain` 列出不得出现的超量表述；
   - `trap.cross_crop`：把小麦 / 玉米 / 蔬菜上的做法或药剂套到水稻，或反之，期望拒答 OUT_OF_SCOPE 或纠正为水稻方案；
   - `trap.stale_kb`：引用过时标准、已禁限用药剂（查 registry 与 safety.md 的禁限用信息）、旧版用量，期望纠正或拒答；若知识库缺版本 / 失效字段表达不了，写能表达的部分并在回报里说明。
5. fixtures 无真实身份与精确坐标（`pii_clean` 扫描会失败）：只用 `farm-demo-*` / `field-*` / `u-*` 一类合成标识，不写手机号、身份证、人名、经纬度。
6. 每条都要能被 `cmd/eval validate` 通过（含参考解过全部代码打分器）。

## 验证
```bash
export GOROOT=/usr/local/go PATH=/usr/local/go/bin:$PATH
go run ./cmd/eval validate --color never                                   # 公开样本（全量，含你的）
go run ./cmd/eval validate --color never --suites-dir /Users/v_qinshisen/Desktop/crops-holdout/<分类>   # 你的 holdout 样本
```
两者都须 `validate OK`。不跑 make eval / 集成测试 / 任何连库命令。

## 提交与回报
- 在 worktree 分支上中文 commit（`P2-L1：<分类> 样本 N 条（capability）`，结尾 `Co-Authored-By: Claude Code <noreply@anthropic.com>`），只含 `eval/suites/capability/<分类>/`；不动 main、不 push。
- 回报（≤ 40 行）：分支与 SHA；公开 / holdout 条数；按 topic × difficulty 的条数表；陷阱与拒答来源条数；两条 validate 的末行输出；发现的知识库 / 工具缺陷；表达不了的期望。
