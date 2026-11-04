package evalkit

import (
	"path/filepath"
	"regexp"
	"testing"

	"agri-steward/internal/evalkit/sample"
)

// P2 L1-T 回填只加元数据（crop / stage / 陷阱 tag / synthetic_pii）、不升 version：
// 把仓库样本原文里的这些元数据剥掉再解析，SamplesHash 必须与原样加载完全一致（逐套件、全量）。
func TestSamplesHash_IgnoresStrataMetadata(t *testing.T) {
	samples, probs, err := sample.LoadDir(filepath.Join("..", "..", "eval", "suites"))
	if err != nil || len(probs) > 0 {
		t.Fatalf("load: %v %v", err, probs)
	}
	metaLine := regexp.MustCompile(`(?m)^(crop|stage|synthetic_pii):.*\n`)
	trapTag := regexp.MustCompile(`, trap\.[a-z_]+`)
	stripped := make([]*sample.Sample, 0, len(samples))
	backfilled := 0
	for _, s := range samples {
		raw := trapTag.ReplaceAll(metaLine.ReplaceAll(s.Raw, nil), nil)
		if len(raw) != len(s.Raw) {
			backfilled++
		}
		o, err := sample.Parse(raw)
		if err != nil {
			t.Fatalf("%s: parse stripped: %v", s.Path, err)
		}
		if o.Crop != "" || o.Stage != "" || len(sample.TrapTags(o)) > 0 {
			t.Fatalf("%s: metadata not stripped", s.Path)
		}
		stripped = append(stripped, o)
	}
	if backfilled != len(samples) {
		t.Errorf("backfilled %d of %d samples, want all", backfilled, len(samples))
	}
	if got, want := SamplesHash(stripped), SamplesHash(samples); got != want {
		t.Errorf("all samples: stripped hash %s != %s", got, want)
	}
	for _, suites := range [][]string{{"regression"}, {"redteam"}, {"constraint"}, {"regression", "redteam"}} {
		if got, want := SamplesHash(Select(stripped, suites, nil)), SamplesHash(Select(samples, suites, nil)); got != want {
			t.Errorf("%v: stripped hash %s != %s", suites, got, want)
		}
	}
}
