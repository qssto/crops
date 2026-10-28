---
kb_id: rice/pest/blast
title: 稻瘟病（叶瘟、穗颈瘟）
crop: rice
stage: [分蘖期, 拔节孕穗期, 抽穗扬花期]
region: jiangsu
category: [protect]
topics: [protect.identify, protect.timing, protect.product, protect.dosage]
target: blast
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: leaf-blast-center, label: 叶瘟发病中心, metric: blast_centers, op: ">=", value: 1, unit: 个, stage: [分蘖期, 拔节孕穗期], action: 出现发病中心或急性型病斑即施药}
  - {id: neck-blast-prevent, label: 穗瘟预防时期, metric: panicle_emerged_rate, op: ">=", value: 0.05, unit: 比例, stage: [抽穗扬花期], action: 破口期施第一次，齐穗期施第二次}
  - {id: rainy-days, label: 连阴雨风险, metric: rain_days_7d, op: ">=", value: 4, unit: 天, action: 7 天内降雨 ≥ 4 天且感病品种，提高预防优先级}
basis:
  - {source: 稻瘟病测报调查规范与防治适期（具体标准号待专家核对）, level: 专家经验, verified: false}
---
## 识别特征

- 叶瘟：典型慢性型病斑梭形，边缘褐色、中央灰白色，两端有褐色坏死线；急性型为暗绿色水渍状斑，背面有灰绿色霉层。
- 穗颈瘟：穗颈节变褐坏死，造成白穗或半白穗。

## 适用生育期

分蘖期至抽穗扬花期；破口至齐穗是穗瘟预防关键期。

## 指标与阈值

- 叶瘟：田间出现发病中心或急性型病斑即施药。
- 穗瘟：感病品种或连阴雨（7 天内雨日 ≥ 4 天）时，破口期和齐穗期各预防一次。

## 操作要点

- 症状描述只能支撑「疑似」，需田间复核后再给作业结论。
- 登记候选：三环唑、稻瘟灵、春雷霉素（target=blast）。三环唑以预防为主，宜在破口前施用。

## 数值依据

阈值见 front-matter；用量与 PHI 以登记表为准。
