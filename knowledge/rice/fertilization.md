---
kb_id: rice/fertilization/n-budget
title: 单季粳稻总施氮量与氮肥运筹
crop: rice
stage: [秧苗期, 返青期, 分蘖期, 拔节孕穗期]
region: jiangsu
category: [fertilize]
topics: [fertilize.rate, fertilize.product]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: total_n_per_mu, label: 总施氮量（土壤氮中等）, value: 18, unit: kg N/亩, when: {soil_n_level: 中}}
  - {id: total_n_per_mu, label: 总施氮量（土壤氮低）, value: 20, unit: kg N/亩, when: {soil_n_level: 低}}
  - {id: total_n_per_mu, label: 总施氮量（土壤氮高）, value: 16, unit: kg N/亩, when: {soil_n_level: 高}}
  - {id: basal_ratio, label: 基肥氮占比, value: 0.35, unit: 比例}
  - {id: tillering_ratio, label: 分蘖肥氮占比, value: 0.25, unit: 比例}
  - {id: panicle_promote_ratio, label: 促花肥（倒4叶）氮占比, value: 0.24, unit: 比例}
  - {id: panicle_protect_ratio, label: 保花肥（倒2叶）氮占比, value: 0.16, unit: 比例}
  - {id: urea_n_content, label: 尿素含氮量, value: 0.46, unit: 比例}
  - {id: compound_n_content, label: 45% 复合肥（15-15-15）含氮量, value: 0.15, unit: 比例}
  - {id: kcl_k2o_content, label: 氯化钾 K2O 含量, value: 0.60, unit: 比例}
formulas:
  - id: n_for_stage
    calc: Fertilizer
    expr: n_kg_per_mu = total_n_per_mu * stage_ratio
    inputs: [total_n_per_mu, stage_ratio]
    output: {name: n_kg_per_mu, unit: kg N/亩}
  - id: product_per_mu
    calc: Fertilizer
    expr: product_kg_per_mu = n_kg_per_mu / product_n_content
    inputs: [n_kg_per_mu, product_n_content]
    output: {name: product_kg_per_mu, unit: kg/亩}
  - id: product_total
    calc: Fertilizer
    expr: product_total_kg = product_kg_per_mu * area_mu
    inputs: [product_kg_per_mu, area_mu]
    output: {name: product_total_kg, unit: kg}
basis:
  - {source: 江苏水稻精确定量栽培氮肥运筹（基蘖肥:穗肥约 6:4，具体文献待专家指定）, level: 专家经验, verified: false}
---
## 适用生育期

本田整地（基肥）至拔节孕穗期（穗肥）。

## 指标与阈值

- 总施氮量按土壤氮水平取值：低 20、中 18、高 16 kg N/亩（`field.profile.soil_n_level`）。
- 运筹比例：基肥 35%、分蘖肥 25%、促花肥 24%、保花肥 16%（基蘖肥:穗肥 = 6:4）。

## 操作要点

- 先由 `field.profile` 取土壤氮水平与面积，再按当前生育期选比例，最后按肥料含氮量换算实物量。
- 氮肥实物默认：基肥用 45%（15-15-15）复合肥，追肥用尿素；用户指定其他肥料时须给出含氮量，否则拒答。
- 前一次追肥未满 7 天不重复追施同一时期肥料。

## 数值依据

计算链：`n = total_n_per_mu × stage_ratio` → `kg/亩 = n ÷ 含氮量` → `总量 = kg/亩 × area_mu`。例：土壤氮中等、促花肥、尿素 → 18 × 0.24 ÷ 0.46 = 9.39 kg/亩。
---
kb_id: rice/fertilization/basal
title: 基肥施用
crop: rice
stage: [秧苗期]
region: jiangsu
category: [fertilize]
topics: [fertilize.timing, fertilize.product]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: basal_days_before_transplant, label: 基肥施用距移栽天数, min: 1, max: 3, unit: 天}
  - {id: basal_kcl_per_mu, label: 基肥氯化钾用量, min: 5, max: 7.5, unit: kg/亩}
basis:
  - {source: rice/fertilization/n-budget@v1（基肥占比 35%）, level: 专家经验, verified: false}
---
## 适用生育期

本田整地至移栽前（归入 `秧苗期`）。

## 指标与阈值

