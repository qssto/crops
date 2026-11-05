package calc

import (
	"slices"
	"testing"
)

// TestPestPriority：只排达标且未过晚的对象，超标倍数高者先，倍数相同按 id。
func TestPestPriority(t *testing.T) {
	tests := []struct {
		name  string
		loads []PestLoad
		want  []string
	}{
		{name: "empty", loads: nil, want: nil},
		{name: "unmet dropped", loads: []PestLoad{{Target: "planthopper", Exceed: 0.5}}, want: nil},
		{name: "by exceed", loads: []PestLoad{
			{Target: "planthopper", Met: true, Exceed: 1.2},
			{Target: "leaf_roller", Met: true, Exceed: 2.5},
			{Target: "sheath_blight", Met: true, Exceed: 1.5},
		}, want: []string{"leaf_roller", "sheath_blight", "planthopper"}},
		{name: "tie by id", loads: []PestLoad{
			{Target: "stem_borer", Met: true, Exceed: 2},
			{Target: "blast", Met: true, Exceed: 2},
		}, want: []string{"blast", "stem_borer"}},
		{name: "too late dropped", loads: []PestLoad{
			{Target: "false_smut", Met: true, TooLate: true, Exceed: 3},
			{Target: "planthopper", Met: true, Exceed: 1.1},
		}, want: []string{"planthopper"}},
		{name: "duplicate target keeps max", loads: []PestLoad{
			{Target: "planthopper", Met: true, Exceed: 1.1},
			{Target: "leaf_roller", Met: true, Exceed: 1.5},
			{Target: "planthopper", Met: true, Exceed: 2.0},
		}, want: []string{"planthopper", "leaf_roller"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PestPriority(tt.loads); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTargetNames：主名加括号内别名，去空去重；无括号时只有主名。
func TestTargetNames(t *testing.T) {
	for label, want := range map[string][]string{
		"稻飞虱（褐飞虱、白背飞虱）": {"稻飞虱", "褐飞虱", "白背飞虱"},
		"稻纵卷叶螟":         {"稻纵卷叶螟"},
		"灰霉病（黄瓜）":       {"灰霉病", "黄瓜"},
		"":              nil,
	} {
		if got := TargetNames(label); !slices.Equal(got, want) {
			t.Errorf("TargetNames(%q) = %v, want %v", label, got, want)
		}
	}
}
