// Package app 是依赖装配：cmd/server 与 evalkit 共用同一个 Build，
// 区别只在传入的依赖实现，保证评测跑的就是生产链路。
package app

import (
	"errors"

	"agri-steward/internal/agent"
)

// Deps 是装配所需的依赖。L0 只有 Agent；L1 起加入 Clock、ToolProvider、存储等注入接缝。
type Deps struct {
	Agent agent.Agent
}

// App 是装配结果。
type App struct {
	Agent agent.Agent
}

// ErrMissingAgent 表示未注入 Agent 实现。
var ErrMissingAgent = errors.New("app: deps.Agent is required")

// Build 校验依赖并装配 App。
func Build(deps Deps) (*App, error) {
	if deps.Agent == nil {
		return nil, ErrMissingAgent
	}
	return &App{Agent: deps.Agent}, nil
}
