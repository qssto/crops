package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/model"
)

// TestDispatchAttempt_RejectsBadState：SetJobTx / UpdateReceiptTx 在入口校验状态枚举与 FAILED 的 failure_kind，
// 不合规返回 model.ErrBadReceipt；校验先于事务检查与 SQL（tx 传 nil：合规输入才走到 ErrNotInTx，证明坏输入未触库）。
func TestDispatchAttempt_RejectsBadState(t *testing.T) {
	ctx := context.Background()
	repo := DispatchAttemptRepo{}
	ref := model.OrderRef{TenantID: 1, FarmID: 1, OrderID: 9}
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	neg := -1.0
	update := []struct {
		name   string
		status model.AttemptStatus
		kind   model.FailureKind
		area   *float64
	}{
		{"unknown status", "RUNNING", "", nil},
		{"empty status", "", "", nil},
		{"SENT is insert-only", model.AttemptSent, "", nil},
		{"FAILED without kind", model.AttemptFailed, "", nil},
		{"FAILED unknown kind", model.AttemptFailed, "MID_AIR", nil},
		{"kind on DONE", model.AttemptDone, model.FailureUnknown, nil},
		{"negative area", model.AttemptPartiallyDone, "", &neg},
	}
	for _, tc := range update {
		t.Run("update/"+tc.name, func(t *testing.T) {
			err := repo.UpdateReceiptTx(ctx, nil, ref, 1, 0, tc.status, tc.kind, tc.area, now, now)
			if !errors.Is(err, model.ErrBadReceipt) || errors.Is(err, ErrNotInTx) {
				t.Fatalf("err = %v, want ErrBadReceipt before any DB access", err)
			}
		})
	}
	setJob := map[string]model.DeviceReceipt{
		"unknown status":      {Status: "RUNNING", DeviceJobID: "sim-1"},
		"FAILED without kind": {Status: model.AttemptFailed, DeviceJobID: "sim-1"},
		"kind on ACCEPTED":    {Status: model.AttemptAccepted, DeviceJobID: "sim-1", FailureKind: model.FailureNotExecuted},
		"local NO_RECEIPT":    {Status: model.AttemptNoReceipt, DeviceJobID: "sim-1"},
	}
	for name, rc := range setJob {
		t.Run("set_job/"+name, func(t *testing.T) {
			err := repo.SetJobTx(ctx, nil, ref, 1, 0, rc, now, now)
			if !errors.Is(err, model.ErrBadReceipt) || errors.Is(err, ErrNotInTx) {
				t.Fatalf("err = %v, want ErrBadReceipt before any DB access", err)
			}
		})
	}
	// 合规输入通过校验，才轮到事务检查。
	if err := repo.UpdateReceiptTx(ctx, nil, ref, 1, 0, model.AttemptFailed, model.FailureUnknown, nil, now, now); !errors.Is(err, ErrNotInTx) {
		t.Fatalf("valid update err = %v, want ErrNotInTx", err)
	}
	ok := model.DeviceReceipt{Status: model.AttemptAccepted, DeviceJobID: "sim-1"}
	if err := repo.SetJobTx(ctx, nil, ref, 1, 0, ok, now, now); !errors.Is(err, ErrNotInTx) {
		t.Fatalf("valid set job err = %v, want ErrNotInTx", err)
	}
}
