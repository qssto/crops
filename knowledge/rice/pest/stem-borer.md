---
kb_id: rice/pest/stem-borer
title: 二化螟
crop: rice
stage: [分蘖期, 拔节孕穗期, 抽穗扬花期]
region: jiangsu
category: [protect]
topics: [protect.identify, protect.timing, protect.product, protect.dosage]
target: stem_borer
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: dead-sheath, label: 分蘖期枯鞘率指标, metric: dead_sheath_rate, op: ">=", value: 0.03, unit: 比例, stage: [分蘖期, 拔节孕穗期], action: 枯鞘率达 3% 即施药}
  - {id: egg-hatch-heading, label: 防白穗时期指标, metric: egg_mass_per_mu, op: ">=", value: 100, unit: 块/亩, stage: [拔节孕穗期, 抽穗扬花期], action: 卵孵高峰期且破口前后施药防白穗}
basis:
  - {source: 水稻二化螟测报调查与防治指标（具体标准号待专家核对）, level: 专家经验, verified: false}
---
## 识别特征

- 幼虫钻蛀稻茎：分蘖期造成枯鞘、枯心，孕穗至抽穗期造成白穗和虫伤株；茎秆内可见淡褐色幼虫，背部有 5 条褐色纵线。

## 适用生育期

分蘖期至抽穗扬花期；江苏一代为害分蘖期，二代为害穗期。

## 指标与阈值

- 分蘖期：枯鞘率 ≥ 3%。
- 穗期：每亩卵块 ≥ 100 块，在卵孵高峰期、破口期前后施药防白穗。

## 操作要点

- 施药时田间保水 3–5 cm 并保持 3–5 天（`rice/irrigation/spray-water`）。
- 登记候选：氯虫苯甲酰胺、甲氨基阿维菌素苯甲酸盐、甲氧虫酰肼（target=stem_borer）。
- 注意甲氧虫酰肼 PHI 长（60 天），临近收获的田块不选。

## 数值依据

阈值见 front-matter；用量与 PHI 以登记表为准。
