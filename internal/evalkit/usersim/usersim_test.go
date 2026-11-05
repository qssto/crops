package usersim

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/sample"
)

func simSample(maxTurns int) *sample.Sample {
	return &sample.Sample{ID: "cap-sim-1", Suite: "capability", Question: "明天能打药吗？",
		UserSim: &sample.UserSim{Persona: sample.PersonaFarmer, Instruction: "问清楚风速再决定。", MaxTurns: maxTurns}}
}

// fakeClient 按调用顺序返回 replies，并记下请求。
type fakeClient struct {
	replies []string
	err     error
	reqs    []Request
}

func (f *fakeClient) Reply(_ context.Context, r Request) (string, error) {
	f.reqs = append(f.reqs, r)
	if f.err != nil {
		return "", f.err
	}
	out := f.replies[0]
	f.replies = f.replies[1:]
	return out, nil
}

// 脚本用户只说样本 question 一句：任何续问都结束对话（与 P2 L7-b 之前单轮回放一致）。
func TestScripted_NoFollowUp(t *testing.T) {
	text, ok, err := Scripted{}.Next(t.Context(), simSample(3), []Turn{{User: "明天能打药吗？", Agent: "可以"}})
	if err != nil || ok || text != "" {
		t.Fatalf("Scripted.Next = %q %v %v", text, ok, err)
	}
}

// 模拟用户：按 (sample_id, turn, 对话哈希) 请求续问；回复含 ###STOP### 即结束；达到 max_turns 不再请求；
// 空回复与客户端错误都是错误（runner 记 infra_error，不编造回复）；样本无 user_sim 块是错误。
func TestSimulated_Next(t *testing.T) {
	h1 := []Turn{{User: "明天能打药吗？", Agent: "需要风速数据"}}
	c := &fakeClient{replies: []string{"  风速 2 米每秒。 "}}
	text, ok, err := NewSimulated(c).Next(t.Context(), simSample(3), h1)
	if err != nil || !ok || text != "风速 2 米每秒。" {
		t.Fatalf("Next = %q %v %v", text, ok, err)
	}
	r := c.reqs[0]
	if r.SampleID != "cap-sim-1" || r.Turn != 1 || r.Persona != sample.PersonaFarmer || r.Instruction != "问清楚风速再决定。" ||
		r.ConvHash != ConvHash(simSample(3).UserSim, h1) || len(r.History) != 1 {
		t.Fatalf("request = %+v", r)
	}

	stop := &fakeClient{replies: []string{"好的，谢谢。" + StopToken}}
	if text, ok, err := NewSimulated(stop).Next(t.Context(), simSample(3), h1); err != nil || ok || text != "" {
		t.Fatalf("stop: %q %v %v", text, ok, err)
	}

	full := &fakeClient{}
	h3 := append(append([]Turn{}, h1...), Turn{User: "a", Agent: "b"}, Turn{User: "c", Agent: "d"})
	if _, ok, err := NewSimulated(full).Next(t.Context(), simSample(3), h3); err != nil || ok || len(full.reqs) != 0 {
		t.Fatalf("max_turns: ok=%v err=%v reqs=%d", ok, err, len(full.reqs))
	}

	if _, _, err := NewSimulated(&fakeClient{replies: []string{"   "}}).Next(t.Context(), simSample(3), h1); !errors.Is(err, ErrEmptyReply) {
		t.Fatalf("empty reply err = %v", err)
	}
	boom := errors.New("boom")
	if _, _, err := NewSimulated(&fakeClient{err: boom}).Next(t.Context(), simSample(3), h1); !errors.Is(err, boom) {
		t.Fatalf("client err = %v", err)
	}
	plain := simSample(3)
	plain.UserSim = nil
	if _, _, err := NewSimulated(c).Next(t.Context(), plain, h1); !errors.Is(err, ErrNotUserSim) {
		t.Fatalf("no user_sim err = %v", err)
	}
	if _, _, err := NewSimulated(c).Next(t.Context(), simSample(3), nil); err == nil {
		t.Fatal("empty history accepted (the opening question must be answered first)")
	}
}

// 对话哈希覆盖人设、隐藏指令与全部历史：任何一项变化都换键（录制失效而不是错配）。
func TestConvHash(t *testing.T) {
	u := simSample(3).UserSim
	h := []Turn{{User: "q", Agent: "a"}}
	base := ConvHash(u, h)
	if len(base) != 64 || base != ConvHash(u, []Turn{{User: "q", Agent: "a"}}) {
		t.Fatalf("hash %q not stable hex sha256", base)
	}
	other := *u
	other.Instruction = "别的指令"
	for name, got := range map[string]string{
		"agent":       ConvHash(u, []Turn{{User: "q", Agent: "b"}}),
		"user":        ConvHash(u, []Turn{{User: "q2", Agent: "a"}}),
		"more turns":  ConvHash(u, append(h, Turn{User: "x"})),
		"instruction": ConvHash(&other, h),
		"split":       ConvHash(u, []Turn{{User: "qa", Agent: ""}}),
	} {
		if got == base {
			t.Errorf("%s: hash unchanged", name)
		}
	}
}

