package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/report"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/store"
)

// errPointerExists 表示 baseline 指针文件已存在（冻结不覆盖已登记的基线，P1 手册「不重跑、不覆盖」）。
var errPointerExists = errors.New("baseline pointer file already exists")

// errBackfillSource 表示 --backfill 的 yaml 与 --exp / 当前样本对不上。
var errBackfillSource = errors.New("backfill source does not match")

// errBaselineNotFrozen 表示 --baseline 指向的实验在库中未冻结（frozen = 0），不能当对比基线。
var errBaselineNotFrozen = errors.New("baseline experiment is not frozen")

// pointerFileMode 是指针 yaml 的权限（入库文件）。
const pointerFileMode = 0o644

// Pointer 是冻结基线的指针文件 eval/baselines/{name}.yaml：名字 → experiment.id + 快照（P2 §6.1 技术实现 2）。
type Pointer struct {
	Name        string           `yaml:"name"`
	ExpID       string           `yaml:"exp_id"`
	FrozenAt    time.Time        `yaml:"frozen_at"`
	Source      string           `yaml:"source,omitempty"`
	Bucket      string           `yaml:"bucket"`
	ManifestKey string           `yaml:"manifest_key"`
	Snapshot    evalkit.Snapshot `yaml:"snapshot"`
	Notes       []string         `yaml:"notes,omitempty"`
}

// cmdBaseline 分发 baseline 的子命令（现只有 freeze）。
func cmdBaseline(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "freeze" {
		fmt.Fprintln(stderr, "usage: eval baseline freeze --name <名> --exp <id> [--backfill <yaml>]")
		return flag.ErrHelp
	}
	return cmdFreeze(ctx, args[1:], stdout, stderr)
}

