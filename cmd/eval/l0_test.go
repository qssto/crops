package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/store"
)

// p1ExitBaseline 是 P1 收口基线 yaml（exp 102，snapshot 列为 NULL，L0 用它补录）。
const p1ExitBaseline = "baseline_p1_exit.yaml"

func repoSamples(t *testing.T) *loadedSamples {
	t.Helper()
	sf := sampleFlags{dir: filepath.Join(repoRoot, defaultDir), taxonomy: filepath.Join(repoRoot, defaultTax), repo: repoRoot}
	ls, err := sf.loadValidSamples()
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

// addedAfterP1Exit 是 exp 102 之后新增进 regression / redteam 的样本（P2 L4 评测资产：redteam/tank_mix）。
// exp 102 的补录已冻结（baseline_p1 指针），本测试按 exp 102 当时的样本集重放补录校验，故先去掉它们。
var addedAfterP1Exit = []string{"rt-mix-0001", "rt-mix-0002", "rt-mix-0003", "rt-mix-0004", "rt-mix-0005", "rt-mix-0006"}

// TestLoadBackfill_P1Exit 校验 exp 102 的补录：当时的样本按 yaml 的 suites 重选后 samples_hash 不变，快照补齐后无空字段。
func TestLoadBackfill_P1Exit(t *testing.T) {
	ctx := t.Context()
	ls := repoSamples(t)
	ls.samples = slices.DeleteFunc(slices.Clone(ls.samples), func(s *sample.Sample) bool {
		return slices.Contains(addedAfterP1Exit, s.ID)
	})
	path := filepath.Join(repoRoot, defaultBaselines, p1ExitBaseline)
	bf, refs, err := loadBackfill(ctx, path, 102, ls.samples, ls.kbContent)
	if err != nil {
		t.Fatal(err)
	}
	s := bf.Snapshot
	if s.LoaderStrategy != evalkit.LoaderV0 || s.RetrievalStrategy != evalkit.RetrievalProgressive || s.JudgeVersions == nil ||
		len(s.JudgeVersions) != 0 || strings.Join(s.Suites, ",") != "regression,redteam" || s.N != 5 || s.TokenUsed != 0 || s.Cost != 0 ||
		s.Tags == nil || len(s.Tags) != 0 {
		t.Fatalf("backfill snapshot = %+v", s)
	}
	// knowledge_ref 是 exp 102 git_sha 上 knowledge/ 的内容哈希（不是 yaml 里的 WORKTREE）。
	wantRef, err := evalkit.KnowledgeContentRef(ctx, ls.kbContent, s.GitSHA)
	if err != nil {
		t.Fatal(err)
	}
	if s.KnowledgeRef != wantRef || !strings.HasPrefix(s.KnowledgeRef, evalkit.KnowledgeContentPrefix) {
		t.Fatalf("knowledge_ref = %q, want %q", s.KnowledgeRef, wantRef)
	}
	if m := s.MissingConfig(); len(m) != 0 {
		t.Fatalf("missing config fields %v", m)
	}
	// wall_time 由 store.Freeze 按 started_at / finished_at 算，补录本身只差这一项。
	if m := s.Missing(); len(m) != 1 || m[0] != "wall_time" {
		t.Fatalf("missing = %v, want only wall_time", m)
	}
	if len(refs) != len(bf.SampleVersions) || len(refs) == 0 {
		t.Fatalf("refs %d versions %d", len(refs), len(bf.SampleVersions))
	}
	for _, v := range bf.SampleVersions {
		if v < 1 {
			t.Fatalf("version %d < 1", v)
		}
	}

	if _, _, err := loadBackfill(ctx, path, 103, ls.samples, ls.kbContent); !errors.Is(err, errBackfillSource) {
		t.Fatalf("wrong exp: err = %v", err)
	}
	// 样本集变化（少一条 regression 样本）→ samples_hash 不符，拒绝。
	var fewer []*sample.Sample
	dropped := false
	for _, s := range ls.samples {
		if !dropped && s.Suite == "regression" {
			dropped = true
			continue
		}
		fewer = append(fewer, s)
	}
	if _, _, err := loadBackfill(ctx, path, 102, fewer, ls.kbContent); !errors.Is(err, errBackfillSource) {
		t.Fatalf("changed samples: err = %v", err)
	}
}

func TestLoadBackfill_RejectsRealModel(t *testing.T) {
	ls := repoSamples(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot, defaultBaselines, p1ExitBaseline))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "real.yaml")
	body := strings.Replace(string(raw), "model: stub", "model: openai", 2)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadBackfill(t.Context(), p, 102, ls.samples, ls.kbContent); !errors.Is(err, errBackfillSource) {
		t.Fatalf("real model backfill: err = %v", err)
	}
}

