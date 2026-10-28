# 固定知识库（P0 · L3）

> **警告：本目录全部内容由 Claude 草拟（`author: claude-draft`，`review_status: pending_expert`），待农艺专家逐条核对，不得作为真实用药、施肥依据。** 农药登记信息（用量、PHI、每季次数）凭常见登记标签整理，登记证号未填，必须对照中国农药信息网逐条核实后才能改为 `signed`。

场景：水稻、江苏、示范农场1。目录结构按母文档 §5.0.7：

```
knowledge/
├── index.yaml                  # 条目索引：kb_id → 文件 → 适用维度（由各条目 front-matter 汇总）
├── rice/
│   ├── phenology.md            # 生育期与物候判定
│   ├── fertilization.md        # 施肥方案与用量公式
│   ├── irrigation.md           # 水层管理、晒田、灌水量
│   ├── pest/<target>.md        # 一病一虫一文件：识别 / 防治指标 / 药剂 / 用量 / PHI
│   └── disaster.md             # 高温、低温、台风暴雨、倒伏
└── common/
    ├── operation_window.md     # 作业窗口硬约束、亩用量与稀释换算
    ├── safety.md               # 安全间隔期、混用禁忌、抗药性、防护、登记合规
    └── pesticide_registry.yaml # 农药登记表（药剂候选集合的唯一来源）
```

## 1. 条目文件格式

一个 `.md` 文件可以含多个条目。每个条目 = 一段 YAML front-matter + 一段正文，front-matter 用单独一行的 `---` 包起来，条目首尾相接：

```markdown
---
kb_id: rice/fertilization/basal
...
---
## 适用生育期
...
---
kb_id: rice/fertilization/tillering
...
---
## 适用生育期
...
```

- 文件必须以 `---` 开头；按「单独一行的 `---`」切分后依次为 front-matter、正文、front-matter、正文……
- 正文里**不得**出现单独一行的 `---`（Markdown 分隔线改用 `***` 或不用）。
- 正文固定四个二级小节，顺序固定：`## 适用生育期`、`## 指标与阈值`、`## 操作要点`、`## 数值依据`；植保条目可在最前面加 `## 识别特征`。
- 正文中出现的数值必须与 front-matter 结构化字段一致，**calc 只读 front-matter**，正文供模型阅读与专家审核。

## 2. front-matter 字段

| 字段 | 必填 | 说明 |
|------|------|------|
| `kb_id` | 是 | 见 §4 命名规则，全库唯一 |
| `title` | 是 | 中文标题 |
| `crop` | 是 | `rice`；通用条目写 `"*"` |
| `stage` | 是 | 生育期列表，取值见 §3；全生育期写 `["*"]` |
| `region` | 是 | `jiangsu`；不分区域写 `"*"` |
| `category` | 是 | 一级 taxonomy code 列表（`fertilize` / `protect` / `irrigate` / `disaster`，见 `eval/taxonomy.yaml`），至少 1 个 |
| `topics` | 是 | 子类 code 列表（如 `protect.timing`），每个都必须隶属于 `category` 中的某个一级 code |
| `target` | 植保条目必填 | 防治对象 id，与 `pesticide_registry.yaml` 的 `targets` 一致；calc 据此出 `product_candidates` |
| `authority_level` | 是 | `国标` / `地标` / `企业标准` / `专家经验`，加载排序按此顺序从高到低。草拟条目统一写 `专家经验`，专家附上已核实来源后再上调 |
| `version` | 是 | `v1` 起，内容（含数值）变化即 +1；引用写成 `kb:<kb_id>@<version>` |
| `author` | 是 | 署名；草拟写 `claude-draft` |
| `review_status` | 是 | `pending_expert` / `signed`；`claude-draft` 只能是 `pending_expert` |
| `effective_from` | 是 | `YYYY-MM-DD` |
| `thresholds` | 否 | 防治指标 / 判定阈值，见 §5.1 |
| `params` | 否 | calc 可用参数，见 §5.2 |
| `formulas` | 否 | calc 公式声明，见 §5.3 |
| `basis` | 是 | 数值依据来源列表，见 §5.4 |

加载器校验：必填项缺失、枚举越界、`topics` 不属于 `category`、`kb_id` 重复、与 `index.yaml` 不一致，任一发生即拒绝加载（P0 §6.4-2）。

## 3. 生育期 `stage` 枚举（与 mcp-crop 物候判定共用）

| 值 | 范围 | 常见细分（只出现在工具返回的 `sub_stage`，不参与过滤） |
|----|------|------|
| `秧苗期` | 播种至移栽（含浸种、育秧、本田整地施基肥） | 浸种催芽、秧田期 |
| `返青期` | 移栽后至新根新叶长出（约 5–7 天） | — |
| `分蘖期` | 返青后至拔节前 | 分蘖初期、分蘖盛期、分蘖末期 |
| `拔节孕穗期` | 拔节至破口前 | 拔节期、幼穗分化期、孕穗期 |
| `抽穗扬花期` | 破口至齐穗、开花结束 | 破口期、始穗期、齐穗期 |
| `灌浆结实期` | 开花后至蜡熟 | 乳熟期、蜡熟期 |
| `成熟期` | 黄熟至收获 | 黄熟期、完熟期 |