- 移栽前 1–3 天结合耙田施入。
- 基肥氮占总氮 35%，用量按 `rice/fertilization/n-budget` 计算；配施氯化钾 5–7.5 kg/亩。

## 操作要点

- 优先选 45%（15-15-15）复合肥，全层施用后耙匀；缺磷田块可增施过磷酸钙。
- 秸秆全量还田田块，基肥氮可上浮 10%（专家确认后写入参数）。

## 数值依据

见 front-matter `params`；氮量公式引用 `rice/fertilization/n-budget@v1#product_per_mu`。
---
kb_id: rice/fertilization/tillering
title: 分蘖肥施用时机与用量
crop: rice
stage: [返青期, 分蘖期]
region: jiangsu
category: [fertilize]
topics: [fertilize.timing, fertilize.rate]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: apply-window-start, label: 分蘖肥最早施用, metric: days_after_transplant, op: ">=", value: 5, unit: 天, action: 返青后施第一次分蘖肥}
  - {id: apply-window-end, label: 分蘖肥最晚施用, metric: days_after_transplant, op: "<=", value: 15, unit: 天, action: 超过则不再追施分蘖肥，防止无效分蘖}
params:
  - {id: tillering_split_first, label: 第一次分蘖肥占分蘖肥比例, value: 0.6, unit: 比例}
  - {id: tillering_split_second, label: 第二次分蘖肥占分蘖肥比例, value: 0.4, unit: 比例}
basis:
  - {source: rice/fertilization/n-budget@v1（分蘖肥占比 25%）, level: 专家经验, verified: false}
---
## 适用生育期

返青期至分蘖期（移栽后 5–15 天）。

## 指标与阈值

- 移栽后 5–7 天施第一次（占分蘖肥 60%），12–15 天施第二次（40%）。
- 移栽后超过 15 天或已进入分蘖末期，不再追施分蘖肥。

## 操作要点

- 施肥前保持浅水层 2–3 cm，施后 3–5 天不排水。
- 分蘖肥总量 = 总氮 × 25% ÷ 尿素含氮量，例：土壤氮中等 → 18 × 0.25 ÷ 0.46 = 9.78 kg 尿素/亩。

## 数值依据

见 front-matter；氮量公式引用 `rice/fertilization/n-budget@v1`。
---
kb_id: rice/fertilization/panicle
title: 穗肥（促花肥、保花肥）施用
crop: rice
stage: [拔节孕穗期]
region: jiangsu
category: [fertilize]
topics: [fertilize.timing, fertilize.rate]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: promote-leaf-age, label: 促花肥叶龄, metric: leaf_age_remaining, op: "==", value: 4, unit: 叶, action: 倒4叶露尖（幼穗分化期）施促花肥}
  - {id: protect-leaf-age, label: 保花肥叶龄, metric: leaf_age_remaining, op: "==", value: 2, unit: 叶, action: 倒2叶露尖施保花肥}
  - {id: leaf-color-high, label: 叶色过深推迟或减量, metric: leaf_color_chart, op: ">=", value: 4, unit: 级, action: 叶色卡 ≥ 4 级时促花肥减半或不施}
basis:
  - {source: rice/fertilization/n-budget@v1（促花肥 24%、保花肥 16%）, level: 专家经验, verified: false}
---
## 适用生育期

拔节孕穗期：促花肥在幼穗分化期（倒 4 叶露尖），保花肥在倒 2 叶露尖。

## 指标与阈值

- 促花肥占总氮 24%，保花肥占 16%。
- 叶色卡 ≥ 4 级（叶色浓绿）时促花肥减半或不施，防倒伏与纹枯病加重。

## 操作要点

- 幼穗分化期施促花肥：尿素用量 = 总氮 × 0.24 ÷ 0.46；土壤氮中等时为 9.39 kg/亩，总量再乘地块面积。
- 施肥时保持浅水，施后不排水；倒伏风险高的田块与 `rice/disaster/lodging` 一并考虑。

## 数值依据

公式引用 `rice/fertilization/n-budget@v1#product_per_mu` 与 `#product_total`。
---
kb_id: rice/fertilization/foliar
title: 灌浆期叶面追肥（粒肥）
crop: rice
stage: [抽穗扬花期, 灌浆结实期]
region: jiangsu
category: [fertilize]
topics: [fertilize.product, fertilize.rate]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: kh2po4_per_mu, label: 磷酸二氢钾亩用量, min: 100, max: 150, value: 100, unit: g/亩}
  - {id: kh2po4_concentration, label: 磷酸二氢钾喷施浓度, min: 0.002, max: 0.003, unit: 比例}
  - {id: foliar_water_per_mu, label: 叶面喷施亩兑水量, value: 50, unit: L/亩}
  - {id: foliar_times, label: 喷施次数, min: 1, max: 2, unit: 次}
