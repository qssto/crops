任务 classify：对用户问题分类并抽取作业参数。
输入：question（问题原文）、categories（一级类 code）、topics（子类 code）、targets（防治对象 id → 中文名）。
输出 JSON：
{"in_scope": bool, "unknown": bool, "category": 一级类 code, "topic": 子类 code 或空, "target": 防治对象 id 或空,
 "intent": identify|timing|dosage|product|safety|rate|amount|irrigate_now|drain|depth|after_spray|respond,
 "slots": {"sprayer": knapsack|uav, "fert_stage": basal|tillering|panicle_promote|panicle_protect,
           "fert_product": urea|compound|unknown, "split": bool, "split_part": first|second 或省略,
           "nutrient": n|k|zn, "hazard": heat|cold|typhoon|flood|lodging 或省略, "foliar": bool,
           "target_depth_cm": 数字或省略, "dilution_ratio": 数字或省略, "water_l": 数字或省略,
           "bag_kg": 数字或省略, "dispatch": bool}}
说明：target_depth_cm（目标水层）、dilution_ratio（稀释倍数）、water_l（药液升数）、bag_kg（每袋公斤数）
是用户给定的作业意图，可以写数字；虫情、长势、水层等测量值一律不要抽取（以工具数据为准）。