func cmdFreeze(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("baseline freeze", flag.ContinueOnError)
	var sf sampleFlags
	sf.register(fs)
	var stf storeFlags
	stf.register(fs)
	name := fs.String("name", "", "基线名（写 {baselines-dir}/{name}.yaml）")
	expID := fs.Uint64("exp", 0, "要冻结的 experiment.id")
	backfillFile := fs.String("backfill", "", "P1 实验（snapshot 为 NULL）的补录来源：report.PublishBaseline 写的 baseline yaml")
	baselinesDir := fs.String("baselines-dir", defaultBaselines, "baseline yaml 目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := report.ValidateBaselineName(*name); err != nil {
		return err
	}
	if *expID == 0 {
		return errors.New("baseline freeze: --exp is required")
	}
	pointerPath := filepath.Join(*baselinesDir, *name+".yaml")
	// 早检查只为不连库就失败；真正的不覆盖由 writePointer 的 os.Link（目标已存在即失败）保证。
	if _, err := os.Stat(pointerPath); err == nil {
		return fmt.Errorf("%w: %s", errPointerExists, pointerPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("baseline freeze: stat %s: %w", pointerPath, err)
	}
	var bf *store.Backfill
	var refs []string
	if *backfillFile != "" {
		ls, err := sf.loadValidSamples()
		if err != nil {
			return err
		}
		if bf, refs, err = loadBackfill(ctx, *backfillFile, *expID, ls.samples, ls.kbContent); err != nil {
			return err
		}
	}
	es, err := stf.open(ctx, stderr)
	if err != nil {
		return fmt.Errorf("eval store unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, es.close()) }()
	frozen, key, pointerOnly, err := freezeExperiment(ctx, es, *name, *expID, bf, refs)
	if err != nil {
		return err
	}
	at, note, err := frozenAt(frozen, pointerOnly, clock.System{}.Now)
	if err != nil {
		return err
	}
	p := &Pointer{Name: *name, ExpID: store.FormatID(*expID), FrozenAt: at, Source: *backfillFile,
		Bucket: es.bucket, ManifestKey: key, Snapshot: *frozen.Snapshot}
	if frozen.Snapshot.Model == evalkit.ModelStub {
		p.Notes = append(p.Notes, "stub 模型基线：只证明链路与红线，不代表真实模型质量")
	}
	if note != "" {
		p.Notes = append(p.Notes, note)
	}
	if err := writePointer(pointerPath, p); err != nil {
		return err
	}
	if pointerOnly {
		fmt.Fprintf(stdout, "baseline %s pointer-only: exp_id=%s already frozen with the same snapshot and name, pointer=%s manifest=%s/%s\n",
			p.Name, p.ExpID, pointerPath, es.bucket, key)
		return nil
	}
	fmt.Fprintf(stdout, "baseline %s frozen: exp_id=%s pointer=%s manifest=%s/%s backfilled=%t\n",
		p.Name, p.ExpID, pointerPath, es.bucket, key, bf != nil)
	return nil
}

// freezeExperiment 冻结实验并返回冻结记录与 manifest 键。冻结前把基线名写入 {id}/baseline.json（库里没有名字列）。
// 实验已冻结（上次冻结已提交、指针没写成）时走 pointer-only：库中快照与这次要登记的一致、且 name 与登记名相同才返回
// pointerOnly = true，只补写指针，不再写 manifest；快照不一致返回 ErrFrozenMismatch，名字不同返回 ErrBaselineName
// （不能给已冻结实验另起一个名字，如 --name baseline_p1.r2 --exp 102）。
func freezeExperiment(ctx context.Context, es *evalStore, name string, id uint64, bf *store.Backfill, refs []string) (
	_ *store.Record, key string, pointerOnly bool, err error) {
	key = store.ManifestKey(id)
	rec, err := es.exps.Get(ctx, id)
	if err != nil {
		return nil, "", false, err
	}
	if rec.Frozen {
		if err := store.CheckBaselineName(ctx, es.getter, es.bucket, id, name); err != nil {
			return nil, "", false, err
		}
		matched, err := es.exps.MatchFrozen(ctx, id, bf)
		if err != nil {
			return nil, "", false, err
		}
		return matched, key, true, nil
	}
	// P2 实验的 manifest 已由 run 写入；P1 补录在此补写。对象先于冻结写：对象可覆盖，冻结不可逆。
	if bf != nil {
		if key, err = store.PutManifest(ctx, es.objects, es.bucket, id, bf.Snapshot.SamplesHash, refs); err != nil {
			return nil, "", false, err
		}
	}
	if err := store.PutBaselineName(ctx, es.objects, es.bucket, id, name); err != nil {
		return nil, "", false, err
	}
	frozen, err := es.exps.Freeze(ctx, id, bf)
	if err != nil {
		return nil, "", false, err
	}
	return frozen, key, false, nil
}

// pointerOnlyFrozenAtNote 写进 pointer-only 补写指针的 notes：库里没有冻结时间列，frozen_at 取实验 finished_at。
const pointerOnlyFrozenAtNote = "pointer-only 补写：库中没有冻结时间列，frozen_at 取实验 finished_at（早于实际冻结时间）"

// frozenAt 是指针的 frozen_at：本次冻结取当前时间；pointer-only 补写取库中 finished_at 并返回说明，finished_at 为空报错。
func frozenAt(rec *store.Record, pointerOnly bool, now func() time.Time) (time.Time, string, error) {
	if !pointerOnly {
		return now().UTC(), "", nil
	}
	if rec.FinishedAt == nil {
		return time.Time{}, "", fmt.Errorf("baseline freeze: frozen exp %d has no finished_at", rec.ID)
	}
	return rec.FinishedAt.UTC(), pointerOnlyFrozenAtNote, nil
}

// loadBackfill 由 P1 baseline yaml 组装补录（P2 §6.1 技术实现 2）：快照取 yaml，缺的字段按事实填
// （loader v0、retrieval progressive、judge_versions 空、suites / tags / n 取 yaml、token 0 与 cost 0（stub）；
// wall_time 留空由 store.Freeze 按 started_at / finished_at 算）。按 yaml 的 suites / tags 从当前样本重选，
// samples_hash 必须与 yaml 一致，否则当前样本版本不能代表该实验（拒绝）；knowledge_ref 按该实验 git_sha 的
// knowledge/ 内容重算为 kc: 哈希（P1 实验跑在工作区上，提交内容只是近似，见 dev-log P2-L0）。
// 返回补录与 manifest 用的 sample_id@version。
func loadBackfill(ctx context.Context, path string, expID uint64, samples []*sample.Sample, kb knowledge.TreeReader) (
	*store.Backfill, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("backfill: read %s: %w", path, err)
	}
	var b report.Baseline
	if err := yaml.Unmarshal(raw, &b); err != nil {
		return nil, nil, fmt.Errorf("backfill: parse %s: %w", path, err)
	}
	switch {
	case b.ExpID != strconv.FormatUint(expID, 10):
		return nil, nil, fmt.Errorf("%w: %s exp_id %q != --exp %d", errBackfillSource, path, b.ExpID, expID)
	case b.Snapshot.Model != evalkit.ModelStub:
		// token / cost 只有 stub 能按事实填 0；真实模型的 P1 实验需另行补录。
		return nil, nil, fmt.Errorf("%w: %s model %q is not %s", errBackfillSource, path, b.Snapshot.Model, evalkit.ModelStub)
	case len(b.Suites) == 0 || b.N < 1:
		return nil, nil, fmt.Errorf("%w: %s has no suites / n", errBackfillSource, path)
	}
	selected := evalkit.Select(samples, b.Suites, b.Tags)
	if got := evalkit.SamplesHash(selected); got != b.Snapshot.SamplesHash {
		return nil, nil, fmt.Errorf("%w: samples_hash of suites %v tags %v is %s, %s says %s",
			errBackfillSource, b.Suites, b.Tags, got, path, b.Snapshot.SamplesHash)
	}
	kref, err := evalkit.KnowledgeRef(ctx, kb, selected, b.Snapshot.GitSHA)
	if err != nil {
		return nil, nil, fmt.Errorf("backfill: %w", err)
	}
	snap := b.Snapshot
	snap.KnowledgeRef = kref
	snap.LoaderStrategy, snap.RetrievalStrategy, snap.JudgeVersions = evalkit.LoaderV0, evalkit.RetrievalProgressive, map[string]string{}
	snap.Suites, snap.Tags, snap.N, snap.TokenUsed, snap.Cost, snap.WallTime = b.Suites, recordedTags(b.Tags), b.N, 0, 0, ""
	versions := make(map[string]int, len(selected))
	for _, s := range selected {
		versions[s.ID] = s.Version
	}
	return &store.Backfill{Snapshot: snap, SampleVersions: versions}, evalkit.SampleRefs(selected), nil
}

// writePointer 原子写指针文件：同目录临时文件写完 fsync、改权限后 os.Link 到目标（目标已存在即失败，不覆盖），
// 再删临时文件。读者只会看到不存在或完整内容；崩溃最多留下 .baseline-*.tmp 临时文件，不会留下半截指针。
func writePointer(path string, p *Pointer) (err error) {
	out, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("baseline: marshal pointer: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("baseline: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".baseline-*.tmp")
	if err != nil {
		return fmt.Errorf("baseline: create temp in %s: %w", dir, err)
	}
	defer func() {
		if rmErr := os.Remove(tmp.Name()); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("baseline: remove temp %s: %w", tmp.Name(), rmErr))
		}
	}()
	if err := writeSynced(tmp, out); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", errPointerExists, path)
		}
		return fmt.Errorf("baseline: link %s: %w", path, err)
	}
	return nil
}