func writeRec(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 录制回放：命中 (sample_id, turn, conv_hash) 返回录制；缺录制返回 ErrNoRecording（错误里带键，runner 记 infra_error）；
// 目录不存在视为无录制；文件名与 sample_id 不符、未知字段、哈希格式错、空回复、键重复都拒绝加载。
func TestReplay(t *testing.T) {
	missing, err := LoadReplay(filepath.Join(t.TempDir(), "none"))
	if err != nil {
		t.Fatal(err)
	}
	req := Request{SampleID: "cap-sim-1", Turn: 1, ConvHash: strings.Repeat("a", 64)}
	if _, err := missing.Reply(t.Context(), req); !errors.Is(err, ErrNoRecording) || !strings.Contains(err.Error(), "cap-sim-1") {
		t.Fatalf("missing dir err = %v", err)
	}

	dir := t.TempDir()
	writeRec(t, dir, "cap-sim-1.yaml", "sample_id: cap-sim-1\nrecordings:\n  - {turn: 1, conv_hash: "+strings.Repeat("a", 64)+", reply: 风速 2 米每秒}\n")
	rp, err := LoadReplay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rp.Reply(t.Context(), req); err != nil || got != "风速 2 米每秒" {
		t.Fatalf("hit = %q %v", got, err)
	}
	for name, r := range map[string]Request{
		"turn": {SampleID: "cap-sim-1", Turn: 2, ConvHash: req.ConvHash},
		"hash": {SampleID: "cap-sim-1", Turn: 1, ConvHash: strings.Repeat("b", 64)},
		"id":   {SampleID: "cap-sim-2", Turn: 1, ConvHash: req.ConvHash},
	} {
		if _, err := rp.Reply(t.Context(), r); !errors.Is(err, ErrNoRecording) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	bad := map[string]string{
		"id mismatch":  "sample_id: other\nrecordings: [{turn: 1, conv_hash: " + strings.Repeat("a", 64) + ", reply: x}]\n",
		"unknown":      "sample_id: cap-sim-1\nextra: 1\nrecordings: []\n",
		"short hash":   "sample_id: cap-sim-1\nrecordings: [{turn: 1, conv_hash: abc, reply: x}]\n",
		"turn 0":       "sample_id: cap-sim-1\nrecordings: [{turn: 0, conv_hash: " + strings.Repeat("a", 64) + ", reply: x}]\n",
		"empty reply":  "sample_id: cap-sim-1\nrecordings: [{turn: 1, conv_hash: " + strings.Repeat("a", 64) + ", reply: ' '}]\n",
		"duplicate":    "sample_id: cap-sim-1\nrecordings:\n  - {turn: 1, conv_hash: " + strings.Repeat("a", 64) + ", reply: x}\n  - {turn: 1, conv_hash: " + strings.Repeat("a", 64) + ", reply: y}\n",
		"upper hex":    "sample_id: cap-sim-1\nrecordings: [{turn: 1, conv_hash: " + strings.Repeat("A", 64) + ", reply: x}]\n",
		"not yaml map": "- 1\n",
	}
	for name, body := range bad {
		d := t.TempDir()
		writeRec(t, d, "cap-sim-1.yaml", body)
		if _, err := LoadReplay(d); !errors.Is(err, ErrBadRecording) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// 模拟用户模型独立配置 eval.usersim.model / model_tier：严格解析，model 现只许 replay（待真实模型），tier 必填。
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "usersim.yaml")
		writeRec(t, dir, "usersim.yaml", body)
		return p
	}
	c, err := LoadConfig(write("eval:\n  usersim:\n    model: replay\n    model_tier: replay-v0\n"))
	if err != nil || c.Model != ModelReplay || c.ModelTier != "replay-v0" {
		t.Fatalf("config = %+v %v", c, err)
	}
	for name, body := range map[string]string{
		"real model": "eval:\n  usersim:\n    model: gpt\n    model_tier: t\n",
		"no tier":    "eval:\n  usersim:\n    model: replay\n",
		"unknown":    "eval:\n  usersim:\n    model: replay\n    model_tier: t\n    temperature: 1\n",
	} {
		if _, err := LoadConfig(write(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := LoadConfig(filepath.Join(dir, "nope.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}

// 仓库内的配置文件可加载，录制目录（可为空）格式合法。
func TestRepoConfig(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	if _, err := LoadConfig(filepath.Join(root, DefaultConfig)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReplay(filepath.Join(root, DefaultRecordingsDir)); err != nil {
		t.Fatal(err)
	}
}
