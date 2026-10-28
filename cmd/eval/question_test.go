package main

import (
	"context"
	"strings"
	"testing"

	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/sample"
)

// 解析成功路径需要 trial schema（FieldIDByCode 查库），由 run 集成链路覆盖；此处只测失败路径。
func TestSeedQuestion_UnknownUser(t *testing.T) {
	s := &sample.Sample{ID: "x", Context: sample.Context{Farm: "farm-demo-01", Field: "field-001", Role: "farm_manager", User: "u-nobody"}}
	_, err := seedQuestion(1, 1)(context.Background(), &app.App{}, s)
	if err == nil || !strings.Contains(err.Error(), `unknown context.user "u-nobody"`) {
		t.Fatalf("err = %v", err)
	}
}
