package constraint

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseRegulationSignoff(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		missing []string
		bad     bool
	}{
		{"signed", "signoff:\n  legal_signer: a\n  legal_signed_at: \"2026-10-01\"\n  record_ref: r\n", nil, false},
		{"blank", "signoff:\n  legal_signer: \" \"\n  legal_signed_at: \"\"\n  record_ref: r\n",
			[]string{"legal_signer", "legal_signed_at"}, false},
		{"header missing", "version: 1\n", nil, true},
		{"unparsable", "signoff: [\n", nil, true},
		// 应失败：拼错的签字字段不能被静默忽略（与启动 ParseRegulationMap 的 KnownFields 一致）
		{"typo field", "signoff:\n  legal_signer: a\n  legal_signed_on: \"2026-10-01\"\n  record_ref: r\n", nil, true},
		{"not a mapping", "signoff: signed\n", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseRegulationSignoff([]byte(tc.yaml))
			if tc.bad {
				if !errors.Is(err, ErrBadRegulationMap) {
					t.Fatalf("err = %v, want ErrBadRegulationMap", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Missing(); !slices.Equal(got, tc.missing) {
				t.Fatalf("Missing() = %v, want %v", got, tc.missing)
			}
		})
	}
}

// TestRegulationSignoff_RepoUnsigned：仓库内法规对照表「待签字」，三项都留空，工程侧不代填。
func TestRegulationSignoff_RepoUnsigned(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "policy", "regulation_map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseRegulationSignoff(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Missing(); len(got) != 3 {
		t.Fatalf("Missing() = %v, want all three blank (待签字)", got)
	}
}