func TestBaselineID(t *testing.T) {
	dir := filepath.Join(repoRoot, defaultBaselines)
	for ref, want := range map[string]uint64{"102": 102, "baseline_p1_exit": 102, "baseline_p0": 28} {
		got, err := baselineID(dir, ref)
		if err != nil || got != want {
			t.Errorf("resolve %s = %d %v, want %d", ref, got, err, want)
		}
	}
	for _, bad := range []string{"nope", "../x", ""} {
		if _, err := baselineID(dir, bad); err == nil {
			t.Errorf("resolve %q: want error", bad)
		}
	}
}

// fakeGetter 按 id 返回预置记录；不存在返回 store.ErrNotFound。
type fakeGetter map[uint64]*store.Record

func (g fakeGetter) Get(_ context.Context, id uint64) (*store.Record, error) {
	if r, ok := g[id]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("%w: %d", store.ErrNotFound, id)
}

// TestResolveBaseline_RequiresFrozen：指针 / 数字 id 指向的实验在库中未冻结即拒绝（指针可手改，只认库）。
func TestResolveBaseline_RequiresFrozen(t *testing.T) {
	dir := filepath.Join(repoRoot, defaultBaselines)
	exps := fakeGetter{102: {ID: 102, Frozen: true}, 28: {ID: 28, Frozen: false}}
	if id, err := resolveBaseline(t.Context(), exps, dir, "baseline_p1_exit"); err != nil || id != 102 {
		t.Fatalf("frozen baseline = %d %v", id, err)
	}
	if _, err := resolveBaseline(t.Context(), exps, dir, "baseline_p0"); !errors.Is(err, errBaselineNotFrozen) {
		t.Fatalf("unfrozen baseline: err = %v", err)
	}
	if _, err := resolveBaseline(t.Context(), exps, dir, "28"); !errors.Is(err, errBaselineNotFrozen) {
		t.Fatalf("unfrozen numeric baseline: err = %v", err)
	}
	if _, err := resolveBaseline(t.Context(), exps, dir, "7"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing baseline: err = %v", err)
	}
}

func TestWritePointer_RoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "b", "baseline_x.yaml")
	if err := writePointer(p, &Pointer{Name: "baseline_x", ExpID: "7", Snapshot: evalkit.Snapshot{GitSHA: "abc"}}); err != nil {
		t.Fatal(err)
	}
	id, err := baselineID(filepath.Dir(p), "baseline_x")
	if err != nil || id != 7 {
		t.Fatalf("resolve pointer = %d %v", id, err)
	}
}

// TestWritePointer_Exclusive：指针已存在（含早检查之后被并发写成）时不覆盖，原内容不变。
func TestWritePointer_Exclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "baseline_x.yaml")
	const orig = "exp_id: \"1\"\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePointer(p, &Pointer{Name: "baseline_x", ExpID: "7"}); !errors.Is(err, errPointerExists) {
		t.Fatalf("existing pointer: err = %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != orig {
		t.Fatalf("pointer overwritten: %q", got)
	}
}

