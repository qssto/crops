package llm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestStubClassify(t *testing.T) {
	tests := []struct {
		q        string
		inScope  bool
		unknown  bool
		category string
		target   string
		intent   string
	}{
		{q: "3号田的稻飞虱要不要打药？", inScope: true, category: "protect", target: "planthopper", intent: IntentTiming},
		{q: "隔壁小麦田赤霉病用什么药？", inScope: false},
		{q: "吡蚜酮现在多少钱一包？", inScope: true, unknown: true, category: "protect", intent: IntentDosage},
		{q: "6号田叶片上有梭形褐色病斑，中间灰白，是什么病？", inScope: true, category: "protect", target: "blast", intent: IntentIdentify},
		{q: "2号田要施穗肥，尿素每亩用多少、整块田一共要多少？", inScope: true, category: "fertilize", intent: IntentRate},
		{q: "4号田稻叶从下往上发黄，是不是缺肥了？", inScope: true, category: "fertilize", intent: IntentIdentify},
		{q: "6号田复水要灌到3厘米，大概要放多少方水？", inScope: true, category: "irrigate", intent: IntentAmount},
		{q: "5号田今天刚打完二化螟的药，田里水怎么管？能排水吗？", inScope: true, category: "irrigate", intent: IntentAfterSpray},
		{q: "5号田别人家都开始晒田了，我这块也晒吗？", inScope: true, category: "irrigate", intent: IntentDrain},
		{q: "4号田在灌浆，接下来几天都35度以上，要怎么管？", inScope: true, category: "disaster", intent: IntentRespond},
		{q: "昨天下冰雹，4号田叶子打烂了不少，要怎么补救？", inScope: true, unknown: true, category: "disaster", intent: IntentRespond},
		{q: "我亲戚在黑龙江种的稻子遭了霜冻", inScope: false},
	}
	for _, tt := range tests {
		t.Run(tt.q, func(t *testing.T) {
			got := StubClassify(tt.q)
			if got.InScope != tt.inScope || got.Unknown != tt.unknown {
				t.Fatalf("scope = %+v", got)
			}
			if tt.inScope && (got.Category != tt.category || got.Target != tt.target || got.Intent != tt.intent) {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestStubSlots(t *testing.T) {
	s := StubClassify("4号田插秧6天了，分蘖肥每亩一共要多少尿素？分两次怎么分？").Slots
	if s.FertStage != "tillering" || s.FertProduct != "urea" || !s.Split {
		t.Fatalf("slots = %+v", s)
	}
	s = StubClassify("1号田现在水层1厘米，要补到3厘米，大概要灌多少方水？").Slots
	if s.TargetDepthCm == nil || *s.TargetDepthCm != 3 {
		t.Fatalf("depth = %+v", s.TargetDepthCm)
	}
	s = StubClassify("安排无人机明天给3号田打稻飞虱的药").Slots
	if s.Sprayer != SprayerUAV || !s.Dispatch {
		t.Fatalf("slots = %+v", s)
	}
}

func TestStub_Generate(t *testing.T) {
	ask := func(task Task, input any, out any) {
		t.Helper()
		body, err := json.Marshal(taskMessage{Task: task, Input: input})
		if err != nil {
			t.Fatal(err)
		}
		m, err := Stub{}.Generate(context.Background(), []*schema.Message{schema.SystemMessage("s"), schema.UserMessage(string(body))})
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(m.Content), out); err != nil {
			t.Fatal(err)
		}
	}
	var ch ChooseOutput
	ask(TaskChoose, ChooseInput{Question: "用吡蚜酮打", Candidates: []Candidate{{ID: "a", CommonName: "噻虫嗪"}, {ID: "b", CommonName: "吡蚜酮"}}}, &ch)
	if ch.ProductID != "b" {
		t.Fatalf("choose = %+v", ch)
	}
	ask(TaskChoose, ChooseInput{Question: "随便", Candidates: []Candidate{{ID: "a"}, {ID: "b"}}}, &ch)
	if ch.ProductID != "a" {
		t.Fatalf("choose default = %+v", ch)
	}
	var ex ExplainOutput
	ask(TaskExplain, ExplainInput{Decision: "建议施药", Numbers: []NumberSlot{{Placeholder: "{{n0}}", Label: "亩用量"}}}, &ex)
	if ex.Conclusion != "建议施药；亩用量 {{n0}}（草拟，待专家确认）" {
		t.Fatalf("explain = %q", ex.Conclusion)
	}
	if _, err := (Stub{}).Generate(context.Background(), nil); err == nil {
		t.Fatal("want error without user message")
	}
}
