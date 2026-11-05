package main

import (
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/gate"
)

// withRunResults 记录新 run 的压缩触发数与未声明调用：压缩数一律写入（含 0），与旧快照的「没记录」（nil）区分。
func withRunResults(snap evalkit.Snapshot, trials []evalkit.Trial) evalkit.Snapshot {
	snap.Compactions = new(int)
	return addRunResults(snap, trials)
}

// addRunResults 把 trials 的压缩触发数与未声明调用累加进快照（resume 在原快照上累加补跑部分）。原快照没记录压缩数
// （L7-b 之前的实验）时保持 nil：只有补跑部分的计数不是全实验的值，不能当作已记录。
func addRunResults(snap evalkit.Snapshot, trials []evalkit.Trial) evalkit.Snapshot {
	n, calls := gate.RunResults(trials)
	if snap.Compactions != nil {
		total := *snap.Compactions + n
		snap.Compactions = &total
	}
	snap.UnexpectedCalls = append(snap.UnexpectedCalls, calls...)
	return snap
}
