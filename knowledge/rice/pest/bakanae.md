---
kb_id: rice/pest/bakanae
title: 恶苗病（种子处理）
crop: rice
stage: [秧苗期]
region: jiangsu
category: [protect]
topics: [protect.identify, protect.product, protect.dosage]
target: bakanae
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: soak_hours, label: 浸种时长, min: 48, max: 72, unit: h}
  - {id: soak_water_seed_ratio, label: 药液与种子体积比, value: 1.25, unit: 倍}
formulas:
  - id: dose_from_dilution
    calc: Dilution
    expr: product_ml = water_l * 1000 / dilution_ratio
    inputs: [water_l, dilution_ratio]
    output: {name: product_ml, unit: mL}
basis:
  - {source: 水稻浸种防恶苗病常规做法（待专家核对）, level: 专家经验, verified: false}
---
## 识别特征

- 秧苗徒长、细高、淡黄绿色，节间伸长，后期茎节处生不定根，病株多提早枯死。

## 适用生育期

秧苗期（播种前浸种）。

## 指标与阈值

- 种子带菌为主要来源，常规品种一律药剂浸种，不设田间防治指标。

## 操作要点

- 登记候选：咪鲜胺、氰烯菌酯（target=bakanae），稀释 2000–3000 倍浸种 48–72 h，药液没过种子。
- 药量由药液量和稀释倍数换算：药量(mL) = 药液量(L) × 1000 ÷ 稀释倍数；例：50 L 药液、2000 倍 → 25 mL。

## 数值依据

公式 `dose_from_dilution`，见 front-matter；稀释倍数以登记表为准。
