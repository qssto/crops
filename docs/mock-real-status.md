# MCP 工具 mock / real 状态（P0 L2）

四个 Server 由 `configs/tools.yaml` 按 `tools.<server>.mode` 选 `mock` / `real`（`servers.Build`）；切换只改配置或环境变量 `AS_TOOLS_<SERVER>_MODE`，real 的 `api_key` 只来自 `AS_TOOLS_<SERVER>_REAL_API_KEY`。
P0 全部为 mock：数据在 `configs/mock/{weather,crop,field,device}.yaml`，时间经 `clock.Clock`，结果都带 `source` 与 `data_freshness_sec`。
real 骨架（`servers/kit/real.go`）与 mock 共用同一份 ToolSpec，P0 不调用外网，一律返回 `UPSTREAM "real not configured"`。

| 工具 | 当前 | real 接口文档 | mock 与真实的差距 |
|------|------|---------------|-------------------|
| `weather.realtime` | mock（mock-station，观测滞后 20 分钟） | 待对接：气象源 D6 | 10 天循环数据投影到任意日期，地块不区分小气候；无站点 / 格点坐标 |
| `weather.forecast` | mock | 待对接：气象源 D6 | 逐日预报由同一循环生成（`rain_prob_pct` 为循环日手写值），无逐小时；1..7 天 |
| `weather.alert` | mock | 待对接：气象源 D6 | 预警手写在循环日上（高温黄色、暴雨蓝色），3 天视野；无官方预警编号与发布单位 |
| `weather.history` | mock | 待对接：气象源 D6 | 最多 31 天、截止昨日；汇总按循环数据计算，非真实观测 |
| `crop.phenology` | mock（mock-agri-kb） | 待对接：极飞 XGEO（或农技知识库 L3） | 按播期 + 月均温表累积积温推算，不用实测气温与遥感；品种差异未建模 |
| `crop.variety` | mock | 待对接：极飞 XGEO / 品种审定数据 | 只登记南粳9108、南粳5055 两个品种 |
| `crop.pest_spectrum` | mock | 待对接：农技知识库（L3） | 静态病虫谱，按生育期过滤，无区域与年份发生趋势 |
| `crop.threshold` | mock | 待对接：农技知识库（L3） | 只覆盖水稻主要病虫的静态防治指标；数值为草拟，待专家确认 |
| `field.profile` | mock（mock-xgeo） | 待对接：极飞 XGEO | field-001..006 静态档案，与种子 `configs/seed/fields.yaml` 对齐；无边界几何 |
| `field.history` | mock | 待对接：极飞 XGEO | 农事记录手写；晚于当前时钟的记录过滤掉 |
| `field.pest_survey` | mock | 待对接：极飞 XGEO | 人工虫情调查样例；field-005 无调查记录（EMPTY）；无图像识别结果 |
| `field.soil` | mock | 待对接：极飞 XGEO | 单次土测快照；field-006 无记录（EMPTY） |
| `device.list` | mock | 待对接：极飞 XGEO | 4 台设备（2 架 P100 无人机、1 个离线水泵、1 个土壤传感器）；P0 只读 |
| `device.status` | mock | 待对接：极飞 XGEO | 状态与电量静态，新鲜度按心跳滞后计算；无遥测流 |

## 共同缺口

- `field_id → ext_field_code` 走 `repository.FieldDirectory`（查 `field_profile`）；real 对接时需确认上游地块 id 与 `ext_field_code` 的映射。
- gateway（超时重试、去重缓存、错误码归一、`tool_call` 落库）由 L2-a 提供；`cmd/server` 当前注入的是返回 `ERROR/UPSTREAM` 的占位 ToolProvider。
- `level` 全部为 `read`；`device` 的写 / 控制类工具（actuate）在 L5 之后另立。
