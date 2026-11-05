//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// itDraftRef 是本轮唯一的 draft_ref（隔离评测库跨轮次复用；bf- + 16 位小写字母）。
func itDraftRef() string {
	n := uint64(time.Now().UnixNano())
	b := make([]byte, 16)
	for i := range b {
		b[i] = 'a' + byte(n%26)
		n /= 26
	}
	return "bf-" + string(b)
}

// TestBackflowImport_Integration（幂等红线）：草稿入评测库建 review_task(BACKFLOW_EXPECT, draft_ref)，重跑不重复派单。
func TestBackflowImport_Integration(t *testing.T) {
	db, _ := evalDB(t)
	ref := itDraftRef()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ref+".yaml"), []byte(draftYAML(ref, "要不要打药？", "")), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"backflow", "import", "--dir", dir}, storeArgs()...)
	for i, want := range []string{"1", "0"} {
		stdout, stderr, code := cli(t, args...)
		if code != exitOK {
			t.Fatalf("run %d exit %d: %s", i, code, stderr)
		}
		if got := field(t, stdout, "inserted"); got != want {
			t.Fatalf("run %d inserted = %s, want %s", i, got, want)
		}
	}
	n := countRows(t, db, "SELECT COUNT(*) FROM review_task WHERE task_type = 'BACKFLOW_EXPECT' AND draft_ref = ? AND status = 'OPEN'"+
		" AND sample_id IS NULL AND experiment_id IS NULL", ref)
	if n != 1 {
		t.Fatalf("review_task rows = %s, want 1", strconv.FormatInt(n, 10))
	}
}
