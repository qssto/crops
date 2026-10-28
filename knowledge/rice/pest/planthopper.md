---
kb_id: rice/pest/planthopper
title: 稻飞虱（褐飞虱、白背飞虱）
crop: rice
stage: [分蘖期, 拔节孕穗期, 抽穗扬花期, 灌浆结实期]
region: jiangsu
category: [protect]
topics: [protect.identify, protect.timing, protect.product, protect.dosage]
target: planthopper
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: tillering-booting, label: 分蘖至孕穗期防治指标, metric: density_per_100_hills, op: ">=", value: 1000, unit: 头/百丛, stage: [分蘖期, 拔节孕穗期], action: 达到即建议施药}
  - {id: heading-filling, label: 穗期防治指标, metric: density_per_100_hills, op: ">=", value: 1500, unit: 头/百丛, stage: [抽穗扬花期, 灌浆结实期], action: 达到即建议施药}
  - {id: survey-staleness, label: 虫情数据时效, metric: survey_age_days, op: "<=", value: 3, unit: 天, action: 调查数据超过 3 天须重新调查后再定}
basis:
  - {source: 稻飞虱测报调查与防治指标（国家/江苏植保口径，具体标准号待专家核对）, level: 专家经验, verified: false}
---
## 识别特征

- 褐飞虱：成虫褐色，短翅型多见，群集稻丛基部；为害重时稻丛基部变黑、成片「冒穿」倒伏。
- 白背飞虱：前胸背板中央有黄白色条纹，前期迁入为主。

## 适用生育期

分蘖期至灌浆结实期；江苏主害代在 8 月中下旬至 9 月（穗期）。

## 指标与阈值

- 分蘖至孕穗期：百丛虫量 ≥ 1000 头。
- 抽穗至灌浆期：百丛虫量 ≥ 1500 头。
- 虫情调查数据超过 3 天须重新调查。

## 操作要点

- 药剂只在登记候选集合内选（`pesticide_registry.yaml` target=planthopper：吡蚜酮、三氟苯嘧啶、呋虫胺），同季轮换不同作用机理。
- 对准稻株中下部喷雾，田间保持浅水层；作业窗口按 `common/operation_window/spray`。
- 吡虫啉对褐飞虱抗性高，不作推荐。

## 数值依据

阈值见 front-matter `thresholds`；亩用量、PHI、每季次数以登记表为准，换算按 `common/operation_window/dosage-dilution` 与 `common/safety/phi`。
