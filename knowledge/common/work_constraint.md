---
kb_id: common/work_constraint/interval
title: 安全间隔期余量与同类作业重复间隔
crop: "*"
stage: ["*"]
region: "*"
category: [protect, fertilize, irrigate]
topics: [protect.timing, fertilize.timing, irrigate.timing]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-29
thresholds:
  - {id: phi-margin, label: 安全间隔期余量, metric: phi_margin_days, op: ">=", value: 0, unit: 天, action: 预计收获日早于最早可收获日（含历史施药）即不得开票}
  - {id: repeat-spray, label: 同地块施药最短间隔, metric: days_since_last_spray, op: ">=", value: 3, unit: 天, action: 3 天内已施药或有在途施药单即不得开票}
  - {id: repeat-fertilize, label: 同地块追肥最短间隔, metric: days_since_last_fertilize, op: ">=", value: 7, unit: 天, action: 7 天内已追肥或有在途追肥单即不得开票}
  - {id: repeat-irrigate, label: 同地块灌溉最短间隔, metric: days_since_last_irrigate, op: ">=", value: 1, unit: 天, action: 当日已灌溉或有在途灌溉单即不得开票}
basis:
  - {source: 农药合理使用准则安全间隔期口径与常规农事间隔（待农艺专家签字）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期，施药、追肥、灌溉作业单开票前。

## 指标与阈值

- 安全间隔期：最早可收获日 = max(今日 + 本药 PHI, 同地块历史 / 在途施药日 + 该药 PHI)；预计收获日 − 最早可收获日 ≥ 0 天，差 1 天即不通过。
- 同类作业重复：施药间隔 ≥ 3 天，追肥 ≥ 7 天，灌溉 ≥ 1 天；本地有同类在途单（已审批 / 已下发 / 执行中）直接不通过。

## 操作要点

- 农事记录（field.history）读取失败或本地作业单读取失败时不通过（失败关闭）。
- 阈值为 Claude 草拟，待农艺专家签字。

## 数值依据

见 front-matter `thresholds`。
---
kb_id: common/work_constraint/airspace
title: 无人机禁飞区与敏感区缓冲
crop: "*"
stage: ["*"]
region: "*"
category: [protect]
topics: [protect.timing]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-29
thresholds:
  - {id: no-fly-buffer, label: 禁飞区距离下限, metric: no_fly_distance_m, op: ">", value: 0, unit: m, action: 地块与禁飞区相接或重叠（含边界）即不得飞防}
  - {id: sensitive-buffer, label: 敏感区距离下限, metric: sensitive_distance_m, op: ">", value: 100, unit: m, action: 距学校 / 水源 / 养殖区等敏感区 ≤ 100 m 不得飞防}
basis:
  - {source: 民用无人驾驶航空器飞行管理口径（具体条款待法务填写）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期，无人机作业单开票前与下发前。

## 指标与阈值

- 地块边界到禁飞区边界的最近距离须 > 0 m；恰在边界上按「在区内」处理。
- 到敏感区的最近距离须 > 100 m。

## 操作要点

- 地块档案未给出 no_fly_zones 时不通过；给出空列表表示已核查、周边无禁飞区。
- 阈值为 Claude 草拟，待农艺专家签字；法规依据待法务填写。

## 数值依据

见 front-matter `thresholds`。
---
kb_id: common/work_constraint/drift
title: 喷雾漂移相邻敏感作物缓冲
crop: "*"
stage: ["*"]
region: "*"
category: [protect]
topics: [protect.timing]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-29
thresholds:
  - {id: drift-buffer-uav, label: 无人机下风向敏感地块距离下限, metric: drift_distance_m, op: ">", value: 50, unit: m, action: 下风向 50 m 内有敏感作物不得飞防}
  - {id: drift-buffer-knapsack, label: 地面喷雾下风向敏感地块距离下限, metric: drift_distance_m, op: ">", value: 20, unit: m, action: 下风向 20 m 内有敏感作物不得喷雾}
  - {id: drift-sector-half-angle, label: 下风向扇区半角, metric: bearing_offset_deg, op: "<=", value: 45, unit: °, action: 相邻地块方位与下风向夹角 ≤ 45° 视为下风向}
basis:
  - {source: 喷雾漂移缓冲区常规口径（待农艺专家签字）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期，喷雾类作业开票前。

## 指标与阈值

- 下风向 = 风的来向 + 180°；相邻敏感地块方位与下风向夹角 ≤ 45°（含边界）即在下风向。
- 下风向敏感地块距离：无人机须 > 50 m，地面喷雾须 > 20 m。

## 操作要点

- 有敏感相邻地块而天气预报缺风向时不通过。
- 阈值为 Claude 草拟，待农艺专家签字。

## 数值依据

见 front-matter `thresholds`。
---
kb_id: common/work_constraint/uav-device
title: 植保无人机下发前设备状态
crop: "*"
stage: ["*"]
region: "*"
category: [protect]
topics: [protect.dosage]
authority_level: 专家经验
version: v2
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-29
thresholds:
  - {id: device-staleness, label: 设备状态时效, metric: device_age_sec, op: "<=", value: 300, unit: s, action: 设备 last_seen_at 超过 5 分钟须刷新后再下发}
  - {id: battery-margin, label: 电量余量下限, metric: battery_margin_pct, op: ">=", value: 0, unit: "%", action: 电量低于一架次所需电量即不得下发}
  - {id: payload-margin, label: 药箱余量下限, metric: tank_margin_l, op: ">=", value: 0, unit: L, action: 药箱药液低于一架次药液量即不得下发}
params:
  - {id: uav_tank_capacity_l, label: 无人机药箱容量, value: 30, unit: L}
  - {id: uav_full_charge_mu, label: 满电单架次作业亩数, value: 25, unit: 亩}
basis:
  - {source: 常见 30 L 级植保无人机规格（待农艺专家签字）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期，无人机作业单下发前复检。

## 指标与阈值

- 一架次作业亩数 = min(地块面积, 药箱容量 ÷ 亩施药液量)。
- 电量 ≥ 一架次作业亩数 ÷ 满电作业亩数 × 100% + 电量余量（battery-margin）；药箱 ≥ 一架次作业亩数 × 亩施药液量 + 药箱余量（payload-margin），比较符取自阈值条目。
- 设备在线、非故障，状态数据 5 分钟内。

## 操作要点

- 设备规格为 Claude 草拟，待农艺专家签字。

## 数值依据

见 front-matter `thresholds` 与 `params`。
