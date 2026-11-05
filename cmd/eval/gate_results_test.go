package main

import (
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// 评审 P2：新 run 一律记录压缩数（含 0），与旧快照「没记录」（nil）区分；resume 在已记录的值上累加，
// 原快照没记录时保持 nil（只有补跑部分的计数不能当全实验的值）。
func TestRunResults_Compactions(t *testing.T) {
	compacted := evalkit.Trial{SampleID: "a", Detail: &service.RunDetail{
		Timeline: []repository.EventRow{{Type: model.EventCompactionApplied}}}}
	plain := evalkit.Trial{SampleID: "b"}
	if s := withRunResults(evalkit.Snapshot{}, []evalkit.Trial{plain}); s.Compactions == nil || *s.Compactions != 0 {
		t.Fatalf("fresh run without compaction = %v, want recorded 0", s.Compactions)
	}
	run := withRunResults(evalkit.Snapshot{}, []evalkit.Trial{compacted, plain})
	if run.Compactions == nil || *run.Compactions != 1 {
		t.Fatalf("fresh run compactions = %v, want 1", run.Compactions)
	}
	resumed := addRunResults(run, []evalkit.Trial{compacted})
	if *resumed.Compactions != 2 || *run.Compactions != 1 {
		t.Fatalf("resume compactions = %d (original now %d), want 2 / 1", *resumed.Compactions, *run.Compactions)
	}
	if legacy := addRunResults(evalkit.Snapshot{}, []evalkit.Trial{compacted}); legacy.Compactions != nil {
		t.Fatalf("legacy snapshot resume compactions = %d, want nil", *legacy.Compactions)
	}
}
