package knowledge

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// validate 做单条目校验（README §2 / §5）；与 index.yaml 的一致性与 taxonomy 校验在快照层做。
func (e *Entry) validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	need := func(ok bool, field string) {
		if !ok {
			fail("%s is required", field)
		}
	}
	need(e.KBID != "", "kb_id")
	need(strings.TrimSpace(e.Title) != "", "title")
	need(e.Crop != "", "crop")
	need(e.Region != "", "region")
	need(len(e.Stage) > 0, "stage")
	need(len(e.Category) > 0, "category")
	need(len(e.Topics) > 0, "topics")
	need(e.Version != "", "version")
	need(strings.TrimSpace(e.Author) != "", "author")
	need(e.EffectiveFrom != "", "effective_from")
	need(len(e.Basis) > 0, "basis")

	if e.KBID != "" {
		file, err := FileOf(e.KBID)
		switch {
		case err != nil:
			errs = append(errs, err)
		case file != e.File:
			fail("kb_id %q belongs in %s, found in %s", e.KBID, file, e.File)
		}
		if strings.Split(e.KBID, "/")[1] == "pest" && e.Target == "" {
			fail("target is required for pest entries")
		}
	}
	if e.Version != "" && !versionPattern.MatchString(e.Version) {
		fail("version %q must be v<n>", e.Version)
	}
	if e.AuthorityLevel.Rank() == 0 {
		fail("authority_level %q not in [国标 地标 企业标准 专家经验]", e.AuthorityLevel)
	}
	switch {
	case e.ReviewStatus != reviewPendingExpert && e.ReviewStatus != reviewSigned:
		fail("review_status %q not in [pending_expert signed]", e.ReviewStatus)
	case e.Author == authorClaudeDraft && e.ReviewStatus != reviewPendingExpert:
		fail("author claude-draft must stay pending_expert")
	}
	if e.EffectiveFrom != "" {
		if _, err := time.Parse(time.DateOnly, e.EffectiveFrom); err != nil {
			fail("effective_from %q must be YYYY-MM-DD", e.EffectiveFrom)
		}
	}
	if err := checkStages(e.Stage, true); err != nil {
		fail("stage: %v", err)
	}
	if dup := firstDup(e.Category); dup != "" {
		fail("duplicate category %q", dup)
	}
	for _, t := range e.Topics {
		if !slices.ContainsFunc(e.Category, func(c string) bool { return strings.HasPrefix(t, c+".") }) {
			fail("topic %q does not belong to category %v", t, e.Category)
		}
	}
	for i, b := range e.Basis {
		if strings.TrimSpace(b.Source) == "" || b.Level.Rank() == 0 {
			fail("basis[%d] needs source and a valid level", i)
		}
	}
	errs = append(errs, e.validateThresholds()...)
	errs = append(errs, e.validateParams()...)
	errs = append(errs, e.validateFormulas()...)
	return errors.Join(errs...)
}

func (e *Entry) validateThresholds() []error {
	var errs []error
	seen := map[string]bool{}
	for i, t := range e.Thresholds {
		switch {
		case t.ID == "" || t.Metric == "" || t.Unit == "":
			errs = append(errs, fmt.Errorf("thresholds[%d] needs id, metric and unit", i))
		case seen[t.ID]:
			errs = append(errs, fmt.Errorf("thresholds: duplicate id %q", t.ID))
		case !knownOps[t.Op]:
			errs = append(errs, fmt.Errorf("thresholds %s: op %q not in [>= > <= < ==]", t.ID, t.Op))
		}
		if err := checkStages(t.Stage, false); err != nil {
			errs = append(errs, fmt.Errorf("thresholds %s: stage: %w", t.ID, err))
		}
		seen[t.ID] = true
	}
	return errs
}

func (e *Entry) validateParams() []error {
	var errs []error
	seen := map[string]bool{}
	for i, p := range e.Params {
		key := p.ID + "|" + fmt.Sprint(p.When)
		switch {
		case p.ID == "" || p.Unit == "":
			errs = append(errs, fmt.Errorf("params[%d] needs id and unit", i))
		case seen[key]:
			errs = append(errs, fmt.Errorf("params: duplicate id+when %s %v", p.ID, p.When))
		case p.Value == nil && p.Min == nil && p.Max == nil:
			errs = append(errs, fmt.Errorf("params %s: needs value or min/max", p.ID))
		case p.Min != nil && p.Max != nil && *p.Min > *p.Max:
			errs = append(errs, fmt.Errorf("params %s: min > max", p.ID))
		case p.Value != nil && ((p.Min != nil && *p.Value < *p.Min) || (p.Max != nil && *p.Value > *p.Max)):
			errs = append(errs, fmt.Errorf("params %s: value outside [min, max]", p.ID))
		}
		seen[key] = true
	}
	return errs
}

func (e *Entry) validateFormulas() []error {
	var errs []error
	seen := map[string]bool{}
	for i, f := range e.Formulas {
		switch {
		case f.ID == "" || f.Expr == "" || len(f.Inputs) == 0 || f.Output.Name == "":
			errs = append(errs, fmt.Errorf("formulas[%d] needs id, expr, inputs and output.name", i))
		case seen[f.ID]:
			errs = append(errs, fmt.Errorf("formulas: duplicate id %q", f.ID))
		case !knownCalcs[f.Calc]:
			errs = append(errs, fmt.Errorf("formulas %s: calc %q not in [Dosage Dilution PHIDate Fertilizer WaterVolume]", f.ID, f.Calc))
		}
		seen[f.ID] = true
	}
	return errs
}

// checkStages：取值须在 Stages 枚举内；allowWildcard 时可为单独的 "*"。
func checkStages(stages []string, allowWildcard bool) error {
	for _, s := range stages {
		if s == Wildcard {
			if !allowWildcard || len(stages) != 1 {
				return fmt.Errorf("%q must appear alone", Wildcard)
			}
			continue
		}
		if !slices.Contains(Stages, s) {
			return fmt.Errorf("%q not in %v", s, Stages)
		}
	}
	if dup := firstDup(stages); dup != "" {
		return fmt.Errorf("duplicate %q", dup)
	}
	return nil
}

func firstDup(items []string) string {
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if seen[it] {
			return it
		}
		seen[it] = true
	}
	return ""
}
