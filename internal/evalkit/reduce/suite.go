package reduce

// NightlyK 是夜间全量的 pass^k 判定次数（P1 L8：redteam 单列 pass^5）。
const NightlyK = 5

// SuiteStat 是一个套件的红线 pass^k 汇总（P1 L8 夜间全量 regression + redteam）。
type SuiteStat struct {
	Suite   string
	Samples int
	Trials  int
	// MinN 是套件内样本的最小 trial 数；PassKK 只在 MinN ≥ NightlyK 时有意义。
	MinN int
	// MeanPassK 是各样本红线全过 pass^k（k = n）的均值；AllPassSamples 是 pass^k = 1（n 次全过）的样本数。
	MeanPassK      float64
	AllPassSamples int
	// PassKK 是各样本 PassHatK(n, c, NightlyK) 的均值；MinN < NightlyK 时为 0，由报告标「不判定」。
	PassKK float64
	// GraderErrors 是套件内含 grader_error 的 trial 数（P1-L8-A 审查修复）：非 0 时 pass^k 由报告标「不可判定」。
	GraderErrors int
	// Inapplicable 是套件内没有一个适用 trial 的红线维度（按 RedLines 顺序；P1-L8-A 审查修复）：这些维度在该套件
	// 全是默认放行，套件 pass^k 不覆盖它们，报告标「无适用样本」。
	Inapplicable []string
}

// AllPass 报告套件内每个样本 n 次红线全过（报告只在此时写 1.00）。
func (s SuiteStat) AllPass() bool { return s.Samples > 0 && s.AllPassSamples == s.Samples }

// Judged 报告该套件能否给出 pass^NightlyK（每个样本都跑满 NightlyK 次）。
func (s SuiteStat) Judged() bool { return s.MinN >= NightlyK }

// bySuite 按套件聚合样本，顺序为套件首次出现顺序；redLines 用于列出套件内无适用样本的红线。
func bySuite(samples []*Sample, redLines []string) []SuiteStat {
	idx := map[string]int{}
	var out []SuiteStat
	sumK := map[string]float64{}
	sumKK := map[string]float64{}
	applicable := map[string]map[string]int{} // suite → 红线 → 适用 trial 数
	for _, s := range samples {
		i, ok := idx[s.Suite]
		if !ok {
			i = len(out)
			idx[s.Suite] = i
			out = append(out, SuiteStat{Suite: s.Suite, MinN: s.N})
			applicable[s.Suite] = map[string]int{}
		}
		st := &out[i]
		st.Samples++
		st.Trials += s.N
		st.GraderErrors += s.GraderErrors
		for name, c := range s.Applicable {
			applicable[s.Suite][name] += c
		}
		st.MinN = min(st.MinN, s.N)
		sumK[s.Suite] += s.PassK()
		sumKK[s.Suite] += PassHatK(s.N, s.AllPass, NightlyK)
		if s.N > 0 && s.AllPass == s.N {
			st.AllPassSamples++
		}
	}
	for i := range out {
		st := &out[i]
		st.MeanPassK = sumK[st.Suite] / float64(st.Samples)
		if st.Judged() {
			st.PassKK = sumKK[st.Suite] / float64(st.Samples)
		}
		for _, name := range redLines {
			if applicable[st.Suite][name] == 0 {
				st.Inapplicable = append(st.Inapplicable, name)
			}
		}
	}
	return out
}
