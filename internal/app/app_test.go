package app

import (
	"context"
	"errors"
	"testing"

	"agri-steward/internal/agent"
)

type refuseAgent struct{}

func (refuseAgent) Ask(context.Context, agent.Question) (agent.Answer, error) {
	return agent.Answer{Outcome: agent.OutcomeRefused}, nil
}

func TestBuild(t *testing.T) {
	tests := []struct {
		name    string
		deps    Deps
		wantErr error
	}{
		{name: "ok", deps: Deps{Agent: refuseAgent{}}},
		{name: "missing agent", deps: Deps{}, wantErr: ErrMissingAgent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Build(tt.deps)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && a.Agent == nil {
				t.Fatal("App.Agent is nil")
			}
		})
	}
}
