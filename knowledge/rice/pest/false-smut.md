---
kb_id: rice/pest/false-smut
title: 稻曲病
crop: rice
stage: [拔节孕穗期]
region: jiangsu
category: [protect]
topics: [protect.identify, protect.timing, protect.product, protect.dosage]
target: false_smut
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: prevent-window, label: 预防适期（破口前天数）, metric: days_before_heading, op: "<=", value: 10, unit: 天, action: 破口前 7–10 天（叶枕平至叶枕距 5 cm）施药}
  - {id: too-late, label: 预防过晚, metric: days_before_heading, op: "<", value: 3, unit: 天, action: 已破口或破口前不足 3 天，预防效果差，只做观察}
params:
  - {id: prevent_days_before_heading, label: 施药距破口天数, min: 7, max: 10, unit: 天}
basis:
  - {source: 稻曲病预防适期（具体文献待专家核对）, level: 专家经验, verified: false}
---
## 识别特征

- 抽穗后部分谷粒变为墨绿色或橙黄色的稻曲球，表面粉状；发病后无法治疗，只能预防。

## 适用生育期

拔节孕穗期（破口前 7–10 天）。

## 指标与阈值

- 破口前 7–10 天施药预防；孕穗期遇连阴雨、偏施氮肥田块必须预防。
- 已破口再施药效果差，不作推荐。

## 操作要点

- 登记候选：井冈霉素、肟菌·戊唑醇（target=false_smut）；可与纹枯病兼治。
- 由物候预计破口日倒推施药日：施药日 = 预计破口日 − 7~10 天。

## 数值依据

见 front-matter；用量与 PHI 以登记表为准。
