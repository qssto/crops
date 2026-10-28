---
kb_id: common/operation_window/spray
title: 喷雾作业窗口硬约束
crop: "*"
stage: ["*"]
region: "*"
category: [protect]
topics: [protect.timing]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: wind-knapsack, label: 地面喷雾风速上限, metric: wind_speed_ms, op: "<=", value: 5.4, unit: m/s, action: 超过 3 级风停止地面喷雾}
  - {id: wind-uav, label: 无人机飞防风速上限, metric: wind_speed_ms, op: "<=", value: 3.0, unit: m/s, action: 超过即停止飞防}
  - {id: rain-free, label: 施药后无雨时长, metric: rain_free_hours, op: ">=", value: 6, unit: h, action: 施药后 6 h 内预报有雨则推迟}
  - {id: rain-24h, label: 24 h 降雨上限, metric: rain_mm_24h, op: "<", value: 5, unit: mm, action: 未来 24 h 降雨 ≥ 5 mm 推迟施药}
  - {id: temp-max, label: 施药气温上限, metric: temp_c, op: "<=", value: 35, unit: ℃, action: 气温 > 35 ℃ 停止施药；> 30 ℃ 只在 10 时前或 16 时后作业}
  - {id: weather-staleness, label: 天气数据时效, metric: weather_age_hours, op: "<=", value: 6, unit: h, action: 预报数据超过 6 h 须刷新后再定}
basis:
  - {source: 农药喷雾与植保无人机作业安全要求（行业标准口径，具体标准号待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期，所有喷雾类作业。

## 指标与阈值

- 风速：地面喷雾 ≤ 5.4 m/s（3 级），无人机 ≤ 3.0 m/s。
- 降雨：施药后 6 h 内无雨，且未来 24 h 降雨 < 5 mm。
- 气温：≤ 35 ℃；> 30 ℃ 时只在 10 时前或 16 时后作业。
- 天气数据须是 6 h 内的预报。

## 操作要点

- 任一硬约束不满足：作业类建议改为「推迟」并说明原因，不生成作业单。
- 天气工具超时或无数据时不得给作业窗口结论（`DATA_MISSING`）。

## 数值依据

见 front-matter `thresholds`。
---
kb_id: common/operation_window/uav
title: 植保无人机飞防作业参数
crop: "*"
stage: ["*"]
region: "*"
category: [protect]
topics: [protect.dosage, protect.timing]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: uav_spray_volume, label: 无人机亩施药液量, value: 1.5, min: 1.0, max: 2.0, unit: L/亩}
  - {id: uav_height_m, label: 作业高度（距冠层）, min: 1.5, max: 2.5, unit: m}
  - {id: uav_speed_ms, label: 飞行速度, min: 3, max: 6, unit: m/s}
basis:
  - {source: 植保无人机施药常规参数（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期。

## 指标与阈值

- 亩施药液量默认 1.5 L（不少于 1.0 L，不超过 2.0 L）；作业高度距冠层 1.5–2.5 m，速度 3–6 m/s。

## 操作要点

- 亩用药量仍按登记用量，只是兑水量按无人机施药液量；稀释倍数 = 施药液量 × 1000 ÷ 亩用药量。
- 风速 > 3.0 m/s 停飞（`common/operation_window/spray`）。
- P0 只生成 dry-run 作业单草稿并转审批，系统不直接调度无人机。

## 数值依据

见 front-matter `params`。
---
kb_id: common/operation_window/dosage-dilution
title: 亩用量、总用量与稀释倍数换算
crop: "*"
stage: ["*"]
region: "*"
category: [protect]
topics: [protect.dosage]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: knapsack_spray_volume, label: 背负式电动喷雾亩施药液量, value: 30, min: 30, max: 45, unit: L/亩}
formulas:
  - id: total_dose
    calc: Dosage
    expr: total = dose_per_mu * area_mu
    inputs: [dose_per_mu, area_mu]
    output: {name: total, unit: 与亩用量同（g 或 mL）}
  - id: dilution_ratio
    calc: Dilution
    expr: dilution_ratio = spray_volume_l_per_mu * 1000 / dose_per_mu
    inputs: [spray_volume_l_per_mu, dose_per_mu]
    output: {name: dilution_ratio, unit: 倍}
  - id: dose_from_dilution
    calc: Dilution
    expr: dose_per_mu = spray_volume_l_per_mu * 1000 / dilution_ratio
    inputs: [spray_volume_l_per_mu, dilution_ratio]
    output: {name: dose_per_mu, unit: mL/亩 或 g/亩}
basis:
  - {source: 体积换算（制剂密度按 1 g/mL 近似，待专家确认是否需要按剂型修正）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期。

## 指标与阈值

- 亩用量只能取登记表 `dose.min`–`dose.max` 之间；超出登记用量一律拒绝。

## 操作要点

- 总用量 = 亩用量 × 地块面积；地块面积来自 `field.profile`。
- 稀释倍数 = 亩施药液量(L) × 1000 ÷ 亩用药量(mL 或 g)。例：无人机 1.5 L/亩、10 mL/亩 → 150 倍。
- 用量区间未指定取值时，同时给出下限与上限，不由模型自行选值。

## 数值依据

公式见 front-matter `formulas`。
---
kb_id: common/operation_window/fertilize
title: 施肥作业窗口
crop: "*"
stage: ["*"]
region: "*"
category: [fertilize]
topics: [fertilize.timing]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: rain-24h, label: 追肥前降雨上限, metric: rain_mm_24h, op: "<", value: 20, unit: mm, action: 未来 24 h 降雨 ≥ 20 mm 推迟追肥，防肥料流失}
  - {id: water-depth, label: 追肥时水层上限, metric: water_depth_cm, op: "<=", value: 3, unit: cm, action: 水层超过 3 cm 先排浅再施}
basis:
  - {source: 追肥作业常规要求（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期追肥作业。

## 指标与阈值

- 未来 24 h 降雨 < 20 mm；田间水层 ≤ 3 cm。

## 操作要点

- 施后 3–5 天不排水、不串灌。
- 两次追肥间隔不少于 7 天。

## 数值依据

见 front-matter。
