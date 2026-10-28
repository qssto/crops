package kit

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/agent/tools/mcp"
)

func okHandler(context.Context, json.RawMessage) (mcp.Result, error) { return mcp.Result{}, nil }

func TestNewServer(t *testing.T) {
	spec := func(n string) mcp.ToolSpec { return mcp.ToolSpec{Name: n} }
	tests := []struct {
		name     string
		specs    []mcp.ToolSpec
		handlers map[string]Handler
		wantErr  bool
	}{
		{"ok", []mcp.ToolSpec{spec("w.a")}, map[string]Handler{"w.a": okHandler}, false},
		{"bad prefix", []mcp.ToolSpec{spec("x.a")}, map[string]Handler{"x.a": okHandler}, true},
		{"duplicate", []mcp.ToolSpec{spec("w.a"), spec("w.a")}, map[string]Handler{"w.a": okHandler}, true},
		{"no handler", []mcp.ToolSpec{spec("w.a")}, map[string]Handler{}, true},
		{"no spec", nil, map[string]Handler{"w.a": okHandler}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewServer("w", tt.specs, tt.handlers)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestServer_InvokeCanceled(t *testing.T) {
	s, err := NewServer("w", []mcp.ToolSpec{{Name: "w.a"}}, map[string]Handler{"w.a": okHandler})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Invoke(ctx, "w.a", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestDecodeArgs(t *testing.T) {
	tests := []struct {
		args    string
		wantErr bool
	}{
		{"", false}, {`{}`, false}, {`{"field_id":3}`, false},
		{`{"field_id":3,"x":1}`, true}, {`{"field_id":"3"}`, true}, {`{"field_id":3}[]`, true}, {`[`, true},
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			var a FieldArgs
			err := DecodeArgs(json.RawMessage(tt.args), &a)
			var te *mcp.ToolError
			if tt.wantErr != (err != nil) || (err != nil && (!errors.As(err, &te) || te.Code != mcp.ErrRejected)) {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewResult_Freshness(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		at   time.Time
		want int
	}{
		{now.Add(-90 * time.Second), 90},
		{now, 0},
		{now.Add(time.Hour), 0}, // 未来时刻按 0 计
	}
	for _, tt := range tests {
		r, err := NewResult(map[string]int{"a": 1}, "mock", tt.at, now)
		if err != nil {
			t.Fatal(err)
		}
		if r.DataFreshnessSec != tt.want || r.Source != "mock" || string(r.Data) != `{"a":1}` {
			t.Errorf("result = %+v, want freshness %d", r, tt.want)
		}
	}
	if _, err := NewResult(make(chan int), "mock", now, now); err == nil {
		t.Error("want marshal error")
	}
}

func TestResolveField(t *testing.T) {
	dir := StaticFields{1: "field-001"}
	tests := []struct {
		id   uint64
		want mcp.ErrorCode
	}{{0, mcp.ErrRejected}, {2, mcp.ErrEmpty}}
	for _, tt := range tests {
		_, err := ResolveField(context.Background(), dir, FieldArgs{FieldID: tt.id})
		var te *mcp.ToolError
		if !errors.As(err, &te) || te.Code != tt.want {
			t.Errorf("id %d: err = %v, want %s", tt.id, err, tt.want)
		}
	}
	if code, err := ResolveField(context.Background(), dir, FieldArgs{FieldID: 1}); err != nil || code != "field-001" {
		t.Errorf("code = %q, err = %v", code, err)
	}
}

func TestStageEnumMatchesStages(t *testing.T) {
	var enum []string
	if err := json.Unmarshal([]byte(StageEnum), &enum); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(enum, Stages) {
		t.Fatalf("StageEnum %v != Stages %v", enum, Stages)
	}
}

func TestRealServer_NotConfigured(t *testing.T) {
	s := NewRealServer("w", []mcp.ToolSpec{{Name: "w.a"}}, RealConfig{Timeout: time.Second})
	_, err := s.Invoke(context.Background(), "w.a", nil)
	var te *mcp.ToolError
	if !errors.As(err, &te) || te.Code != mcp.ErrUpstream || te.Msg != ErrRealNotConfigured {
		t.Fatalf("err = %v, want UPSTREAM real not configured", err)
	}
	if s.Name() != "w" || len(s.Tools()) != 1 {
		t.Fatalf("name/tools mismatch")
	}
}
