---
kb_id: rice/pest/weeds
title: 移栽稻田杂草防除
crop: rice
stage: [返青期, 分蘖期]
region: jiangsu
category: [protect]
topics: [protect.identify, protect.timing, protect.product, protect.dosage]
target: weeds
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: closed-treatment, label: 封闭处理时期, metric: days_after_transplant, op: "<=", value: 7, unit: 天, action: 移栽后 5–7 天药土法封闭}
  - {id: post-emergence, label: 茎叶处理时期, metric: barnyard_grass_leaf_age, op: "<=", value: 4, unit: 叶, action: 稗草、千金子 2–4 叶期茎叶喷雾}
basis:
  - {source: 稻田杂草化学防除常规做法（待专家核对）, level: 专家经验, verified: false}
---
## 识别特征

- 稗草：叶片无叶舌、无叶耳，是与稻苗区分的关键；千金子：叶舌膜质、分枝多；阔叶杂草（鸭舌草、节节菜）与莎草（异型莎草、碎米莎草）。

## 适用生育期

返青期至分蘖期（移栽后 5–25 天）。

## 指标与阈值

- 封闭：移栽后 5–7 天，药土法撒施（苄嘧磺隆）。
- 茎叶处理：稗草、千金子 2–4 叶期（五氟磺草胺、氰氟草酯）。

## 操作要点

- 水层要求按 `rice/irrigation/spray-water`：药土法施药后保水 5–7 天；茎叶喷雾前排水、施药后 1–2 天复水。
- 氰氟草酯不与激素类阔叶除草剂混用（`common/safety/mixing`）。
- 登记候选见登记表 target=weeds；除草剂 `phi_days` 为 null，不做 PHI 推算。

## 数值依据

见 front-matter；用量以登记表为准。