basis:
  - {source: 磷酸二氢钾叶面喷施常规用法（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

始穗至灌浆中期。

## 指标与阈值

- 磷酸二氢钾 100–150 g/亩，兑水 50 L（浓度 0.2%–0.3%），间隔 7 天喷 1–2 次。

## 操作要点

- 选晴天傍晚或阴天喷施，喷后 4 小时内遇雨补喷。
- 抽穗后不再追施氮肥（土壤追施），防贪青晚熟。
- 本条目只覆盖磷酸二氢钾；富硒、富锌等功能性叶面肥不在 P0 知识范围（`KB_MISS`）。

## 数值依据

见 front-matter `params`。
---
kb_id: rice/fertilization/deficiency-n
title: 缺氮诊断
crop: rice
stage: [分蘖期, 拔节孕穗期]
region: jiangsu
category: [fertilize]
topics: [fertilize.deficiency]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
thresholds:
  - {id: leaf-color-low, label: 叶色偏淡, metric: leaf_color_chart, op: "<", value: 3, unit: 级, action: 叶色卡低于 3 级疑似缺氮，现场核实后补施}
params:
  - {id: rescue_urea_per_mu, label: 缺氮补施尿素, min: 3, max: 5, unit: kg/亩}
basis:
  - {source: 水稻营养诊断常规症状（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

分蘖期至拔节孕穗期。

## 指标与阈值

- 症状：植株矮小、分蘖少，老叶先黄、由下往上发展，叶片窄短直立。
- 叶色卡 < 3 级视为偏淡。

## 操作要点

- 先排除缺钾（叶尖叶缘褐斑）、缺锌（新叶基部白化、坐蔸）与根系受害（黑根、僵苗），再判缺氮。
- 确认后补施尿素 3–5 kg/亩，并计入总氮预算，后续穗肥相应减量。
- 只凭文字描述时只给观察类建议，作业结论须有现场核实。

## 数值依据

见 front-matter。
---
kb_id: rice/fertilization/deficiency-k
title: 缺钾诊断
crop: rice
stage: [分蘖期, 拔节孕穗期]
region: jiangsu
category: [fertilize]
topics: [fertilize.deficiency]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: rescue_kcl_per_mu, label: 缺钾补施氯化钾, min: 5, max: 7.5, unit: kg/亩}
basis:
  - {source: 水稻营养诊断常规症状（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

分蘖期至拔节孕穗期。

## 指标与阈值

- 症状：老叶叶尖、叶缘先出现褐色斑点并逐渐焦枯，根系发黑早衰；易与胡麻叶斑病混淆。

## 操作要点

- 补施氯化钾 5–7.5 kg/亩；沙性土、秸秆移出田块更易缺钾。
- 伴随大量褐色病斑时转 `protect.identify` 排查病害。

## 数值依据

见 front-matter。
---
kb_id: rice/fertilization/deficiency-zn
title: 缺锌（坐蔸）诊断
crop: rice
stage: [返青期, 分蘖期]
region: jiangsu
category: [fertilize]
topics: [fertilize.deficiency]
authority_level: 专家经验
version: v1
author: claude-draft
review_status: pending_expert
effective_from: 2026-10-28
params:
  - {id: znso4_concentration, label: 硫酸锌叶面喷施浓度, value: 0.002, unit: 比例}
  - {id: foliar_water_per_mu, label: 叶面喷施亩兑水量, value: 50, unit: L/亩}
basis:
  - {source: 水稻营养诊断常规症状（待专家核对）, level: 专家经验, verified: false}
---
## 适用生育期

返青期至分蘖期。

## 指标与阈值

- 症状：移栽后迟迟不发、分蘖停滞（坐蔸），新叶基部褪绿白化，老叶出现褐色斑点。

## 操作要点

- 叶面喷施 0.2% 硫酸锌溶液，每亩 50 L，间隔 7 天喷 2 次；同时浅水勤灌、适当露田增氧。
- 石灰性土、冷浸田更易发生。

## 数值依据

见 front-matter。
