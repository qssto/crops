package evalkit

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/sample"
)

// 评审 P2：快照记录本次 run 的 user_sim 样本 ID（排序）与模拟用户 model / model_tier，gate 优先按它排除；
// 没有 user_sim 样本时为 nil，不序列化（既有实验快照逐字节不变）。
func TestUserSimOf(t *testing.T) {
	if got := UserSimOf([]*sample.Sample{mkSample("plain", "regression")}, "replay", "t1"); got != nil {
		t.Fatalf("no user_sim samples: %+v", got)
	}
	got := UserSimOf([]*sample.Sample{simSample("sim-b", 3), mkSample("plain", "regression"), simSample("sim-a", 3)}, "replay", "t1")
	if got == nil || !slices.Equal(got.SampleIDs, []string{"sim-a", "sim-b"}) || got.Model != "replay" || got.ModelTier != "t1" {
		t.Fatalf("user_sim snapshot = %+v", got)
	}
	raw, err := json.Marshal(Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "user_sim") {
		t.Fatalf("nil user_sim serialized: %s", raw)
	}
	raw, err = json.Marshal(Snapshot{UserSim: got})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"user_sim":{"sample_ids":["sim-a","sim-b"],"model":"replay","model_tier":"t1"}`) {
		t.Fatalf("user_sim json = %s", raw)
	}
}
