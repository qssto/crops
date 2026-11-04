package store

import (
	"reflect"
	"strings"
	"testing"
)

func TestGraduateQuery(t *testing.T) {
	q, args := graduateQuery([]SampleRef{{"rice-cap-0001", 1}, {"rice-cap-0002", 3}})
	if !strings.Contains(q, "(t.sample_id, t.sample_version) IN ((?, ?), (?, ?))") {
		t.Errorf("query = %s", q)
	}
	for _, want := range []string{"trigger_kind = 'nightly' AND status = 'DONE'", "ORDER BY id DESC LIMIT 3",
		"t.suite = 'capability'",
		"HAVING COUNT(DISTINCT t.trial_index) >= ? AND SUM(t.passed = 0) = 0", "GROUP BY p.sample_id, p.sample_version\nHAVING COUNT(*) = 3"} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q", want)
		}
	}
	if want := []any{"rice-cap-0001", 1, "rice-cap-0002", 3, GraduateTrials}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}
