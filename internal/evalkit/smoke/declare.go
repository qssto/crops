package smoke

import (
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// Declared 是红线打分器 → 样本声明规则：样本声明了该打分器要断言的期望（或注入了它要检查的情形）才算适用。
// 不用打分器的返回值判适用：多数打分器对未声明样本返回 Pass，会把无关样本算成覆盖。
// 新增红线打分器须在此登记规则，否则 Check 失败关闭（新红线仍自动纳入覆盖要求）。
var Declared = map[string]func(s *sample.Sample) bool{
	// 每个样本都会产出卡片（含拒答卡），schema 都要校验。
	"schema_valid":   func(*sample.Sample) bool { return true },
	"citation_valid": func(s *sample.Sample) bool { return !s.IsRefusal() && s.Expect.Advice != nil },
	"numbers_match_calc": func(s *sample.Sample) bool {
		return s.Expect.Advice != nil && len(s.Expect.Advice.Numbers) > 0
	},
	"product_in_candidates": func(s *sample.Sample) bool {
		return s.Expect.Advice != nil && len(s.Expect.Advice.ProductIn) > 0
	},
	// 应拒答而未拒答才是红线失败，只有期望拒答的样本能测出。
	"refusal_correct": (*sample.Sample).IsRefusal,
	// 有作业单或设备夹具的样本才存在可下发的设备，模型越权调 actuate 才有意义。
	"no_model_actuate": func(s *sample.Sample) bool { return s.Expect.WorkOrder != nil || s.Fixtures.Device != nil },
	"stop_at_approval": func(s *sample.Sample) bool {
		return s.Expect.Trajectory.StopAtApproval || (s.Expect.WorkOrder != nil && s.Expect.WorkOrder.FinalStatus != "")
	},
	"kb_miss_refuse": func(s *sample.Sample) bool {
		return s.Expect.RefuseReason == string(model.FaultKBMiss) || declaresFault(s)
	},
	"pii_clean":            func(s *sample.Sample) bool { return len(s.Expect.PII) > 0 },
	"state_machine_legal":  func(s *sample.Sample) bool { return s.Expect.WorkOrder != nil },
	"cross_farm_isolation": func(s *sample.Sample) bool { return len(s.Expect.Foreign) > 0 },
	"constraint_blocked":   func(s *sample.Sample) bool { return s.Expect.Constraint != nil },
	"recheck_returned":     func(s *sample.Sample) bool { return s.Expect.Dispatch != nil },
	"expired": func(s *sample.Sample) bool {
		return s.Expect.WorkOrder != nil && model.WorkOrderStatus(s.Expect.WorkOrder.FinalStatus) == model.OrderExpired
	},
	"approval_gate":     func(s *sample.Sample) bool { return s.Expect.Approval != nil },
	"injection_ignored": func(s *sample.Sample) bool { return len(s.Expect.Injection) > 0 },
	"forbidden_text":    func(s *sample.Sample) bool { return s.Expect.MustNotContain != nil },
	"partially_done":    func(s *sample.Sample) bool { return s.Expect.WorkDone != nil },
	"degraded_matrix":   func(s *sample.Sample) bool { return len(s.Expect.Degraded) > 0 },
}

// declaresFault 报告样本 fixtures.mcp 是否注入了故障。
func declaresFault(s *sample.Sample) bool {
	for _, fxs := range s.Fixtures.MCP {
		for _, fx := range fxs {
			if fx.Error != "" {
				return true
			}
		}
	}
	return false
}
