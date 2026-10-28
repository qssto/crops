---
kb_id: rice/disaster/heat
title: 抽穗扬花期高温热害防御
crop: rice
stage: [拔节孕穗期, 抽穗扬花期, 灌浆结实期]
region: jiangsu
category: [disaster]
topics: [disaster.heat]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: heat-days, label: 高温热害指标, metric: max_temp_c, op: ">=", value: 35, unit: ℃, duration_days: 3, stage: [抽穗扬花期], action: 日最高气温 ≥ 35 ℃ 持续 3 天以上，启动热害防御}
  - {id: heat-filling, label: 灌浆期高温指标, metric: max_temp_c, op: ">=", value: 35, unit: ℃, duration_days: 3, stage: [灌浆结实期], action: 影响灌浆，加强水分管理}
params:
  - {id: deep_water_cm, label: 调温深水层, min: 5, max: 10, unit: cm}
  - {id: kh2po4_concentration, label: 叶面喷施磷酸二氢钾浓度, value: 0.002, unit: 比例}
basis:
  - {source: 水稻高温热害指标与防御措施（气象行业标准口径，具体标准号待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

孕穗末期至灌浆结实期，抽穗扬花期最敏感。

## 指标与阈值

- 抽穗扬花期日最高气温 ≥ 35 ℃ 持续 ≥ 3 天，花粉活力下降、结实率降低。

## 操作要点

- 高温来临前灌深水 5–10 cm，有条件的「日灌夜排」或长流水降温。
- 叶面喷施 0.2% 磷酸二氢钾（`rice/fertilization/foliar`），避开中午高温时段。
- 高温期间推迟施药作业至早晚（`common/operation_window/spray`）。
- 灾后调查结实率，属观察类建议，不直接生成作业单。

## 数值依据

见 front-matter。
---
kb_id: rice/disaster/cold
title: 低温冷害与寒露风防御
crop: rice
stage: [拔节孕穗期, 抽穗扬花期]
region: jiangsu
category: [disaster]
topics: [disaster.cold]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: mean-temp, label: 粳稻寒露风指标（日均温）, metric: mean_temp_c, op: "<=", value: 20, unit: ℃, duration_days: 3, stage: [抽穗扬花期], action: 日平均气温 ≤ 20 ℃ 持续 3 天，启动防御}
  - {id: min-temp, label: 粳稻寒露风指标（日最低温）, metric: min_temp_c, op: "<=", value: 17, unit: ℃, duration_days: 2, stage: [抽穗扬花期], action: 日最低气温 ≤ 17 ℃ 持续 2 天，启动防御}
  - {id: booting-cold, label: 孕穗期低温, metric: mean_temp_c, op: "<=", value: 17, unit: ℃, duration_days: 2, stage: [拔节孕穗期], action: 减数分裂期低温，灌深水保温}
params:
  - {id: warm_water_cm, label: 保温深水层, min: 5, max: 10, unit: cm}
basis:
  - {source: 水稻寒露风气象指标（具体标准号待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

孕穗期（减数分裂）至抽穗扬花期。

## 指标与阈值

- 粳稻抽穗扬花期日平均气温 ≤ 20 ℃ 连续 3 天，或日最低气温 ≤ 17 ℃ 连续 2 天。
- 孕穗期日平均气温 ≤ 17 ℃ 连续 2 天。

## 操作要点

- 低温来临前灌深水 5–10 cm 保温，低温过后逐步排浅，不急排。
- 低温过后叶面喷施磷酸二氢钾促灌浆。
- 伴随大风时注意倒伏（`rice/disaster/lodging`）。

## 数值依据

见 front-matter。
---
kb_id: rice/disaster/typhoon
title: 台风暴雨前防备
crop: rice
stage: ["*"]
region: jiangsu
category: [disaster]
topics: [disaster.storm]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: heavy-rain, label: 暴雨预警, metric: expected_rain_mm, op: ">=", value: 50, unit: mm, action: 预计过程降雨 ≥ 50 mm，提前疏通沟渠、降低田间水位}
  - {id: strong-wind, label: 大风预警, metric: max_wind_ms, op: ">=", value: 13.9, unit: m/s, action: 7 级以上大风，暂停一切施药与飞防作业}
basis:
  - {source: 台风暴雨农业防灾常规措施（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期，8–9 月穗期影响最大。

## 指标与阈值

- 预计过程降雨 ≥ 50 mm：提前排水预降田间水位。
- 风速 ≥ 13.9 m/s（7 级）：停止施药、飞防作业。

## 操作要点

- 台风前疏通「三沟」（围沟、腰沟、厢沟），检查排灌泵站。
- 已到防治指标但台风临近的田块，推迟施药至过境后，避免药液被冲刷。
- 灾后按 `rice/disaster/flood` 排水扶理，并防白叶枯病、纹枯病等加重。

## 数值依据

见 front-matter。
---
kb_id: rice/disaster/flood
title: 暴雨淹涝后的排水与恢复
crop: rice
stage: ["*"]
region: jiangsu
category: [disaster]
topics: [disaster.storm]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: submerge-booting, label: 孕穗期淹没耐受, metric: submerged_hours, op: ">=", value: 24, unit: h, stage: [拔节孕穗期], action: 孕穗期没顶超过 24 h 损失大，须抢排}
  - {id: submerge-tillering, label: 分蘖期淹没耐受, metric: submerged_hours, op: ">=", value: 72, unit: h, stage: [分蘖期], action: 分蘖期没顶超过 72 h 须抢排并补肥}
params:
  - {id: rescue_urea_per_mu, label: 退水后补施尿素, min: 3, max: 5, unit: kg/亩}
basis:
  - {source: 水稻涝灾后管理常规措施（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

全生育期，孕穗期最不耐淹。

## 指标与阈值

- 孕穗期没顶超过 24 h、分蘖期没顶超过 72 h 损失明显。

## 操作要点

- 尽快排水露出叶尖，排水时留浅水，避免烈日下一次排干导致青枯。
- 退水后清洗叶面泥浆，分蘖期补施尿素 3–5 kg/亩。
- 灾损定损、保险理赔不在本知识库范围（`KB_MISS`）。

## 数值依据

见 front-matter。
---
kb_id: rice/disaster/lodging
title: 倒伏预防
crop: rice
stage: [分蘖期, 拔节孕穗期, 抽穗扬花期, 灌浆结实期]
region: jiangsu
category: [disaster]
topics: [disaster.lodging]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: tall-jointing, label: 拔节期株高偏高, metric: plant_height_cm, op: ">=", value: 75, unit: cm, stage: [拔节孕穗期], action: 拔节期株高 ≥ 75 cm 视为旺长，控氮并重晒田}
  - {id: leaf-color-dark, label: 叶色过深, metric: leaf_color_chart, op: ">=", value: 4, unit: 级, action: 促花肥减半或不施}
basis:
  - {source: 水稻倒伏成因与防控常规措施（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

分蘖期至灌浆结实期，控制措施主要在拔节前后。

## 指标与阈值

- 拔节期株高 ≥ 75 cm 或叶色卡 ≥ 4 级视为旺长，倒伏风险高。

## 操作要点

- 控氮：促花肥减半或不施（`rice/fertilization/panicle`），增施钾肥壮秆。
- 适度重晒田（`rice/irrigation/drain`），控制基部节间伸长。
- 植物生长调节剂只能在登记范围内使用，P0 登记表未收录，相关产品与用量交专家确定，不由系统推荐。
- 已倒伏田块：灌浆期倒伏及时排水，不强行扶起。

## 数值依据

见 front-matter。