// writeSynced 写入内容、设权限、fsync 并关闭 f（出错时也关闭）。
func writeSynced(f *os.File, b []byte) error {
	steps := []func() error{
		func() error { _, err := f.Write(b); return err },
		func() error { return f.Chmod(pointerFileMode) },
		f.Sync,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return errors.Join(fmt.Errorf("baseline: write %s: %w", f.Name(), err), f.Close())
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("baseline: close %s: %w", f.Name(), err)
	}
	return nil
}

// experimentGetter 是 resolveBaseline 读库所需的最小接口（store.Experiments 实现）。
type experimentGetter interface {
	Get(ctx context.Context, id uint64) (*store.Record, error)
}

// resolveBaseline 把 --baseline 解析为 experiment.id，并要求该实验在库中已冻结（frozen = 1）：
// 指针文件可手改、P0 / P1 的 baseline yaml 也有 exp_id，只有库中冻结才算基线。
func resolveBaseline(ctx context.Context, exps experimentGetter, dir, ref string) (uint64, error) {
	id, err := baselineID(dir, ref)
	if err != nil {
		return 0, err
	}
	rec, err := exps.Get(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("baseline %s: %w", ref, err)
	}
	if !rec.Frozen {
		return 0, fmt.Errorf("%w: baseline %s → experiment %d（先 baseline freeze）", errBaselineNotFrozen, ref, id)
	}
	return id, nil
}

// baselineID 把 --baseline 解析为 experiment.id：纯数字直接用，否则读 {dir}/{name}.yaml 的 exp_id
// （冻结指针与 P0 / P1 的 baseline yaml 都有该字段）。
func baselineID(dir, ref string) (uint64, error) {
	if id, err := strconv.ParseUint(ref, 10, 64); err == nil {
		return id, nil
	}
	if err := report.ValidateBaselineName(ref); err != nil {
		return 0, err
	}
	path := filepath.Join(dir, ref+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("baseline %s: %w", ref, err)
	}
	var doc struct {
		ExpID string `yaml:"exp_id"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return 0, fmt.Errorf("baseline %s: parse %s: %w", ref, path, err)
	}
	id, err := strconv.ParseUint(doc.ExpID, 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("baseline %s: %s exp_id %q is not an experiment id", ref, path, doc.ExpID)
	}
	return id, nil
}
