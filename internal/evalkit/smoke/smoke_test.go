package smoke

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
)

// fakeGrader 只对 applies 中的样本适用：适用规则登记进 Declared（测试结束删除）；Grade 总是通过，
// 证明 Check 不看打分器返回值。applies 为 nil 时不登记规则。
type fakeGrader struct {
	name    string
	red     bool
	applies map[string]bool
}

func (g fakeGrader) Name() string  { return g.name }
func (g fakeGrader) RedLine() bool { return g.red }
func (g fakeGrader) Grade(context.Context, *grader.Trial) (grader.Score, error) {
	return grader.Score{Pass: true, Value: 1}, nil
}

// declare 把 gs 的适用规则登记进 Declared，测试结束恢复。
func declare(t *testing.T, gs []grader.Grader) []grader.Grader {
	t.Helper()
	for _, g := range gs {
		f, ok := g.(fakeGrader)
		if !ok || f.applies == nil {
			continue
		}
		if _, exists := Declared[f.name]; exists {
			t.Fatalf("fake grader %s shadows a real rule", f.name)
		}
		Declared[f.name] = func(s *sample.Sample) bool { return f.applies[s.ID] }
		t.Cleanup(func() { delete(Declared, f.name) })
	}
	return gs
}

func ids(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func pool() []*sample.Sample {
	reason := "needs REAL"
	return []*sample.Sample{
		{ID: "r1", Suite: SuiteRegression}, {ID: "r2", Suite: SuiteRegression}, {ID: "r3", Suite: SuiteRegression},
		{ID: "rq", Suite: SuiteRegression, Quarantine: &reason},
		{ID: "ru", Suite: SuiteRegression, Tags: []string{TagUserSim}},
		{ID: "t1", Suite: SuiteRedteam}, {ID: "t2", Suite: SuiteConstraint},
		{ID: "c1", Suite: "capability"},
	}
}

func graders() []grader.Grader {
	return []grader.Grader{
		fakeGrader{name: "a", red: true, applies: ids("r1", "r2", "r3", "rq")},
		fakeGrader{name: "b", red: true, applies: ids("t1", "t2", "rq")},
		fakeGrader{name: "never", red: true, applies: ids("rq")},
		fakeGrader{name: "quality", red: false},
	}
}

func okList() *List {
	return &List{
		Samples:     []string{"r1", "r2"},
		Supplement:  []Supplement{{ID: "t1", Grader: "b"}, {ID: "t2", Grader: "b"}},
		Uncoverable: []Uncoverable{{Grader: "never", Reason: "only quarantined samples"}},
	}
}

func TestCheck_OK(t *testing.T) {
	if err := Check(okList(), pool(), declare(t, graders())); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// 红线门禁：每种违规都必须被拒（应失败用例）。
func TestCheck_Rejects(t *testing.T) {
	cases := []struct {
		name string
		edit func(l *List)
		want string
	}{
		{"missing sample", func(l *List) { l.Samples = append(l.Samples, "nope") }, "nope: no such sample"},
		{"duplicate", func(l *List) { l.Samples = append(l.Samples, "r1") }, "r1: listed twice"},
		{"quarantined", func(l *List) { l.Samples = append(l.Samples, "rq") }, "rq: quarantined"},
		{"user_sim", func(l *List) { l.Samples = append(l.Samples, "ru") }, "ru: quarantined or tagged user_sim"},
		{"not regression", func(l *List) { l.Samples = append(l.Samples, "c1") }, "c1: samples must be in regression"},
		{"redteam in samples", func(l *List) { l.Samples = append(l.Samples, "t1") }, "t1: samples must be in regression"},
		{"red line under-covered", func(l *List) { l.Samples = []string{"r1"} }, "red-line grader a: 1 applicable samples"},
		{"supplement not needed", func(l *List) { l.Supplement = append(l.Supplement, Supplement{ID: "t1", Grader: "a"}) }, "regression already has 3 runnable samples for a"},
		{"supplement not red line", func(l *List) { l.Supplement[0].Grader = "quality" }, "quality is not a red-line grader"},
		{"supplement wrong suite", func(l *List) { l.Supplement[0].ID = "c1" }, "supplement c1: must be in redteam|constraint"},
		{"supplement does not apply", func(l *List) {
			l.Supplement = []Supplement{{ID: "t1", Grader: "b"}, {ID: "t2", Grader: "never"}}
		}, "supplement t2: never does not apply"},
		{"uncoverable but coverable", func(l *List) { l.Uncoverable = append(l.Uncoverable, Uncoverable{Grader: "b", Reason: "x"}) }, "uncoverable b: regression+redteam+constraint has 2"},
		{"uncoverable no reason", func(l *List) { l.Uncoverable[0].Reason = " " }, "uncoverable never: reason is empty"},
		{"uncoverable unknown", func(l *List) { l.Uncoverable = append(l.Uncoverable, Uncoverable{Grader: "zzz", Reason: "x"}) }, "uncoverable zzz: not a red-line grader"},
		{"uncovered red line not declared", func(l *List) { l.Uncoverable = nil }, "red-line grader never: 0 applicable samples"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := okList()
			tc.edit(l)
			err := Check(l, pool(), declare(t, graders()))
			if !errors.Is(err, ErrCoverage) {
				t.Fatalf("err = %v, want ErrCoverage", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// 新增红线打分器自动纳入覆盖要求（不手写红线清单）。
func TestCheck_NewRedLineAutoIncluded(t *testing.T) {
	gs := append(graders(), fakeGrader{name: "new_red", red: true, applies: ids("r3")})
	err := Check(okList(), pool(), declare(t, gs))
	if err == nil || !strings.Contains(err.Error(), "red-line grader new_red: 0 applicable samples") {
		t.Fatalf("err = %v, want new_red under-covered", err)
	}
}

// 未在 Declared 登记适用规则的红线打分器失败关闭（不回退到打分器返回值）。
func TestCheck_UndeclaredRedLineFails(t *testing.T) {
	gs := append(declare(t, graders()), fakeGrader{name: "undeclared", red: true})
	err := Check(okList(), pool(), gs)
	if !errors.Is(err, ErrCoverage) || !strings.Contains(err.Error(), "red-line grader undeclared: no applicability rule") {
		t.Fatalf("err = %v, want undeclared rule error", err)
	}
}

// 声明规则按样本期望判定：未声明 expect.constraint 的样本不算 constraint_blocked 适用，声明了才算。
func TestApplies_Declared(t *testing.T) {
	plain := &sample.Sample{ID: "p"}
	cons := &sample.Sample{ID: "c", Expect: sample.Expect{Constraint: &sample.ConstraintExpect{}}}
	xf := &sample.Sample{ID: "x", Expect: sample.Expect{Foreign: []string{"farm-x"}}}
	cases := []struct {
		grader string
		s      *sample.Sample
		want   bool
	}{
		{"constraint_blocked", plain, false},
		{"constraint_blocked", cons, true},
		{"cross_farm_isolation", plain, false},
		{"cross_farm_isolation", xf, true},
		{"schema_valid", plain, true},
		{"no_such_grader", plain, false},
	}
	for _, tc := range cases {
		if got := Applies(tc.grader, tc.s); got != tc.want {
			t.Errorf("Applies(%s, %s) = %v, want %v", tc.grader, tc.s.ID, got, tc.want)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	l, err := Load(write("ok.yaml", "samples: [a, b]\nsupplement:\n  - {id: c, grader: g}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(l.IDs(), ","); got != "a,b,c" {
		t.Fatalf("IDs = %s", got)
	}
	for name, body := range map[string]string{"empty.yaml": "samples: []\n", "unknown.yaml": "samples: [a]\nextra: 1\n"} {
		if _, err := Load(write(name, body)); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}
