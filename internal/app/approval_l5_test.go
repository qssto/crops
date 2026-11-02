package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/config"
)

// nopObjects 是 ObjectGetter 桩（装配测试不读对象）。
type nopObjects struct{}

func (nopObjects) GetObject(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("nop")
}

// P1 L5 装配：approval 段零值取默认、非法即 Build 失败；HITL.Objects 为空不装详情；
// 应失败：未注入通知文案时 worker 的 outbox 处理器表构造失败（不回退占位处理器）。
func TestBuild_ApprovalL5(t *testing.T) {
	tests := []struct {
		name       string
		cfg        config.Approval
		objects    bool
		want       config.Approval
		wantDetail bool
		wantErr    bool
	}{
		{name: "zero takes default", want: config.DefaultApproval()},
		{name: "custom kept, detail with objects", cfg: config.Approval{RejectionExportMaxSpan: 24 * time.Hour}, objects: true,
			want: config.Approval{RejectionExportMaxSpan: 24 * time.Hour}, wantDetail: true},
		{name: "negative span fails", cfg: config.Approval{RejectionExportMaxSpan: -time.Hour}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fullDeps(t)
			d.Approval = tt.cfg
			if tt.objects {
				d.HITL.Objects = nopObjects{}
			}
			a, err := Build(d)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if a.L5.ApprovalConfig != tt.want || a.L5.Rejections == nil || (a.L5.Detail != nil) != tt.wantDetail {
				t.Fatalf("L5 = %+v", a.L5)
			}
			if got := len(a.approvalL5Mounts()); got != map[bool]int{false: 1, true: 2}[tt.wantDetail] {
				t.Fatalf("mounts = %d", got)
			}
		})
	}
	d := fullDeps(t)
	d.NotifyTexts = nil
	a, err := Build(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.OutboxHandlers(); !errors.Is(err, ErrMissingNotifyTexts) {
		t.Fatalf("OutboxHandlers err = %v, want ErrMissingNotifyTexts", err)
	}
	if _, err := a.WorkerLoops(); !errors.Is(err, ErrMissingNotifyTexts) {
		t.Fatalf("WorkerLoops err = %v", err)
	}
}
