package policy

import (
	"encoding/json"
	"errors"
	"testing"
)

// needField 是只要求 field_id 为 json.Number 的假校验器。
type needField struct{}

func (needField) Validate(v any) error {
	m, _ := v.(map[string]any)
	if _, ok := m["field_id"].(json.Number); !ok {
		return errors.New("field_id must be a number")
	}
	return nil
}

func TestNew_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		specs []Spec
	}{
		{"empty name", []Spec{{Tool: "", Level: LevelRead, Args: needField{}}}},
		{"no validator", []Spec{{Tool: "a.b", Level: LevelRead}}},
		{"duplicate", []Spec{{Tool: "a.b", Level: LevelRead, Args: needField{}}, {Tool: "a.b", Level: LevelRead, Args: needField{}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.specs, LevelRead); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestBefore(t *testing.T) {
	p, err := New([]Spec{
		{Tool: "w.read", Level: LevelRead, Args: needField{}},
		{Tool: "w.write", Level: LevelWrite, Args: needField{}},
		{Tool: "w.nolevel", Args: needField{}},
	}, LevelRead)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, tool, args string
		want             error
	}{
		{"ok", "w.read", `{"field_id":1}`, nil},
		{"unknown tool", "w.nope", `{"field_id":1}`, ErrUnknownTool},
		{"level undeclared", "w.nolevel", `{"field_id":1}`, ErrLevelUndeclared},
		{"level denied", "w.write", `{"field_id":1}`, ErrLevelDenied},
		{"schema mismatch", "w.read", `{"field_id":"x"}`, ErrBadArgs},
		{"not object", "w.read", `[1]`, ErrBadArgs},
		{"null", "w.read", `null`, ErrBadArgs},
		{"trailing", "w.read", `{"field_id":1} {}`, ErrBadArgs},
		{"empty args treated as {}", "w.read", ``, ErrBadArgs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := p.Before(tt.tool, json.RawMessage(tt.args))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if tt.want == nil && s.Tool != tt.tool {
				t.Fatalf("spec = %+v", s)
			}
		})
	}
}

func TestBefore_AllowedWrite(t *testing.T) {
	p, err := New([]Spec{{Tool: "w.write", Level: LevelWrite, Args: needField{}}}, LevelRead, LevelWrite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Before("w.write", json.RawMessage(`{"field_id":2}`)); err != nil {
		t.Fatal(err)
	}
}

func TestAfter(t *testing.T) {
	p, err := New(nil, LevelRead)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		in, want   Outcome
		wantReason bool
	}{
		{"ok with source", Outcome{Status: "OK", Source: "mock"}, Outcome{Status: "OK", Source: "mock"}, false},
		{"ok without source", Outcome{Status: "OK"}, Outcome{Status: "ERROR", ErrorCode: "MALFORMED"}, true},
		{"failed with code", Outcome{Status: "EMPTY", ErrorCode: "EMPTY"}, Outcome{Status: "EMPTY", ErrorCode: "EMPTY"}, false},
		{"failed without code", Outcome{Status: "ERROR"}, Outcome{Status: "ERROR", ErrorCode: "UPSTREAM"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := p.After("w.read", tt.in)
			if got != tt.want || (reason != "") != tt.wantReason {
				t.Fatalf("got %+v reason %q, want %+v", got, reason, tt.want)
			}
		})
	}
}
