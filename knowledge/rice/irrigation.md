---
kb_id: rice/irrigation/seedling-tillering
title: 移栽返青与分蘖期水层管理
crop: rice
stage: [返青期, 分蘖期]
region: jiangsu
category: [irrigate]
topics: [irrigate.depth, irrigate.timing]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: regreening_depth_cm, label: 返青期水层, min: 3, max: 5, unit: cm}
  - {id: tillering_depth_cm, label: 分蘖期浅水层, min: 1, max: 3, unit: cm}
  - {id: irrigate_trigger_depth_cm, label: 分蘖期补水下限（水层落干即补）, value: 0, unit: cm}
basis:
  - {source: 水稻浅湿灌溉常规做法（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

返青期、分蘖期。

## 指标与阈值

- 返青期寸水护苗，水层 3–5 cm。
- 分蘖期浅水 1–3 cm，自然落干至田面无明水后再补灌，浅湿交替促分蘖。

## 操作要点

- 施分蘖肥、除草剂时按 `rice/irrigation/spray-water` 保水。
- 遇高温晴热天气午间不灌冷井水；遇暴雨及时排水防淹苗。
- 判断是否灌水需同时有物候与未来 72 h 降雨预报，缺任何一项不给灌水结论。

## 数值依据

见 front-matter `params`。
---
kb_id: rice/irrigation/drain
title: 够苗晒田（搁田）
crop: rice
stage: [分蘖期, 拔节孕穗期]
region: jiangsu
category: [irrigate]
topics: [irrigate.drain, irrigate.timing]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: enough-tillers, label: 够苗标准, metric: tillers_per_mu_10k, op: ">=", value: 18, unit: 万/亩, action: 茎蘖数达到预期穗数 80%（预期穗数按 22 万/亩计）即排水晒田}
  - {id: rain-forecast, label: 晒田期降雨上限, metric: rain_mm_72h, op: "<", value: 20, unit: mm, action: 未来 72 h 降雨 ≥ 20 mm 时推迟晒田或开好排水沟}
params:
  - {id: target_panicles_10k, label: 预期有效穗数, value: 22, unit: 万/亩}
  - {id: enough_ratio, label: 够苗比例, value: 0.8, unit: 比例}
  - {id: drain_days, label: 晒田天数, min: 7, max: 10, unit: 天}
basis:
  - {source: 江苏粳稻够苗晒田常规做法（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

分蘖末期至拔节初期。

## 指标与阈值

- 茎蘖数 ≥ 预期穗数 × 80%（22 × 0.8 = 17.6，取 18 万/亩）即开始排水晒田。
- 未来 72 h 降雨 ≥ 20 mm 时推迟或先开排水沟。

## 操作要点

- 晒 7–10 天，至田面开细裂、脚踩不陷、叶色褪淡；长势旺的田重晒，长势弱的田轻晒。
- 晒田后复水按 `rice/irrigation/rewater`。

## 数值依据

见 front-matter。
---
kb_id: rice/irrigation/rewater
title: 晒田后复水与拔节期干湿交替
crop: rice
stage: [拔节孕穗期]
region: jiangsu
category: [irrigate]
topics: [irrigate.depth, irrigate.timing, irrigate.amount]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: rewater_depth_cm, label: 复水水层, min: 2, max: 3, value: 3, unit: cm}
  - {id: alternate_cycle_days, label: 干湿交替周期, min: 3, max: 5, unit: 天}
