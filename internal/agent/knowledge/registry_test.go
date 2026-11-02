package knowledge

import (
	"context"
	"slices"
	"testing"
)

// TestNormalizeProductName（P1-L3 审查 1；复核 25：剂型表取自登记表 formulations）：去开头含量与结尾剂型。
func TestNormalizeProductName(t *testing.T) {
	cases := []struct{ in, formulation, want string }{
		{"24%噻呋酰胺悬浮剂", "", "噻呋酰胺"},
		{"24％ 噻呋酰胺 悬浮剂", "", "噻呋酰胺"},
		{"200 g/L 氯虫苯甲酰胺悬浮剂", "", "氯虫苯甲酰胺"},
		{"200克/升氯虫苯甲酰胺", "", "氯虫苯甲酰胺"},
		{"25%五氟磺草胺可分散油悬浮剂", "", "五氟磺草胺"},
		{"75%三环唑可湿性粉剂", "可湿性粉剂", "三环唑"},
		{"吡蚜酮", "", "吡蚜酮"},
		{"悬浮剂", "", "悬浮剂"}, // 只有剂型时不剥成空串
	}
	reg := repoSnapshot(t).Registry()
	for _, tc := range cases {
		if got := reg.NormalizeProductName(tc.in, tc.formulation); got != tc.want {
			t.Errorf("NormalizeProductName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRegistry_Resolve（P1-L3 审查 1）：id / 通用名 / 商品名别名 / 含量剂型写法都能查到登记表产品；查不到返回空。
func TestRegistry_Resolve(t *testing.T) {
	reg := repoSnapshot(t).Registry()
	cases := map[string]string{
		"thifluzamide-240sc": "thifluzamide-240sc",
		"噻呋酰胺":               "thifluzamide-240sc",
		"24%噻呋酰胺悬浮剂":         "thifluzamide-240sc",
		"满穗":                 "thifluzamide-240sc",
		"康宽":                 "chlorantraniliprole-200sc",
		"200g/L氯虫苯甲酰胺悬浮剂":    "chlorantraniliprole-200sc",
		"75%肟菌·戊唑醇水分散粒剂":     "trifloxystrobin-tebuconazole-75wg",
		"50%吡蚜酮可湿性粉剂":        "pymetrozine-50wg", // 剂型与登记表不同，仍按通用名归一
	}
	for name, want := range cases {
		got := reg.Resolve(name)
		if len(got) != 1 || got[0].ID != want {
			ids := make([]string, len(got))
			for i, p := range got {
				ids[i] = p.ID
			}
			t.Errorf("Resolve(%q) = %v, want [%s]", name, ids, want)
		}
	}
	for _, name := range []string{"", "未知药", "噻呋", "悬浮剂"} {
		if got := reg.Resolve(name); len(got) != 0 {
			t.Errorf("Resolve(%q) = %d products, want none", name, len(got))
		}
	}
}

// TestRegistry_FormulationsFromKB（复核 25）：剂型表取自登记表 formulations，加载后按字数从长到短排序；
// 表里没有的剂型不剥。
func TestRegistry_FormulationsFromKB(t *testing.T) {
	snap, err := NewStore(fsWith(validFiles())).Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	reg := snap.Registry()
	if want := []string{"可分散油悬浮剂", "水分散粒剂", "悬浮剂"}; !slices.Equal(reg.Formulations, want) {
		t.Fatalf("formulations = %v, want %v", reg.Formulations, want)
	}
	for in, want := range map[string]string{"25%五氟磺草胺可分散油悬浮剂": "五氟磺草胺", "10%某药悬浮剂": "某药", "40%稻瘟灵乳油": "稻瘟灵乳油"} {
		if got := reg.NormalizeProductName(in, ""); got != want {
			t.Errorf("NormalizeProductName(%q) = %q, want %q", in, got, want)
		}
	}
	if len(repoSnapshot(t).Registry().Formulations) == 0 {
		t.Fatal("repo registry formulations empty")
	}
}

// TestRegistry_PHIOnlyProduct（复核 7 遗留）：candidate:false 产品可被 Resolve 识别（PHI 来源），IsCandidate 为 false。
func TestRegistry_PHIOnlyProduct(t *testing.T) {
	files := validFiles()
	files[RegistryFile] = regMinimal + regPHIOnly
	snap, err := NewStore(fsWith(files)).Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	got := snap.Registry().Resolve("10%测试药悬浮剂")
	if len(got) != 1 || got[0].ID != "phi-only-test" || got[0].IsCandidate() {
		t.Fatalf("Resolve = %+v", got)
	}
	if p, ok := snap.Registry().Product("pymetrozine-50wg"); !ok || !p.IsCandidate() {
		t.Fatal("default product must be a candidate")
	}
}
