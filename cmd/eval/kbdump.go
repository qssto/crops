package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/sample"
)

// phenologyTool 是提供生育期的 MCP 工具名；kbdump 从其 fixture 取 stage。
const phenologyTool = "crop.phenology"

// cmdKBDump 按样本情境跑 progressive_loader，逐样本打印命中的 kb_id@version（或 KB_MISS），
// 并标出 must_cite 未被加载到的条目，末行给出 must_cite 召回汇总。
func cmdKBDump(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("kbdump", flag.ContinueOnError)
	var sf sampleFlags
	sf.register(fs)
	suites := fs.String("suite", "regression", "套件，逗号分隔")
	limit := fs.Int("limit", knowledge.DefaultLimit, "单次注入条目上限")
	if err := fs.Parse(args); err != nil {
		return err
	}
	all, tx, kb, err := sf.loadValidSamples()
	if err != nil {
		return err
	}
	selected := evalkit.Select(all, splitList(*suites), nil)
	if len(selected) == 0 {
		return fmt.Errorf("no sample matches suite=%s", *suites)
	}
	loader, err := knowledge.NewLoader(kb, knowledge.WithLimit(*limit))
	if err != nil {
		return err
	}
	var errs []error
	var mustTotal, mustHit int
	for _, s := range selected {
		sit := situationOf(tx, s)
		got, err := loader.Load(ctx, s.KnowledgeRef, tx, sit)
		head := fmt.Sprintf("%s [%s %s stage=%s target=%s]", s.ID, outcomeOf(s), topicOf(sit), orDash(sit.Stage), orDash(sit.Target))
		switch {
		case errors.Is(err, knowledge.ErrKBMiss):
			fmt.Fprintf(stdout, "%s -> %s\n", head, knowledge.RefuseReasonKBMiss)
		case err != nil:
			fmt.Fprintf(stdout, "%s -> ERROR %v\n", head, err)
			errs = append(errs, fmt.Errorf("%s: %w", s.ID, err))
			continue
		default:
			refs := make([]string, len(got))
			for i, e := range got {
				refs[i] = e.KBID + "@" + e.Version
			}
			fmt.Fprintf(stdout, "%s -> %s\n", head, strings.Join(refs, ", "))
		}
		if s.Expect.Advice == nil {
			continue
		}
		var missing []string
		for _, ref := range s.Expect.Advice.MustCite {
			mustTotal++
			if slices.ContainsFunc(got, func(e *knowledge.Entry) bool { return knowledge.CitationRef(e.KBID, e.Version) == ref }) {
				mustHit++
			} else {
				missing = append(missing, strings.TrimPrefix(ref, "kb:"))
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(stdout, "    must_cite not loaded: %s\n", strings.Join(missing, ", "))
		}
	}
	fmt.Fprintf(stdout, "kbdump: %d samples, must_cite loaded %d/%d\n", len(selected), mustHit, mustTotal)
	return errors.Join(errs...)
}

// situationOf 由样本推出加载情境：crop / region 取 taxonomy，stage 取 crop.phenology fixture，
// topic 取首个非 Ln 的 taxonomy tag（一级 code 则作 category），target 取 expect.advice.target，
// 缺省时取 must_cite 中带 target 的植保条目。
func situationOf(tx *knowledge.Taxonomy, s *sample.Sample) knowledge.Situation {
	sit := knowledge.Situation{Crop: tx.Crop, Region: tx.Region, Stage: stageOf(s)}
	for _, tag := range s.Tags {
		top, ok := tx.CategoryOf(tag)
		if !ok {
			continue // Ln 层号
		}
		if top == tag {
			sit.Category = tag
		} else {
			sit.Topic = tag
		}
		break
	}
	if a := s.Expect.Advice; a != nil {
		sit.Target = a.Target
		for _, ref := range a.MustCite {
			if sit.Target != "" {
				break
			}
			if id, ok := strings.CutPrefix(ref, "kb:rice/pest/"); ok {
				slug, _, _ := strings.Cut(id, "@")
				sit.Target = strings.ReplaceAll(slug, "-", "_")
			}
		}
	}
	return sit
}

func stageOf(s *sample.Sample) string {
	for _, f := range s.Fixtures.MCP[phenologyTool] {
		ret := f.Return
		if ret == nil && len(f.Returns) > 0 {
			ret = f.Returns[0]
		}
		if m, ok := ret.(map[string]any); ok {
			if st, ok := m["stage"].(string); ok {
				return st
			}
		}
	}
	return ""
}

func outcomeOf(s *sample.Sample) string {
	if s.IsRefusal() {
		return "REFUSED:" + s.Expect.RefuseReason
	}
	return string(s.Expect.Outcome)
}

func topicOf(sit knowledge.Situation) string {
	if sit.Topic != "" {
		return sit.Topic
	}
	return sit.Category
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
