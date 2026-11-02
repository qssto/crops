package app

import (
	"fmt"
	"os"

	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/service"
)

// LoadRiskMatrix 读审批风险矩阵（policy/risk_matrix.yaml）并按内置约束注册表交叉校验 near_threshold 的 rule_id
// （拼错的 rule_id 永远不会命中临界、定级静默偏低）；不合法即报错（服务不启动）。
func LoadRiskMatrix(path string) (*policy.RiskMatrix, error) {
	m, err := policy.LoadRiskMatrix(path)
	if err != nil {
		return nil, err
	}
	if err := m.CheckRuleIDs(constraint.Default().Has); err != nil {
		return nil, fmt.Errorf("app: %s: %w", path, err)
	}
	return m, nil
}

// LoadConstraintReviewer 读法规对照表（policy/regulation_map.yaml）并按内置约束注册表启动期校验 rule_id，
// 返回开票前拦截判定（Deps.Constraints）；文件缺失或任一条目不合法即报错（服务不启动）。
func LoadConstraintReviewer(path string) (*constraint.Reviewer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("app: read regulation map: %w", err)
	}
	reg := constraint.Default()
	regs, err := constraint.ParseRegulationMap(b, reg)
	if err != nil {
		return nil, fmt.Errorf("app: %s: %w", path, err)
	}
	return constraint.NewReviewer(reg, regs), nil
}

// registryer 是能给出约束注册表的开票前拦截判定（*constraint.Reviewer）。
type registryer interface{ Registry() *constraint.Registry }

// constraintRegistry 取开票前拦截判定所用的约束注册表，供主链路生成校验行（同一实例）；
// 不提供注册表的实现（测试替身）返回 nil，graph.New 据此报缺依赖（不静默换用另一份注册表）。
func constraintRegistry(r service.ConstraintReviewer) *constraint.Registry {
	if g, ok := r.(registryer); ok {
		return g.Registry()
	}
	return nil
}