`crop.phenology` 工具返回 `stage` 必须是上表的值，细分写在 `sub_stage`。`"*"` 只用于条目，表示全生育期适用。

## 4. `kb_id` 命名规则

`{crop|common}/{文件主题}/{slug}`，如 `rice/pest/planthopper`、`rice/fertilization/panicle`、`common/safety/phi`。

- 第一段：作物 code（`rice`）或 `common`；第二段：所在文件名（不含 `.md`），`pest/` 下文件第二段固定为 `pest`；第三段 slug 小写，单词间用 `-`。每段匹配 `^[a-z0-9]+([_-][a-z0-9]+)*$`。
- 由 kb_id 可推出文件：`rice/pest/<slug>` → `rice/pest/<slug>.md`，其余 → `<第一段>/<第二段>.md`。
- kb_id 一经发布不改名；废弃条目保留并在 `index.yaml` 标 `deprecated: true`。
- 引用（样本 `must_cite`、AdviceCard `Citation`）写 `kb:<kb_id>@<version>`，如 `kb:rice/pest/planthopper@v1`。

## 5. 结构化数值字段

### 5.1 `thresholds`：指标与阈值

```yaml
thresholds:
  - id: tillering-booting          # 条目内唯一
    label: 分蘖至孕穗期防治指标
    metric: density_per_100_hills  # 与工具返回字段同名，便于直接比较
    op: ">="                        # >= / > / <= / < / ==
    value: 1000
    unit: 头/百丛
    stage: [分蘖期, 拔节孕穗期]      # 可选，缺省为条目 stage
    duration_days: 3               # 可选：连续天数类阈值（高温、低温）
    action: 达到即建议施药
```

### 5.2 `params`：calc 参数

```yaml
params:
  - id: uav_spray_volume           # calc 按 kb_id + id（+ when 条件）取值；同一 id 可按不同 when 出现多次，id+when 组合须唯一
    label: 无人机亩施药液量
    value: 1.5                     # 点值；或只写 min / max 表示区间；可三者都写（value 为默认取值）
    min: 1.0
    max: 2.0
    unit: L/亩
    when: {soil_n_level: 中}        # 可选：适用条件（键为工具返回字段）
```

### 5.3 `formulas`：公式声明

```yaml
formulas:
  - id: total_dose
    calc: Dosage                   # calc 包中的实现：Dosage / Dilution / PHIDate / Fertilizer / WaterVolume
    expr: total = dose_per_mu * area_mu
    inputs: [dose_per_mu, area_mu]
    output: {name: total, unit: 与亩用量单位的分子相同}
```

calc 在 Go 中实现公式，`expr` 是人读的定义与单测依据；结果 trace 写 `formula: <kb_id>@<version>#<formula id>`。约定：中间结果保留全精度，只在输出时按样本 `precision` 舍入。

### 5.4 `basis`：数值依据

```yaml
basis:
  - source: 稻飞虱测报与防治指标（江苏省植保站，具体文号待核对）
    level: 地标                    # 同 authority_level 枚举
    verified: false               # 专家核对原文后改为 true
```

## 6. `index.yaml` 格式

```yaml
version: v1
entries:
  - kb_id: rice/pest/planthopper
    file: rice/pest/planthopper.md
    version: v1
    crop: rice
    stage: [分蘖期, 拔节孕穗期, 抽穗扬花期, 灌浆结实期]
    region: jiangsu
    category: [protect]
    topics: [protect.identify, protect.timing, protect.product, protect.dosage]
    target: planthopper            # 仅植保条目
    authority_level: 专家经验
```

`index.yaml` 是各条目 front-matter 中检索维度的汇总，加载器据此选片段（按 `crop / stage / region / category` 过滤 → `authority_level` 排序 → 截断，P0 §6.4）；与 front-matter 不一致时拒绝加载。

## 7. 农药登记表 `common/pesticide_registry.yaml`

- `targets`：防治对象 id → 中文名。
- `products[]`：`id`（稳定英文 id：`<有效成分英文>-<含量><剂型代码>`，如 `chlorantraniliprole-200sc`）、`common_name`、`formulation`、`content`、`registration_no`（待核对，未填）、`registrations[]`。
- `registrations[]`：`crop`、`target`、`dose{min,max,unit}`（`unit` 为 `g/亩`、`mL/亩` 或 `倍`）、`method`、`dilution_note`、`phi_days`（不适用写 `null`，如浸种、播后封闭）、`max_times_per_season`、`regions`（`"*"` 表示全国登记）、`confidence`（草拟时自评 `high` / `medium`，供专家排核对优先级）。
- `out_of_set_test: true` 的产品刻意**没有**水稻登记，只供「集合外药剂」测试；calc 按 `crop × target × region` 过滤后自然不会出现在候选集合中。
- 候选集合 `product_candidates(crop, target, region)` = 存在一条 `registrations` 满足三者匹配的产品 id。
