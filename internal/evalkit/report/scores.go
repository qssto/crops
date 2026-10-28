package report

import (
	"fmt"

	"agri-steward/internal/evalkit"
)

// dimStats 是一个打分维度在全部 trial 上的计数。
type dimStats struct {
	name    string
	redLine bool
	pass    int
	fail    int
	errored int // grader_error
	value   float64
}

func (d *dimStats) meanValue() float64 {
	if n := d.pass + d.fail; n > 0 {
		return d.value / float64(n)
	}
	return 0
}

// collectDims 按首次出现顺序汇总各维度；红线维度名另行返回。
func collectDims(trials []evalkit.Trial) (dims []*dimStats, redLines []string) {
	idx := map[string]*dimStats{}
	for _, t := range trials {
		for _, sc := range t.Scores {
			d, ok := idx[sc.Name]
			if !ok {
				d = &dimStats{name: sc.Name, redLine: sc.RedLine}
				idx[sc.Name] = d
				dims = append(dims, d)
				if sc.RedLine {
					redLines = append(redLines, sc.Name)
				}
			}
			switch {
			case sc.Error != "":
				d.errored++
			case sc.Pass:
				d.pass++
				d.value += sc.Value
			default:
				d.fail++
				d.value += sc.Value
			}
		}
	}
	return dims, redLines
}

// sampleRedLine 是一个样本的红线通过计数：n 为 trial 数，pass[dim] 为该维度通过次数，all 为红线全过次数。
// 出错（INFRA_ERROR / grader_error）的 trial 计入 n、不计通过。
type sampleRedLine struct {
	id   string
	n    int
	pass map[string]int
	all  int
}

func collectRedLines(trials []evalkit.Trial, redLines []string) []*sampleRedLine {
	var out []*sampleRedLine
	idx := map[string]*sampleRedLine{}
	for _, t := range trials {
		s, ok := idx[t.SampleID]
		if !ok {
			s = &sampleRedLine{id: t.SampleID, pass: map[string]int{}}
			idx[t.SampleID] = s
			out = append(out, s)
		}
		s.n++
		allPass := t.Error == ""
		for _, name := range redLines {
			sc, ok := t.ScoreOf(name)
			if t.Error == "" && ok && sc.Error == "" && sc.Pass {
				s.pass[name]++
				continue
			}
			allPass = false
		}
		if allPass {
			s.all++
		}
	}
	return out
}

// passHatK 是 k 次独立运行全部通过的无偏估计 C(c,k)/C(n,k)；c < k 为 0，k 非法为 0。
func passHatK(n, c, k int) float64 {
	if k < 1 || k > n || c < k {
		return 0
	}
	p := 1.0
	for i := 0; i < k; i++ {
		p *= float64(c-i) / float64(n-i)
	}
	return p
}

// fmtPassK 输出「c/n → pass^k」，k 取样本 trial 数 n。
func fmtPassK(n, c int) string {
	return fmt.Sprintf("%d/%d → %.2f", c, n, passHatK(n, c, n))
}
