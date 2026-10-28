---
kb_id: common/safety/phi
title: 安全间隔期（PHI）与作业日推算
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
formulas:
  - id: last_spray_date
    calc: PHIDate
    expr: last_spray_date = expected_harvest - phi_days
    inputs: [expected_harvest, phi_days]
    output: {name: last_spray_date, unit: 日期}
  - id: earliest_harvest
    calc: PHIDate
    expr: earliest_harvest = spray_date + phi_days
    inputs: [spray_date, phi_days]
    output: {name: earliest_harvest, unit: 日期}
basis:
  - {source: 农药合理使用准则 GB/T 8321 系列（安全间隔期定义；各药剂 PHI 以登记标签为准，待核对）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期，临近收获时必须校验。

## 指标与阈值

- PHI 取登记表 `phi_days`；`null`（浸种、除草剂封闭等）不做推算。
- 最晚施药日 = 预计收获日 − PHI；若最晚施药日早于今天，该药剂不可用。

## 操作要点

- 同一田块多次施药，以最后一次施药与最长 PHI 计算最早收获日。
- 预计收获日来自 `crop.phenology.expected_harvest`；缺失时提示补充，不自行估算。

## 数值依据

公式见 front-matter `formulas`。
---
kb_id: common/safety/mixing
title: 农药混用禁忌
crop: "*"
stage: ["*"]
region: "*"
category: [protect]
topics: [protect.product]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: max_products_in_tank, label: 单次桶混产品数上限, value: 3, unit: 个}
basis:
  - {source: 农药混用常规原则（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期。

## 指标与阈值

- 单次桶混不超过 3 个产品。

## 操作要点

- 不混用：氰氟草酯与激素类阔叶除草剂；碱性药剂与遇碱分解的药剂；同一作用机理的两种药剂。
- 混配顺序：水分散粒剂 / 可湿性粉剂 → 悬浮剂 → 水剂 → 乳油，逐一加入并充分搅拌，现配现用。
- 标签注明「不可混用」的一律不混；未在本条覆盖的混配组合，不给能否混用的结论。

## 数值依据

见 front-matter。
---
kb_id: common/safety/resistance
title: 抗药性治理与每季用药次数
crop: "*"
stage: ["*"]
region: "*"
category: [protect]
topics: [protect.product]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
basis:
  - {source: 抗药性治理常规原则（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期。

## 指标与阈值

- 每种药剂每季使用次数不超过登记表 `max_times_per_season`。
- 同一防治对象连续两次施药应换用不同作用机理的药剂。

## 操作要点

- 根据 `field.history` 统计本季已用次数；已达上限的药剂从候选中剔除。
- 稻飞虱：吡蚜酮、三氟苯嘧啶、呋虫胺轮换；吡虫啉对褐飞虱抗性高，不推荐。
- 鳞翅目（稻纵卷叶螟、二化螟）：双酰胺类（氯虫苯甲酰胺）每季不超过 2 次，与甲维盐、茚虫威、甲氧虫酰肼轮换。

## 数值依据

次数上限以登记表为准。
---
kb_id: common/safety/ppe
title: 施药人员防护
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
  - {id: max_work_hours_per_day, label: 人工施药每日作业时长上限, value: 6, unit: h}
basis:
  - {source: 农药安全使用常规要求（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期。

## 指标与阈值

- 人工施药每日不超过 6 h。

## 操作要点

- 穿长衣长裤、戴手套口罩，顺风隔行施药，作业中不进食、不吸烟。
- 施药后用肥皂清洗，药械不在河塘清洗；空包装集中回收。
- 出现头晕、恶心等中毒症状立即停止作业并就医，携带农药标签。

## 数值依据

见 front-matter。
---
kb_id: common/safety/registration
title: 只用登记药剂、不超登记用量
crop: "*"
stage: ["*"]
region: "*"
category: [protect]
topics: [protect.product, protect.dosage]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
basis:
  - {source: 《农药管理条例》按登记的使用范围和用量使用农药的要求, level: 专家经验, verified: false}
---
## 适用生育期

全生育期。

## 指标与阈值

- 药剂必须在 `pesticide_registry.yaml` 中存在「作物 × 防治对象 × 区域」匹配的登记记录。
- 亩用量不得超过登记 `dose.max`。

## 操作要点

- 用户点名的药剂若不在候选集合内：说明未登记于水稻该防治对象，不给用量，改从候选集合推荐。
- 用户要求加大剂量、超次数使用时一律拒绝，并说明登记上限。
- 登记表外的药剂（含自配药、未知商品名）不给用法。

## 数值依据

以登记表为准。
