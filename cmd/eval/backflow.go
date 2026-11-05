package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/piiclean"
)

// defaultBackflowDir 是 backflow-collect 写草稿的缺省目录（configs/app.yaml attribution.backflow.drafts_dir）。
const defaultBackflowDir = "eval/backflow/drafts"

// backflowDraftMaxBytes 是单份草稿的大小上限（草稿是一条 run 的桩，远小于此；超限多半不是草稿）。
const backflowDraftMaxBytes = 1 << 20

// backflowRefRe 是 draft_ref 的格式（与 internal/service/backflow 生成口径一致：bf- + 16 位小写字母，不含数字）。
var backflowRefRe = regexp.MustCompile(`^bf-[a-z]{16}$`)

// 草稿须保持 backflow-collect 写出的元数据（专家补写前不得改动）。
const (
	backflowSuite   = "capability"
	backflowPending = "pending_expert"
)

// ErrBackflowDraft 表示有草稿未通过入库前检查（格式或 pii_clean），本次一份都不入库。
var ErrBackflowDraft = errors.New("backflow import: drafts rejected")

// reviewInserter 是 review_task 写入（store.Reviews 实现；单测用假实现）。
type reviewInserter interface {
	Insert(ctx context.Context, tasks []store.ReviewTask, at time.Time) (int, error)
}

var errBackflowUsage = errors.New("usage: eval backflow import [--dir " + defaultBackflowDir + "] [store flags]")

// cmdBackflow 分派 backflow 子命令：import（草稿入评测库 review_task）。MERGED / DROPPED 由专家流程推进（待专家）。
func cmdBackflow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errBackflowUsage
	}
	if args[0] != "import" {
		return fmt.Errorf("%w: unknown subcommand %q", errBackflowUsage, args[0])
	}
	return cmdBackflowImport(ctx, args[1:], stdout, stderr, clock.System{})
}

// cmdBackflowImport 读 --dir 下的回流草稿，严格解码 + 再跑 pii_clean，全部通过后为每份草稿建
// review_task(BACKFLOW_EXPECT)（draft_ref 幂等）；有一份不过就整批拒绝，不入库。
func cmdBackflowImport(ctx context.Context, args []string, stdout, stderr io.Writer, clk clock.Clock) (err error) {
	fs := flag.NewFlagSet("backflow import", flag.ContinueOnError)
	var stf storeFlags
	stf.register(fs)
	dir := fs.String("dir", defaultBackflowDir, "回流草稿目录（{draft_ref}.yaml）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tasks, rejects, err := checkBackflowDrafts(*dir)
	if err != nil {
		return err
	}
	if len(rejects) > 0 {
		for _, r := range rejects {
			fmt.Fprintln(stderr, "backflow import:", r)
		}
		fmt.Fprintf(stdout, "backflow import: drafts=%d rejected=%d inserted=0\n", len(tasks)+len(rejects), len(rejects))
		return fmt.Errorf("%w: %d of %d", ErrBackflowDraft, len(rejects), len(tasks)+len(rejects))
	}
	es, err := stf.open(ctx, stderr)
	if err != nil {
		return fmt.Errorf("eval store unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, es.close()) }()
	inserted, err := insertBackflowTasks(ctx, store.NewReviews(es.db), tasks, clk.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "backflow import: drafts=%d rejected=0 inserted=%d\n", len(tasks), inserted)
	return nil
}

// insertBackflowTasks 写入 review_task（空批不碰库）。
func insertBackflowTasks(ctx context.Context, r reviewInserter, tasks []store.ReviewTask, at time.Time) (int, error) {
	if len(tasks) == 0 {
		return 0, nil
	}
	n, err := r.Insert(ctx, tasks, at)
	if err != nil {
		return 0, fmt.Errorf("backflow import: insert review tasks: %w", err)
	}
	return n, nil
}

// checkBackflowDrafts 检查 dir 下每份 *.yaml 草稿（跳过点文件，如 .gitkeep 与写到一半的临时文件），
// 返回通过者的任务（按文件名排序）与未通过者的原因。原因只含文件名、行号与类别，不回显命中的值。
func checkBackflowDrafts(dir string) ([]store.ReviewTask, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("backflow import: read dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); e.Type().IsRegular() && !strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".yaml") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var tasks []store.ReviewTask
	var rejects []string
	for _, name := range names {
		ref, reason, err := checkBackflowDraft(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		if reason != "" {
			rejects = append(rejects, name+": "+reason)
			continue
		}
		tasks = append(tasks, store.ReviewTask{TaskType: store.TaskBackflowExpect, DraftRef: ref})
	}
	return tasks, rejects, nil
}

// checkBackflowDraft 检查一份草稿：文件名即 draft_ref、格式合规、样本严格解码、元数据未改、逐行过 pii_clean。
// 未通过返回原因；只有读文件失败才返回 error。
func checkBackflowDraft(path string) (ref, reason string, err error) {
	raw, err := readCapped(path, backflowDraftMaxBytes)
	if errors.Is(err, errDraftTooLarge) {
		return "", "too large", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("backflow import: %w", err)
	}
	ref = strings.TrimSuffix(filepath.Base(path), ".yaml")
	if !backflowRefRe.MatchString(ref) {
		return "", "file name is not a draft_ref (bf- + 16 lowercase letters)", nil
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if l, ok := piiclean.Detect(line); ok {
			return "", "pii_clean hit (" + l.Kind + ") at line " + strconv.Itoa(i+1), nil
		}
	}
	s, err := sample.Parse(raw)
	if err != nil {
		return "", "not a sample: " + parseErrorReason(err), nil
	}
	switch {
	case s.ID != ref:
		return "", "id does not match file name", nil
	case s.Suite != backflowSuite || s.ReviewStatus != backflowPending:
		return "", "suite / review_status changed (want capability / pending_expert)", nil
	}
	return ref, "", nil
}

// yamlLineRe 取 yaml 错误里的行号（yaml.v3 的语法错误「yaml: line N: …」与解码错误「line N: …」同一写法）。
var yamlLineRe = regexp.MustCompile(`line (\d+)`)

// parseErrorReason 把样本解码错误归成「类别 at line N」。yaml.v3 的原始错误会带字段名与取值片段，可能含未脱敏原文，
// 不回显：只留类别与行号（多处解码错误取第一处）。
func parseErrorReason(err error) string {
	msg, category := err.Error(), "syntax error"
	var te *yaml.TypeError
	switch {
	case errors.As(err, &te) && len(te.Errors) > 0:
		msg = te.Errors[0]
		category = "invalid value"
		if strings.Contains(msg, "not found in type") {
			category = "unknown field"
		} else if strings.Contains(msg, "cannot unmarshal") {
			category = "type mismatch"
		}
	case strings.HasSuffix(msg, "empty sample file"):
		return "empty"
	}
	if m := yamlLineRe.FindStringSubmatch(msg); m != nil {
		return category + " at line " + m[1]
	}
	return category
}

var errDraftTooLarge = errors.New("draft too large")

// readCapped 读文件，超过 max 字节返回 errDraftTooLarge。
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open draft: %w", err)
	}
	b, rerr := io.ReadAll(io.LimitReader(f, max+1))
	if err := errors.Join(rerr, f.Close()); err != nil {
		return nil, fmt.Errorf("read draft: %w", err)
	}
	if int64(len(b)) > max {
		return nil, errDraftTooLarge
	}
	return b, nil
}