// TestL0Commands_FlagErrors 是不连库即失败的参数校验（退出码 1 / 2）。
func TestL0Commands_FlagErrors(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "baseline_dup.yaml")
	if err := os.WriteFile(existing, []byte("exp_id: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		code int
		want error
	}{
		{"baseline no sub", []string{"baseline"}, exitUsage, nil},
		{"freeze bad name", []string{"baseline", "freeze", "--name", "Bad Name", "--exp", "1"}, exitFailed, nil},
		{"freeze no exp", []string{"baseline", "freeze", "--name", "baseline_x"}, exitFailed, nil},
		{"freeze pointer exists", []string{"baseline", "freeze", "--name", "baseline_dup", "--exp", "1", "--baselines-dir", dir}, exitFailed, errPointerExists},
		{"compare bad mode", []string{"compare", "--baseline", "1", "--candidate", "2", "--mode", "x"}, exitFailed, nil},
		{"compare no candidate", []string{"compare", "--baseline", "1"}, exitFailed, nil},
		{"compare reference needs phase", []string{"compare", "--baseline", "1", "--candidate", "2", "--reference", "3"}, exitFailed, compare.ErrReferencePhaseOnly},
		{"aa one exp", []string{"aa", "--from-exps", "5"}, exitFailed, nil},
		{"aa duplicate exp", []string{"aa", "--from-exps", "5,6,5"}, exitFailed, compare.ErrAADuplicate},
		{"aa bad id", []string{"aa", "--from-exps", "5,x"}, exitFailed, nil},
		{"aa runs 1", []string{"aa", "--runs", "1"}, exitFailed, nil},
		{"run bad trigger", []string{"run", "--trigger", "cron"}, exitFailed, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := run(tt.args, &out, &errOut); got != tt.code {
				t.Fatalf("exit = %d, want %d; stderr = %s", got, tt.code, errOut.String())
			}
			if tt.want != nil && !strings.Contains(errOut.String(), tt.want.Error()) {
				t.Fatalf("stderr = %q, want %q", errOut.String(), tt.want)
			}
		})
	}
}

// TestReleaseGate：--fail-on-unreleasable 时不可放行（回归 / 不完整 / 阶段级）返回 errUnreleasable，可放行与默认不报错。
func TestReleaseGate(t *testing.T) {
	ok := &compare.Result{Mode: compare.ModeFactor, Conclusion: compare.ConclusionImproved}
	twoDiffs := []compare.FactorDiff{{Name: "model"}, {Name: "prompt_version"}}
	bad := []*compare.Result{
		{Mode: compare.ModeFactor, Conclusion: compare.ConclusionRegressed},
		{Mode: compare.ModeFactor, Conclusion: compare.ConclusionIncomplete},
		{Mode: compare.ModeFactor, Conclusion: compare.ConclusionNotAttributable, Diffs: twoDiffs},
		{Mode: compare.ModePhase},
	}
	if err := releaseGate(ok, true); err != nil {
		t.Fatalf("releasable: %v", err)
	}
	for i, r := range bad {
		if err := releaseGate(r, true); !errors.Is(err, errUnreleasable) {
			t.Errorf("case %d: err = %v, want errUnreleasable", i, err)
		}
		if err := releaseGate(r, false); err != nil {
			t.Errorf("case %d default: err = %v", i, err)
		}
	}
}

// fakeLifecycle 记录 Start / Finish / Fail；failErr / finishErr 非空时对应方法报错。
type fakeLifecycle struct {
	started, finished, failed []uint64
	failErr, finishErr        error
}

func (f *fakeLifecycle) Finish(_ context.Context, id uint64, _ *evalkit.Experiment) error {
	f.finished = append(f.finished, id)
	return f.finishErr
}

func (f *fakeLifecycle) Start(context.Context, string, evalkit.Snapshot, time.Time) (uint64, error) {
	f.started = append(f.started, 42)
	return 42, nil
}

func (f *fakeLifecycle) Fail(_ context.Context, id uint64, _ time.Time) error {
	f.failed = append(f.failed, id)
	return f.failErr
}

// putter 是 ObjectPutter 桩：err 非空时上传失败。
type putter struct{ err error }

func (p putter) PutObject(context.Context, string, string, []byte, string) error { return p.err }

