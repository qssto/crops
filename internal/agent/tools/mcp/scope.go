package mcp

import (
	"context"

	"agri-steward/internal/model"
)

type scopeKey struct{}

// WithScope 把调用方 Scope 挂到 ctx：gateway 调 Server 前统一注入，Server 据此按农场取数（P1 L1-B）。
func WithScope(ctx context.Context, scope model.Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ScopeFrom 取 WithScope 注入的 Scope；未注入返回零值（仓储实现按零值拒绝）。
func ScopeFrom(ctx context.Context) model.Scope {
	s, _ := ctx.Value(scopeKey{}).(model.Scope)
	return s
}
