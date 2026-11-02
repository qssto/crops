package graph

import (
	"context"
	"testing"

	"agri-steward/internal/agent/knowledge"
)

// TestEnvMaxPHI（复核 19）：maxPHI 区分「查不到这个药」与「已登记但本作物 PHI 不适用」：
// 前者 known=false（phi_interval 判失败），后者 known=true、days=nil（不计入最早收获日）。
func TestEnvMaxPHI(t *testing.T) {
	snap, err := knowledge.NewStore(knowledge.NewRepoReader("../../..")).Open(context.Background(), knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{snap: snap, crop: "rice"}
	for _, tc := range []struct {
		name  string
		known bool
		days  int // -1 表示 days=nil
	}{
		{"24%噻呋酰胺悬浮剂", true, 14},
		{"五氟磺草胺", true, -1},
		{"10%氰氟草酯乳油", true, -1},
		{"井冈霉素", true, 14},
		{"吡虫啉", false, -1},  // 登记表无吡虫啉，KB 无 PHI 来源：失败关闭
		{"啶酰菌胺", false, -1}, // 只有黄瓜登记，无本作物登记
		{"", false, -1},
	} {
		days, known := e.maxPHI(tc.name)
		got := -1
		if days != nil {
			got = *days
		}
		if known != tc.known || got != tc.days {
			t.Errorf("maxPHI(%q) = (%d, %v), want (%d, %v)", tc.name, got, known, tc.days, tc.known)
		}
	}
	if days, known := (&env{crop: "rice"}).maxPHI("五氟磺草胺"); known || days != nil {
		t.Errorf("maxPHI without snapshot = (%v, %v), want unknown", days, known)
	}
}
