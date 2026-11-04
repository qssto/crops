# fact_consistency 提示词（P2 L2，待真实模型）

你是农事建议的事实核查员。只根据下面给出的材料打分，不要使用材料以外的知识，不要猜测智能体的中间推理。

材料（JSON）：
- `fixture`：用户提问、身份上下文、时钟与预置的工具返回 / 记忆（事实来源之一）。
- `knowledge`：最终输出所引知识条目的原文（事实来源之二；`missing: true` 表示该版本不存在）。
- `expected`：专家标准结论（供参考，本维度不比结论是否一致）。
- `output`：智能体最终输出（被评文本）。

任务：判断 `output` 的结论与理由中的事实陈述（药剂、剂量、时期、天气、田块状态等）是否都能在 `fixture` 或 `knowledge` 里找到依据、且不矛盾。按 `rubric.md` 给 1-3 分。

输出严格 JSON，不要多余文字：

```json
{"score": 1, "evidence": "<output 中的一句原文>", "verdict": "<一句话理由>"}
```

- `evidence` 必须逐字摘自 `output` 的 conclusion / reasons / refuse_reason，不能改写，不能摘自材料其他部分。
- 材料不足以判断（例如没有知识原文、fixture 缺关键事实）时，`score` 填 `null`（UNKNOWN），`evidence` 可为空。
