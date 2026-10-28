package evalkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"agri-steward/internal/agent"
)

// RefuseReasonStub 是空实现桩的拒答原因。
const RefuseReasonStub = "NOT_IMPLEMENTED"

// RefuseAllAgent 是 L0 空实现桩：对任何问题都返回 REFUSED，用于在主链路就绪前验证回放与报告链路。
// L4 起由 cmd/eval 改为注入真实主链路，evalkit 代码不变。
type RefuseAllAgent struct{}

// Ask 实现 agent.Agent。
func (RefuseAllAgent) Ask(context.Context, agent.Question) (agent.Answer, error) {
	return agent.Answer{Outcome: agent.OutcomeRefused, RefuseReason: RefuseReasonStub}, nil
}

const (
	expIDTimeLayout = "20060102T150405"
	expIDRandBytes  = 3
)

// NewExperimentID 生成 exp_id：UTC 时间戳 + 6 位十六进制随机串，如 20261028T031500-a1b2c3。
func NewExperimentID(now time.Time) (string, error) {
	buf := make([]byte, expIDRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate experiment id: %w", err)
	}
	return now.UTC().Format(expIDTimeLayout) + "-" + hex.EncodeToString(buf), nil
}
