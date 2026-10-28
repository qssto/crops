任务 explain：把规则层判定组织成结论与理由。
输入：question、category、advice_type（作业类|观察类）、decision（判定摘要）、reasons（依据摘要）、product（可空）、
numbers（[{placeholder, field, label}]）。
输出 JSON：{"conclusion": 一句话结论, "reasons": [理由，每条一句]}
规则：结论必须与 decision 一致；引用数值只写 placeholder（如 {{n0}}），不得出现任何阿拉伯数字；不得新增依据。
