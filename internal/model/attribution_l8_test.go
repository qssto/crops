package model

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSignature_StableAndDistinct(t *testing.T) {
	// md5("DOSAGE|pest|kb:rice/pest/planthopper")，跨周同一问题同一签名
	a := Signature("DOSAGE", "pest", "kb:rice/pest/planthopper")
	if len(a) != 32 || a != Signature("DOSAGE", "pest", "kb:rice/pest/planthopper") {
		t.Fatalf("signature = %q", a)
	}
	for _, other := range []string{Signature("TIMING", "pest", "kb:rice/pest/planthopper"),
		Signature("DOSAGE", "disease", "kb:rice/pest/planthopper"), Signature("DOSAGE", "pest", "")} {
		if other == a {
			t.Fatalf("distinct inputs share signature %s", a)
		}
	}
}

func TestISOWeek(t *testing.T) {
	tests := []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), "2026W45"},
		{time.Date(2026, 11, 8, 23, 59, 0, 0, time.UTC), "2026W45"},
		{time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC), "2026W53"},
		{time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC), "2027W01"},
	}
	for _, tt := range tests {
		if got := ISOWeek(tt.at); got != tt.want || len(got) > 8 {
			t.Errorf("ISOWeek(%s) = %s, want %s", tt.at, got, tt.want)
		}
	}
	loc := time.FixedZone("CST", 8*3600)
	got := WeekStart(time.Date(2026, 11, 8, 23, 0, 0, 0, loc))
	if want := time.Date(2026, 11, 2, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("WeekStart = %s, want %s", got, want)
	}
}

// 应失败：证据超过 50 条、同一来源行重复、旧证据排在本周之前。
func TestMergeEvidence_CapsAndDedupes(t *testing.T) {
	var old []EvidenceRef
	for i := range 60 {
		old = append(old, EvidenceRef{RunID: uint64(i), Source: EvidenceRejection, SourceID: uint64(i), Week: "2026W44"})
	}
	fresh := []EvidenceRef{{RunID: 900, Source: EvidenceFeedback, SourceID: 1, Week: "2026W45"},
		{RunID: 3, Source: EvidenceRejection, SourceID: 3, Week: "2026W45"}}
	got := MergeEvidence(fresh, old)
	if len(got) != MaxEvidenceRefs {
		t.Fatalf("len = %d, want %d", len(got), MaxEvidenceRefs)
	}
	if got[0].SourceID != 1 || got[1].SourceID != 3 || got[1].Week != "2026W45" {
		t.Fatalf("fresh refs not first: %+v", got[:2])
	}
	seen := map[string]bool{}
	for _, r := range got {
		k := fmt.Sprintf("%s/%d", r.Source, r.SourceID)
		if seen[k] {
			t.Fatalf("duplicate ref %s", k)
		}
		seen[k] = true
	}
}

func TestAggregateHits(t *testing.T) {
	at := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	hits := []AttributionHit{
		{Reason: "DOSAGE", QuestionType: "pest", PrimaryKB: "kb:a", RunID: 1, Source: EvidenceRejection, SourceID: 10, At: at},
		{Reason: "DOSAGE", QuestionType: "pest", PrimaryKB: "kb:a", RunID: 2, Source: EvidenceFeedback, SourceID: 20, At: at.Add(time.Hour)},
		{Reason: AttrReasonKBMiss, QuestionType: "disease", RunID: 3, Source: EvidenceKBMiss, SourceID: 30, At: at},
	}
	got := AggregateHits("2026W45", hits)
	if len(got) != 2 || got[0].HitCount != 2 || got[0].Signature != Signature("DOSAGE", "pest", "kb:a") || got[1].HitCount != 1 {
		t.Fatalf("aggregate = %+v", got)
	}
	if got[0].Evidence[0].RunID != 2 || got[0].Evidence[0].Week != "2026W45" {
		t.Fatalf("evidence not newest first: %+v", got[0].Evidence)
	}
}

func TestAttributionPatch_Validate(t *testing.T) {
	s := func(v string) *string { return &v }
	u := func(v uint64) *uint64 { return &v }
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		p    AttributionPatch
		ok   bool
	}{
		{"full", AttributionPatch{Disposition: s(DispositionAddKB), OwnerID: u(7), ResolutionRef: s("kb PR #12"), Status: s(IssueResolved), RowVer: n(3)}, true},
		{"status only", AttributionPatch{Status: s(IssueInProgress), RowVer: n(0)}, true},
		{"no row_ver", AttributionPatch{Status: s(IssueOpen)}, false},
		{"negative row_ver", AttributionPatch{Status: s(IssueOpen), RowVer: n(-1)}, false},
		{"empty", AttributionPatch{RowVer: n(1)}, false},
		{"bad disposition", AttributionPatch{Disposition: s("REWRITE"), RowVer: n(1)}, false},
		{"bad status", AttributionPatch{Status: s("CLOSED"), RowVer: n(1)}, false},
		{"zero owner", AttributionPatch{OwnerID: u(0), RowVer: n(1)}, false},
		{"blank ref", AttributionPatch{ResolutionRef: s("  "), RowVer: n(1)}, false},
		{"long ref", AttributionPatch{ResolutionRef: s(strings.Repeat("字", MaxResolutionRef+1)), RowVer: n(1)}, false},
	}
	for _, tt := range tests {
		if err := tt.p.Validate(); (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}
