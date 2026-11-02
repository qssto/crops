package seed

import (
	"testing"
)

// flags: 段按注册表 / 作用域 / 形态校验，含应失败用例。
func TestLoadFlags(t *testing.T) {
	ok := func(key string, tn, fm uint64, v string) flagEntry {
		return flagEntry{FlagKey: key, TenantID: tn, FarmID: fm, Value: v, Comment: "种子"}
	}
	tests := []struct {
		name    string
		in      []flagEntry
		wantN   int
		wantErr bool
	}{
		{name: "none", in: nil},
		{name: "farm on", in: []flagEntry{ok("enable_advice", 1, 1, `{"on":true}`)}, wantN: 1},
		{name: "global force_off", in: []flagEntry{ok("enable_robot_dispatch", 0, 0, `{"force_off":true}`)}, wantN: 1},
		{name: "robot farm off", in: []flagEntry{ok("enable_robot_dispatch", 1, 1, `{"on":false}`)}, wantN: 1},
		{name: "robot farm on needs admin api", in: []flagEntry{ok("enable_robot_dispatch", 1, 1, `{"on":true}`)}, wantErr: true},
		{name: "unknown key", in: []flagEntry{ok("enable_magic", 1, 1, `{"on":true}`)}, wantErr: true},
		{name: "farm without tenant", in: []flagEntry{ok("enable_advice", 0, 1, `{"on":true}`)}, wantErr: true},
		{name: "tenant rejects on", in: []flagEntry{ok("enable_advice", 1, 0, `{"on":true}`)}, wantErr: true},
		{name: "bad json", in: []flagEntry{ok("enable_advice", 1, 1, `{"on":`)}, wantErr: true},
		{name: "empty comment", in: []flagEntry{{FlagKey: "enable_advice", TenantID: 1, FarmID: 1, Value: `{"on":true}`}},
			wantErr: true},
		{name: "duplicate", in: []flagEntry{ok("enable_advice", 1, 1, `{"on":true}`), ok("enable_advice", 1, 1, `{"on":false}`)},
			wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadFlags(tt.in)
			if (err != nil) != tt.wantErr || len(got) != tt.wantN {
				t.Fatalf("loadFlags = %d rows, %v; want %d err=%t", len(got), err, tt.wantN, tt.wantErr)
			}
		})
	}
}

// 仓库种子：示范农场 1 的 FARM 行 enable_advice={"on":true}，不写 enable_robot_dispatch。
func TestLoad_RepoFlags(t *testing.T) {
	d, err := Load("../../configs/seed/fields.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Flags) != 1 {
		t.Fatalf("flags = %+v", d.Flags)
	}
	f := d.Flags[0]
	if f.Key != "enable_advice" || f.TenantID != 1 || f.FarmID != 1 || f.ScopeType != "FARM" || string(f.Value) != `{"on":true}` {
		t.Fatalf("flag = %+v", f)
	}
}