// TestStartExperiment_ManifestFailMarksFailed：清单写 MinIO 失败时实验经 store 状态更新路径置 FAILED，原错误保留；
// 置 FAILED 本身失败时两个错误都返回。
func TestStartExperiment_ManifestFailMarksFailed(t *testing.T) {
	ctx := t.Context()
	now := func() time.Time { return time.Unix(0, 0) }
	refs := []string{"a@1"}
	boom := errors.New("minio down")

	ok := &fakeLifecycle{}
	if id, err := startExperiment(ctx, ok, putter{}, "eval", store.TriggerManual, evalkit.Snapshot{}, refs, now); err != nil || id != 42 ||
		len(ok.failed) != 0 {
		t.Fatalf("ok start = %d %v failed=%v", id, err, ok.failed)
	}

	lc := &fakeLifecycle{}
	_, err := startExperiment(ctx, lc, putter{err: boom}, "eval", store.TriggerManual, evalkit.Snapshot{}, refs, now)
	if !errors.Is(err, boom) || len(lc.failed) != 1 || lc.failed[0] != 42 || !strings.Contains(err.Error(), "marked FAILED") {
		t.Fatalf("manifest failure: err = %v failed = %v", err, lc.failed)
	}

	dbDown := errors.New("mysql gone")
	lc = &fakeLifecycle{failErr: dbDown}
	_, err = startExperiment(ctx, lc, putter{err: boom}, "eval", store.TriggerManual, evalkit.Snapshot{}, refs, now)
	if !errors.Is(err, boom) || !errors.Is(err, dbDown) {
		t.Fatalf("fail also failing: err = %v", err)
	}
}

// TestMarkFailed_IgnoresCancel：ctx 已取消（如 Ctrl-C）时仍用不随之取消的上下文落库 FAILED。
func TestMarkFailed_IgnoresCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	lc := &cancelAwareLifecycle{}
	if err := markFailed(ctx, lc, 7, context.Canceled, time.Now); !errors.Is(err, context.Canceled) || !lc.sawLive {
		t.Fatalf("err = %v sawLive = %t", err, lc.sawLive)
	}
}

type cancelAwareLifecycle struct{ sawLive, hasDeadline bool }

func (c *cancelAwareLifecycle) Start(context.Context, string, evalkit.Snapshot, time.Time) (uint64, error) {
	return 0, errors.New("unused")
}

func (c *cancelAwareLifecycle) Finish(context.Context, uint64, *evalkit.Experiment) error {
	return errors.New("unused")
}

func (c *cancelAwareLifecycle) Fail(ctx context.Context, _ uint64, _ time.Time) error {
	c.sawLive = ctx.Err() == nil
	_, c.hasDeadline = ctx.Deadline()
	return nil
}

// TestMarkFailed_HasTimeout：置 FAILED 的上下文不随 ctx 取消，但有超时（库挂起时不无限等）。
func TestMarkFailed_HasTimeout(t *testing.T) {
	lc := &cancelAwareLifecycle{}
	if err := markFailed(t.Context(), lc, 7, errors.New("x"), time.Now); err == nil || !lc.hasDeadline {
		t.Fatalf("err = %v hasDeadline = %t", err, lc.hasDeadline)
	}
}

// TestRecordRun_MarksFailed：建行后 Run 失败、Finish 失败、Run panic 都把实验置 FAILED（panic 置失败后原样重新 panic）；
// 成功时 Finish、不置失败。
func TestRecordRun_MarksFailed(t *testing.T) {
	ctx := t.Context()
	now := func() time.Time { return time.Unix(0, 0) }
	refs := []string{"a@1"}
	snap := evalkit.Snapshot{SamplesHash: "h"}
	boom := errors.New("runner boom")
	okRun := func(context.Context, uint64) (*evalkit.Experiment, error) { return &evalkit.Experiment{ID: "42"}, nil }

	lc := &fakeLifecycle{}
	if exp, id, err := recordRun(ctx, lc, putter{}, "eval", store.TriggerManual, snap, refs, now, okRun); err != nil || id != 42 ||
		exp == nil || len(lc.finished) != 1 || len(lc.failed) != 0 {
		t.Fatalf("ok: %v %d %v finished=%v failed=%v", exp, id, err, lc.finished, lc.failed)
	}

	lc = &fakeLifecycle{}
	failRun := func(context.Context, uint64) (*evalkit.Experiment, error) { return nil, boom }
	if _, _, err := recordRun(ctx, lc, putter{}, "eval", store.TriggerManual, snap, refs, now, failRun); !errors.Is(err, boom) ||
		len(lc.failed) != 1 || len(lc.finished) != 0 {
		t.Fatalf("run fails: err = %v failed=%v finished=%v", err, lc.failed, lc.finished)
	}

	dbDown := errors.New("finish tx lost")
	lc = &fakeLifecycle{finishErr: dbDown}
	if _, _, err := recordRun(ctx, lc, putter{}, "eval", store.TriggerManual, snap, refs, now, okRun); !errors.Is(err, dbDown) ||
		len(lc.failed) != 1 || lc.failed[0] != 42 {
		t.Fatalf("finish fails: err = %v failed=%v", err, lc.failed)
	}

	lc = &fakeLifecycle{}
	panicRun := func(context.Context, uint64) (*evalkit.Experiment, error) { panic("grader nil map") }
	func() {
		defer func() {
			if r := recover(); r != "grader nil map" {
				t.Fatalf("recovered %v, want the original panic value", r)
			}
		}()
		_, _, err := recordRun(ctx, lc, putter{}, "eval", store.TriggerManual, snap, refs, now, panicRun)
		t.Fatalf("panic swallowed: %v", err)
	}()
	if len(lc.failed) != 1 || lc.failed[0] != 42 {
		t.Fatalf("panic must mark FAILED: failed=%v", lc.failed)
	}
}

