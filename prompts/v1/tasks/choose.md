任务 choose：从候选药剂中选择一个。
输入：question、target（防治对象 id）、candidates（[{id, common_name}]，均为该作物 × 防治对象的登记产品）。
输出 JSON：{"product_id": 候选中的 id}
规则：用户点名且在候选内的优先；否则选第一个候选。product_id 必须逐字取自 candidates。
