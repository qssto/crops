package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agri-steward/internal/evalkit/sample/coverage"
)

// defaultTargets 是覆盖目标文件（P2 L1-T）。
const defaultTargets = "eval/coverage/targets.yaml"

// errCoverageShort 是 --strict-coverage 下覆盖不足的错误。
var errCoverageShort = errors.New("coverage below target")

// coverageFlags 是 validate 的覆盖报表参数：覆盖不足默认只报告，--strict-coverage 时才让 validate 失败。
type coverageFlags struct {
	targets string
	strict  bool
	color   string
}

func (f *coverageFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.targets, "coverage-targets", "", "分层覆盖目标文件（默认 --repo 下 "+defaultTargets+"）")
	fs.BoolVar(&f.strict, "strict-coverage", false, "覆盖未达标（总数 / 格子 / 拒答 / 陷阱）时 validate 退出码为 1")
	fs.StringVar(&f.color, "color", "auto", "低于目标标红：auto（终端时）| always | never")
}

// printCoverage 在静态校验通过后输出覆盖报表与补写清单；strict 且有未达标项时返回错误。
func (f *coverageFlags) printCoverage(stdout io.Writer, ls *loadedSamples, sf sampleFlags) error {
	path := f.targets
	if path == "" {
		path = filepath.Join(sf.repo, defaultTargets)
	}
	color, err := wantColor(f.color, stdout)
	if err != nil {
		return err
	}
	tg, err := coverage.LoadTargets(path, ls.tx)
	if err != nil {
		return err
	}
	r, err := coverage.Count(ls.samples, ls.tx, tg, sf.dir)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout)
	if err := coverage.Render(stdout, r, ls.tx, coverage.RenderOptions{Color: color}); err != nil {
		return fmt.Errorf("render coverage: %w", err)
	}
	short := r.Shortfalls(ls.tx)
	if !f.strict || len(short) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", errCoverageShort, strings.Join(short, "; "))
}

func wantColor(mode string, w io.Writer) (bool, error) {
	switch mode {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "auto":
		f, ok := w.(*os.File)
		if !ok {
			return false, nil
		}
		fi, err := f.Stat()
		if err != nil {
			return false, fmt.Errorf("stat stdout: %w", err)
		}
		return fi.Mode()&os.ModeCharDevice != 0, nil
	default:
		return false, fmt.Errorf("--color %q: want auto, always or never", mode)
	}
}
