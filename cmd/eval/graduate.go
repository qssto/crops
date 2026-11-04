package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/repository"
)

const (
	suiteCapability = "capability"
	suiteRegression = "regression"
)

// suiteLine 匹配样本顶层的 suite: capability 行（毕业时只改这一行，其余字节原样保留）。
var suiteLine = regexp.MustCompile(`(?m)^suite:[ \t]*capability[ \t]*$`)

// cmdGraduate（P2 L1-T，路线 §6.2）：列出最近 3 次 nightly 在 capability 下全过的样本；
// --apply 时把 YAML 移到 regression/ 并改 suite 字段（不升 version、不做 git 操作）。
func cmdGraduate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("graduate", flag.ContinueOnError)
	dir := fs.String("suites-dir", defaultDir, "样本根目录（其下 capability/ 与 regression/）")
	cfgFile := fs.String("config", "configs/app.yaml", "配置文件（只连 agri_eval）")
	envFile := fs.String("env-file", "deploy/.env", "密钥文件（不存在则只读环境变量）")
	apply := fs.Bool("apply", false, "把可毕业样本移到 regression/ 并改 suite（默认只列出）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	samples, problems, err := sample.LoadDir(*dir)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("graduate: %d sample file(s) fail to parse, run validate first: %s", len(problems), problems[0].Path)
	}
	var caps []*sample.Sample
	var refs []store.SampleRef
	seen := make(map[string]string, len(samples))
	for _, s := range samples {
		// 同 id 的两个文件会让毕业 SQL 把两份 trial 混在一起判定，也会让 --apply 移出两份同 id 样本。
		if prev, dup := seen[s.ID]; dup {
			return fmt.Errorf("graduate: duplicate sample id %s in %s and %s, run validate first", s.ID, prev, s.Path)
		}
		seen[s.ID] = s.Path
		if s.Suite == suiteCapability {
			caps = append(caps, s)
			refs = append(refs, store.SampleRef{ID: s.ID, Version: s.Version})
		}
	}
	ids, err := queryGraduates(ctx, *cfgFile, *envFile, stderr, refs)
	if err != nil {
		return err
	}
	return graduate(stdout, *dir, caps, ids, *apply)
}

// queryGraduates 只打开 agri_eval 跑 §6.2 毕业 SQL；refs 为空时不连库。
func queryGraduates(ctx context.Context, cfgFile, envFile string, logW io.Writer, refs []store.SampleRef) (_ []string, err error) {
	if len(refs) == 0 {
		return nil, nil
	}
	cfg, err := config.Load(cfgFile, envFile)
	if err != nil {
		return nil, err
	}
	if err := cfg.RequireSecrets("mysql.password"); err != nil {
		return nil, err
	}
	logger, err := app.NewLogger(logW, cfg.Log.Level)
	if err != nil {
		return nil, err
	}
	db, err := app.OpenMySQL(ctx, cfg, cfg.MySQL.EvalDatabase, clock.System{}, logger)
	if err != nil {
		return nil, fmt.Errorf("eval db unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, repository.Close(db)) }()
	return store.New(db).Graduates(ctx, refs)
}

// graduate 打印可毕业清单；apply 时移动文件。caps 是 capability 样本，ids 是毕业 SQL 的结果。
// 移动前先算出全部目标路径并确认都不存在，避免移到一半才因路径冲突中止。
func graduate(w io.Writer, dir string, caps []*sample.Sample, ids []string, apply bool) error {
	fmt.Fprintf(w, "graduate: %d capability sample(s), %d eligible (last 3 nightly DONE, pass^%d each)\n", len(caps), len(ids), store.GraduateTrials)
	type move struct {
		s    *sample.Sample
		dest string
	}
	var moves []move
	for _, s := range caps {
		if !slices.Contains(ids, s.ID) {
			continue
		}
		dest, err := graduatePath(dir, s.Path)
		if err != nil {
			return err
		}
		switch _, err := os.Stat(dest); {
		case err == nil:
			return fmt.Errorf("graduate %s: destination %s already exists", s.ID, dest)
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("graduate %s: %w", s.ID, err)
		}
		moves = append(moves, move{s, dest})
	}
	for _, m := range moves {
		if !apply {
			fmt.Fprintf(w, "  %s@%d %s -> %s\n", m.s.ID, m.s.Version, m.s.Path, m.dest)
			continue
		}
		if err := moveToRegression(m.s.Path, m.dest); err != nil {
			return fmt.Errorf("graduate %s: %w", m.s.ID, err)
		}
		fmt.Fprintf(w, "  moved %s@%d -> %s\n", m.s.ID, m.s.Version, m.dest)
	}
	if !apply && len(ids) > 0 {
		fmt.Fprintln(w, "dry run: pass --apply to move them (no git operations; commit yourself)")
	}
	return nil
}

// graduatePath 把 {dir}/capability/<rel> 映射为 {dir}/regression/<rel>。
func graduatePath(dir, src string) (string, error) {
	rel, err := filepath.Rel(filepath.Join(dir, suiteCapability), src)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("sample %s is not under %s/%s", src, dir, suiteCapability)
	}
	return filepath.Join(dir, suiteRegression, rel), nil
}

// moveToRegression 改写唯一的 suite 行后以 O_EXCL 写到 dest（已存在则失败、不覆盖），再删除源文件。
func moveToRegression(src, dest string) (err error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if n := len(suiteLine.FindAllIndex(raw, -1)); n != 1 {
		return fmt.Errorf("%s: want exactly one %q line, found %d", src, "suite: capability", n)
	}
	out := suiteLine.ReplaceAll(raw, []byte("suite: "+suiteRegression))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(out); err != nil {
		return errors.Join(err, f.Close(), os.Remove(dest))
	}
	if err := f.Close(); err != nil {
		return errors.Join(err, os.Remove(dest))
	}
	// 源文件删不掉时撤回 dest，不让同一 id 同时留在 capability/ 与 regression/。
	if err := os.Remove(src); err != nil {
		return errors.Join(err, os.Remove(dest))
	}
	return nil
}
