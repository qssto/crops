package evalkit

import (
	"context"
	"fmt"
	"sync"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
)

// TokenCounter 返回一个 trial 消耗的 LLM token（预算累计用）；默认 TrialTokens，单测注入（桩模型 token 为 0）。
type TokenCounter func(t *Trial) int

// TrialTokens 累加 trial 明细中全部 LLM 调用的 prompt + completion token（与 reduce Metrics.Tokens 同口径）。
func TrialTokens(t *Trial) int {
	if t.Detail == nil {
		return 0
	}
	n := 0
	for _, st := range t.Detail.Steps {
		for _, l := range st.LLMCalls {
			n += l.PromptTokens + l.CompletionTokens
		}
	}
	return n
}

// WithTokenCounter 替换 token 计数（预算单测用）。
func WithTokenCounter(fn TokenCounter) RunnerOption {
	return func(r *Runner) { r.tokens = fn }
}

// Concurrency 是 trial 并发度：floor(qpm × 0.7 / 每 trial 估计 LLM 调用数)，至少 1（P2 §6.8 技术实现 6）。
func Concurrency(qpm, callsPerTrial int) int {
	if qpm <= 0 || callsPerTrial <= 0 {
		return 1
	}
	return max(1, qpm*7/(10*callsPerTrial))
}

// TrialKey 是 trial 的唯一键 (sample_id, trial_index)（resume 按它挑补跑的 trial）。
type TrialKey struct {
	SampleID string
	Index    int
}

// job 是一个待派发的 trial。
type job struct {
	s     *sample.Sample
	index int
}

// jobsOf 按样本顺序、trial 序号展开全部 trial；only 非 nil 时只保留其中的键（resume）。
func jobsOf(samples []*sample.Sample, n int, only map[TrialKey]bool) []job {
	jobs := make([]job, 0, len(samples)*n)
	for _, s := range samples {
		for i := 0; i < n; i++ {
			if only == nil || only[TrialKey{SampleID: s.ID, Index: i}] {
				jobs = append(jobs, job{s: s, index: i})
			}
		}
	}
	return jobs
}

// budget 是累计 token 与预算（limit 0 = 不限），并发 trial 结束时累加。
type budget struct {
	mu       sync.Mutex
	limit    int
	used     int
	exceeded bool
}

func (b *budget) add(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used += n
	if b.limit > 0 && b.used > b.limit {
		b.exceeded = true
	}
}

func (b *budget) over() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exceeded
}

// sampleKB 是按样本缓存的知识快照（同一样本只打开一次，与原顺序实现一致）。
type sampleKB struct {
	kb  grader.KnowledgeIndex
	err error
}

// dispatch 按 jobs 顺序派发 trial，同时在跑的不超过 opts.Concurrency（≤1 时顺序执行）；每次派发前检查 ctx、预算与
// 已发生的 trial panic：超预算即停止派发（在跑的 trial 跑完），panic 后先停派发、等在跑的结束再重新抛出（并发 1 时与
// 顺序执行一致）。结果按 jobs 顺序返回（并发不改变 trial 顺序与内容），未派发的不出现。返回的 exceeded 只在超预算且
// 确有未派发的 trial 时为 true（最后一个 trial 才让累计超预算不算）。
func (r *Runner) dispatch(ctx context.Context, jobs []job, opts Options) ([]Trial, bool, error) {
	slots := make(chan struct{}, max(1, opts.Concurrency))
	results := make([]*Trial, len(jobs))
	kbs := make(map[*sample.Sample]sampleKB)
	b := &budget{limit: opts.TokenBudget}
	var wg sync.WaitGroup
	var stopErr error
	var panicMu sync.Mutex
	var panicked any
	hasPanicked := func() bool {
		panicMu.Lock()
		defer panicMu.Unlock()
		return panicked != nil
	}
	exceeded := false
	for k, j := range jobs {
		slots <- struct{}{}
		if hasPanicked() {
			<-slots
			break
		}
		if err := ctx.Err(); err != nil {
			<-slots
			stopErr = fmt.Errorf("evalkit: stopped before %s#%d: %w", j.s.ID, j.index, err)
			break
		}
		if b.over() {
			<-slots
			exceeded = true
			break
		}
		kn, ok := kbs[j.s]
		if !ok {
			kn.kb, kn.err = r.grading.openKB(ctx, j.s)
			kbs[j.s] = kn
		}
		wg.Add(1)
		go func(k int, j job, kn sampleKB) {
			defer wg.Done()
			defer func() { <-slots }()
			// trial panic 交回调用方 goroutine 重新抛出（cmd/eval recordRun 据此置 FAILED），不在 worker 里崩进程。
			defer func() {
				if p := recover(); p != nil {
					panicMu.Lock()
					panicked = p
					panicMu.Unlock()
				}
			}()
			var t Trial
			if kn.err != nil {
				t = failedTrial(j.s, j.index, kn.err)
			} else {
				t = r.runTrial(ctx, j.s, j.index, kn.kb)
			}
			b.add(r.tokens(&t))
			results[k] = &t
		}(k, j, kn)
	}
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}
	trials := make([]Trial, 0, len(jobs))
	for _, t := range results {
		if t != nil {
			trials = append(trials, *t)
		}
	}
	return trials, exceeded, stopErr
}