basis:
  - {source: 水稻干湿交替灌溉常规做法（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

拔节孕穗期（晒田结束后至破口前）。

## 指标与阈值

- 晒田达标后复水 2–3 cm，之后每 3–5 天一次灌水—自然落干的干湿交替。

## 操作要点

- 复水灌水量按 `rice/irrigation/water-volume` 计算。
- 幼穗分化至孕穗为需水敏感期，落干不超过 2 天，不得重晒。

## 数值依据

见 front-matter。
---
kb_id: rice/irrigation/heading
title: 孕穗至抽穗扬花期水层管理
crop: rice
stage: [拔节孕穗期, 抽穗扬花期]
region: jiangsu
category: [irrigate]
topics: [irrigate.depth]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: heading_depth_cm, label: 抽穗扬花期水层, min: 3, max: 5, unit: cm}
basis:
  - {source: 水稻需水关键期常规做法（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

孕穗期至抽穗扬花期。

## 指标与阈值

- 保持浅水层 3–5 cm，不断水；此期缺水造成颖花退化、空秕率上升。

## 操作要点

- 遇 ≥ 35 ℃ 高温按 `rice/disaster/heat` 灌深水调温；遇低温按 `rice/disaster/cold` 灌深水保温。
- 施药期间保持水层，不排水。

## 数值依据

见 front-matter。
---
kb_id: rice/irrigation/grain-filling
title: 灌浆结实期干湿交替与收获前断水
crop: rice
stage: [灌浆结实期, 成熟期]
region: jiangsu
category: [irrigate]
topics: [irrigate.drain, irrigate.depth, irrigate.timing]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: cutoff_days_before_harvest, label: 收获前断水天数, min: 7, max: 10, unit: 天}
  - {id: alternate_cycle_days, label: 灌浆期干湿交替周期, min: 3, max: 5, unit: 天}
basis:
  - {source: 水稻后期水分管理常规做法（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

灌浆结实期至成熟期。

## 指标与阈值

- 灌浆期干湿交替，灌一次浅水后自然落干 3–5 天再灌。
- 收获前 7–10 天断水，不宜过早断水（易早衰、降低千粒重）。

## 操作要点

- 机收田块断水时间偏早一端，保证收割机下田不陷。
- 由预计收获日倒推断水日：断水日 = 预计收获日 − 7~10 天。

## 数值依据

见 front-matter。
---
kb_id: rice/irrigation/water-volume
title: 灌水量计算
crop: rice
stage: ["*"]
region: jiangsu
category: [irrigate]
topics: [irrigate.amount]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: m2_per_mu, label: 每亩面积, value: 666.67, unit: m2}
  - {id: loss_factor, label: 渗漏与补墒系数（晒田后复水）, value: 1.2, unit: 系数, when: {after_drain: true}}
  - {id: loss_factor, label: 渗漏系数（田面有水层时补水）, value: 1.05, unit: 系数, when: {after_drain: false}}
formulas:
  - id: water_m3
    calc: WaterVolume
    expr: water_m3 = area_mu * m2_per_mu * (target_depth_cm - current_depth_cm) / 100 * loss_factor
    inputs: [area_mu, target_depth_cm, current_depth_cm, loss_factor]
    output: {name: water_m3, unit: m3}
basis:
  - {source: 面积 × 水深体积换算；损失系数为草拟经验值（待专家按土质核对）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期。

## 指标与阈值

- 每亩 666.67 m²；1 cm 水层约 6.67 m³/亩。
- 晒田后复水土壤需先补墒，系数取 1.2；田面已有水层时补水取 1.05。

## 操作要点

- 例：28 亩晒田后复水到 3 cm（当前 0 cm）→ 28 × 666.67 × 0.03 × 1.2 ≈ 672 m³。
- 地块面积必须来自 `field.profile`，缺失时拒答。

## 数值依据

公式 `water_m3`，见 front-matter。
---
kb_id: rice/irrigation/spray-water
title: 施药、施肥前后的水层要求
crop: rice
stage: [返青期, 分蘖期, 拔节孕穗期, 抽穗扬花期]
region: jiangsu
category: [irrigate, protect]
topics: [irrigate.depth, protect.dosage]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: keep_water_depth_cm, label: 施药后保水水层, min: 3, max: 5, unit: cm}
  - {id: keep_water_days, label: 施药后保水天数, min: 3, max: 7, unit: 天}
basis:
  - {source: 水稻施药期水层常规要求（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

返青期至抽穗扬花期。

## 指标与阈值

- 防治二化螟、纹枯病：施药时田间保水 3–5 cm，保持 3–5 天。
- 药土法封闭除草：施药后保水 3–5 cm，保持 5–7 天，水层不淹没心叶。
- 茎叶喷雾除草：施药前排干田水，施药后 1–2 天复水。

## 操作要点

- 保水期内不排水、不串灌，避免药液流失与药害。

## 数值依据

见 front-matter。