// TestWritePointer_Atomic：临时文件写完 fsync 后 link 到目标：成功或目标已存在时都不留临时文件，目标权限 0644、内容完整；
// 并发写同一目标只有一个成功。
func TestWritePointer_Atomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "baseline_x.yaml")
	const writers = 8
	errs := make(chan error, writers)
	for i := range writers {
		go func() { errs <- writePointer(p, &Pointer{Name: "baseline_x", ExpID: strconv.Itoa(i + 1)}) }()
	}
	won := 0
	for range writers {
		switch err := <-errs; {
		case err == nil:
			won++
		case !errors.Is(err, errPointerExists):
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d writers won, want 1", won)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "baseline_x.yaml" {
		t.Fatalf("leftover files: %v", entries)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != pointerFileMode {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	if id, err := baselineID(dir, "baseline_x"); err != nil || id < 1 || id > writers {
		t.Fatalf("pointer = %d %v", id, err)
	}
}

// nameStore 是登记名对象的桩：names 为 experiment.id → 登记名，缺省即对象不存在。
type nameStore map[uint64]string

func (n nameStore) GetObject(_ context.Context, _, key string) ([]byte, error) {
	for id, name := range n {
		if key == store.BaselineNameKey(id) {
			return []byte(`{"experiment_id":"` + store.FormatID(id) + `","name":"` + name + `"}`), nil
		}
	}
	return nil, errors.New("NoSuchKey")
}

// TestCheckBaselineName：pointer-only 只接受与已登记名一致的名字；名字不同、登记名对象不存在或内容对不上都拒绝。
func TestCheckBaselineName(t *testing.T) {
	ctx := t.Context()
	names := nameStore{102: "baseline_p1", 7: ""}
	if err := store.CheckBaselineName(ctx, names, "eval", 102, "baseline_p1"); err != nil {
		t.Fatalf("same name: %v", err)
	}
	for _, tc := range []struct {
		id   uint64
		name string
	}{{102, "baseline_p1.r2"}, {103, "baseline_p1"}, {7, "baseline_x"}} {
		if err := store.CheckBaselineName(ctx, names, "eval", tc.id, tc.name); !errors.Is(err, store.ErrBaselineName) {
			t.Errorf("exp %d name %s: err = %v, want ErrBaselineName", tc.id, tc.name, err)
		}
	}
}

// TestPointerOnlyFrozenAt：pointer-only 补写的指针 frozen_at 取库中 finished_at（库里没有冻结时间列），并在 notes 写明；
// finished_at 为空时报错。
func TestPointerOnlyFrozenAt(t *testing.T) {
	fin := time.Date(2026, 11, 3, 3, 59, 0, 0, time.UTC)
	at, note, err := frozenAt(&store.Record{ID: 102, FinishedAt: &fin}, true, time.Now)
	if err != nil || !at.Equal(fin) || !strings.Contains(note, "finished_at") {
		t.Fatalf("pointer-only frozen_at = %v %q %v", at, note, err)
	}
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))
	at, note, err = frozenAt(&store.Record{ID: 102, FinishedAt: &fin}, false, func() time.Time { return now })
	if err != nil || !at.Equal(now) || at.Location() != time.UTC || note != "" {
		t.Fatalf("fresh freeze frozen_at = %v %q %v", at, note, err)
	}
	if _, _, err := frozenAt(&store.Record{ID: 102}, true, time.Now); err == nil {
		t.Fatal("pointer-only without finished_at must fail")
	}
}
