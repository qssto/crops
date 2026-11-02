package constraint

// registeredPrecondition：法规前置条件「药剂在本作物 × 防治对象 × 区域有登记」。无工具来源（登记表随知识快照），
// 不要求 tool_call_id；threshold.kb 为登记表引用 common/pesticide_registry@vN。未选药或未登记即不通过。
type registeredPrecondition struct{}

func (registeredPrecondition) RuleID() string     { return RuleRegistered }
func (registeredPrecondition) Precondition() bool { return true }

func (registeredPrecondition) Check(in *Input) Result {
	obs := Observed{Source: "pesticide_registry"}
	th := Threshold{ThresholdID: "registered", Op: "==", Value: 1}
	p := in.Product
	if p == nil {
		return fail(th, obs, "no product selected")
	}
	th.KB = p.Ref
	obs.Value = p.Registered
	obs.Note = p.CommonName
	if th.KB == "" {
		return fail(th, obs, "registry ref unknown")
	}
	if !p.Registered {
		return fail(th, obs, p.CommonName+" not registered for crop × target × region")
	}
	return Result{Passed: true, Observed: obs, Threshold: th}
}
