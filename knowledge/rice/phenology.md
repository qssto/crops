---
kb_id: rice/phenology/stages
title: 水稻生育期划分与物候判定
crop: rice
stage: ["*"]
region: jiangsu
category: [fertilize, protect, irrigate]
topics: [fertilize.timing, protect.timing, irrigate.timing]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: tillering-start, label: 进入分蘖期, metric: days_after_transplant, op: ">=", value: 7, unit: 天, action: 返青结束，转入分蘖期管理}
  - {id: jointing-start, label: 进入拔节孕穗期, metric: leaf_age_remaining, op: "<=", value: 4, unit: 叶, action: 倒 4 叶露尖视为幼穗分化开始}
  - {id: heading-start, label: 进入抽穗扬花期, metric: panicle_emerged_rate, op: ">=", value: 0.05, unit: 比例, action: 5% 稻穗破口视为破口期}
  - {id: full-heading, label: 齐穗, metric: panicle_emerged_rate, op: ">=", value: 0.8, unit: 比例, action: 80% 稻穗抽出视为齐穗}
  - {id: maturity, label: 进入成熟期, metric: yellow_grain_rate, op: ">=", value: 0.85, unit: 比例, action: 85% 以上谷粒黄熟可安排收获}
params:
  - {id: heading_to_maturity_days, label: 抽穗至成熟天数, min: 45, max: 55, unit: 天}
basis:
  - {source: 水稻栽培学通用生育期划分（教材口径，待专家指定标准）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期。本条目定义 `stage` 枚举的判定口径，供 mcp-crop 物候判定与知识加载共用：秧苗期、返青期、分蘖期、拔节孕穗期、抽穗扬花期、灌浆结实期、成熟期。

## 指标与阈值

- 返青期：移栽后约 7 天内，新根长出、叶片转绿；移栽后 ≥ 7 天进入分蘖期。
- 拔节孕穗期：剩余叶龄 ≤ 4（倒 4 叶露尖）视为幼穗分化开始，至破口前结束。
- 抽穗扬花期：5% 稻穗破口为破口期，80% 抽出为齐穗。
- 成熟期：85% 以上谷粒黄熟。

## 操作要点

- 江苏单季粳稻常规茬口：5 月中下旬播种，6 月中旬移栽，8 月下旬至 9 月初抽穗，10 月下旬至 11 月上旬收获；抽穗至成熟约 45–55 天。
- 工具返回的细分时期（分蘖初期、幼穗分化期、破口期等）写在 `sub_stage`，知识过滤只看 `stage`。
- 物候判定缺失时不得凭日期推断生育期给作业结论，应提示补充田间调查。

## 数值依据

见 front-matter `thresholds` / `params`；口径为水稻栽培学通用划分，待专家指定依据标准后上调 `authority_level`。
