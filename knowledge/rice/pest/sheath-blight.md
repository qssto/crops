---
kb_id: rice/pest/sheath-blight
title: 纹枯病
crop: rice
stage: [分蘖期, 拔节孕穗期, 抽穗扬花期]
region: jiangsu
category: [protect]
topics: [protect.identify, protect.timing, protect.product, protect.dosage]
target: sheath_blight
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: tillering-booting, label: 分蘖末期至孕穗期防治指标, metric: diseased_hill_rate, op: ">=", value: 0.2, unit: 比例, stage: [分蘖期, 拔节孕穗期], action: 病丛率达 20% 施药}
  - {id: heading, label: 抽穗期防治指标, metric: diseased_hill_rate, op: ">=", value: 0.3, unit: 比例, stage: [抽穗扬花期], action: 病丛率达 30% 施药}
basis:
  - {source: 稻纹枯病测报技术规范与防治指标（具体标准号待专家核对）, level: 专家经验, verified: false}
---
## 识别特征

- 叶鞘近水面处先出现暗绿色水渍状斑，后成椭圆形云纹状病斑，边缘褐色、中部灰白；高温高湿时病斑向上蔓延至叶片，可见白色菌丝与褐色菌核。

## 适用生育期

分蘖末期至抽穗扬花期，孕穗前后为防治关键期。

## 指标与阈值

- 分蘖末期至孕穗期：病丛率 ≥ 20%。
- 抽穗期：病丛率 ≥ 30%。

## 操作要点

- 病害向上部叶片蔓延时及时防治，重点喷稻株中下部，药液量偏大（每亩 45–60 L 常规喷雾）。
- 登记候选：井冈霉素、噻呋酰胺、己唑醇、肟菌·戊唑醇（target=sheath_blight）。
- 偏施氮肥、密植加重发病，结合 `rice/fertilization/panicle` 控氮。

## 数值依据

阈值见 front-matter；用量与 PHI 以登记表为准。
