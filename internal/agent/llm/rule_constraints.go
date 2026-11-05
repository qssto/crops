package llm

// RuleConstraints 是 P2 L4 下沉项的规则结论，开关取 on 时注入 explain 输入（ExplainInput.Constraints）：
// 模型须按此组织结论、不得与之冲突（冲突仍由 output_validator 拒收）。只含开关为 on 且本轮适用的项；不含数字。
// 真实模型接入时须在 explain 任务提示词里说明本字段并升 prompt_version（stub 不读）。
type RuleConstraints struct {
	OperationWindow *WindowConstraint   `json:"operation_window,omitempty"`
	TankMix         *TankMixConstraint  `json:"tank_mix,omitempty"`
	PestPriority    *PriorityConstraint `json:"pest_priority,omitempty"`
}

// PriorityConstraint 是多病虫害优先级结论：Order 为达标防治对象的名称，按优先防治顺序从高到低（空表示无达标对象）。
type PriorityConstraint struct {
	Order []string `json:"order"`
}

// TankMixConstraint 是混用禁忌结论：Selected 为所选药剂，Forbidden 为分组表中不得与之同桶的药剂（通用名），
// TankLimit 表示知识库有单桶产品数上限（数值不进模型入参：模型原文不得写裸数字）。
type TankMixConstraint struct {
	Selected  string   `json:"selected,omitempty"`
	Forbidden []string `json:"forbidden,omitempty"`
	TankLimit bool     `json:"tank_limit,omitempty"`
}

// WindowConstraint 是作业窗口结论：Open 为窗口是否打开，Days 为窗口内可安排作业的日期词（今天 / 明天 …），
// 窗口关闭时为空（不得安排作业）。
type WindowConstraint struct {
	Open bool     `json:"open"`
	Days []string `json:"days,omitempty"`
}
