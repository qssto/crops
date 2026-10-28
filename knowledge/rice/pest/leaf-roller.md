---
kb_id: rice/pest/leaf-roller
title: 稻纵卷叶螟
crop: rice
stage: [分蘖期, 拔节孕穗期, 抽穗扬花期]
region: jiangsu
category: [protect]
topics: [protect.identify, protect.timing, protect.product, protect.dosage]
target: leaf_roller
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: tillering, label: 分蘖期防治指标, metric: larvae_per_100_hills, op: ">=", value: 60, unit: 头/百丛, stage: [分蘖期], action: 低龄幼虫高峰期施药}
  - {id: booting-heading, label: 孕穗后防治指标, metric: larvae_per_100_hills, op: ">=", value: 40, unit: 头/百丛, stage: [拔节孕穗期, 抽穗扬花期], action: 低龄幼虫高峰期施药}
basis:
  - {source: 稻纵卷叶螟测报与防治指标（具体标准号待专家核对）, level: 专家经验, verified: false}
---
## 识别特征

- 幼虫吐丝纵卷叶片成虫苞，在苞内取食叶肉，留下白色条斑；成虫淡黄褐色，前翅有两条褐色横线。

## 适用生育期

分蘖期至抽穗扬花期；保护上部三片功能叶是重点。

## 指标与阈值

- 分蘖期：百丛低龄幼虫 ≥ 60 头。
- 孕穗期以后：百丛低龄幼虫 ≥ 40 头。

## 操作要点

- 在卵孵盛期至低龄幼虫（1–2 龄）高峰期施药；傍晚或阴天施药效果好。
- 登记候选：氯虫苯甲酰胺、茚虫威、甲氨基阿维菌素苯甲酸盐（target=leaf_roller）。
- 无人机作业亩施药液量按 `common/operation_window/uav`。

## 数值依据

阈值见 front-matter；用量与 PHI 以登记表为准。
