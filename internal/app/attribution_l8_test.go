package app

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"agri-steward/internal/config"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
	"agri-steward/internal/service/attribution"
	"agri-steward/internal/service/backflow"
)

// TestAttributionLoops：P2-L8 已登记；未启用不起任务；启用缺依赖或配置非法 worker 启动失败；齐全起 attribution-weekly。
func TestAttributionLoops(t *testing.T) {
	if !slices.ContainsFunc(p2Loops, func(e p2LoopEntry) bool { return e.name == "P2-L8" }) {
		t.Fatal("P2-L8 not registered")
	}
	if loops, err := attributionLoops(&App{}); err != nil || loops != nil {
		t.Fatalf("disabled = %v, %v, want nil, nil", loops, err)
	}
	on := config.DefaultAttribution()
	on.Enabled = true
	a := &App{DB: &repository.DB{}, Locker: repository.NewLocker(nil), Keys: testKeys(t), Clock: qualityClock{},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		attribution: AttributionDeps{Config: on, Objects: repository.QualityObjects{}}}
	if _, err := attributionLoops(a); !errors.Is(err, ErrAttributionDeps) {
		t.Fatalf("no bucket err = %v, want ErrAttributionDeps", err)
	}
	a.attribution.Bucket = "agri-steward-eval"
	loops, err := attributionLoops(a)
	if err != nil || len(loops) != 1 || loops[0].Name != attribution.JobWeekly || loops[0].Run == nil {
		t.Fatalf("enabled = %v, %v", loops, err)
	}
	a.attribution.Config.TopN = 0
	if _, err := attributionLoops(a); !errors.Is(err, config.ErrBadAttribution) {
		t.Fatalf("bad config err = %v, want ErrBadAttribution", err)
	}
	var _ attribution.Objects = repository.QualityObjects{}
}

// TestBackflowLoops：回流单独开关；缺原文读取（a.Audit）启动失败；齐全起 backflow-collect；与周报同开起两个任务。
func TestBackflowLoops(t *testing.T) {
	cfg := config.DefaultAttribution()
	cfg.Backflow.Enabled = true
	a := &App{DB: &repository.DB{}, Locker: repository.NewLocker(nil), Keys: testKeys(t), Clock: qualityClock{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), attribution: AttributionDeps{Config: cfg}}
	if _, err := attributionLoops(a); !errors.Is(err, ErrAttributionDeps) {
		t.Fatalf("no audit err = %v, want ErrAttributionDeps", err)
	}
	a.Audit = &service.AuditService{}
	loops, err := attributionLoops(a)
	if err != nil || len(loops) != 1 || loops[0].Name != backflow.JobCollect || loops[0].Run == nil {
		t.Fatalf("backflow only = %v, %v", loops, err)
	}
	a.attribution.Config.Enabled = true
	a.attribution.Objects, a.attribution.Bucket = repository.QualityObjects{}, "agri-steward-eval"
	if loops, err := attributionLoops(a); err != nil || len(loops) != 2 {
		t.Fatalf("both = %v, %v", loops, err)
	}
	a.attribution.Config.Backflow.MaxRuns = 0
	if _, err := attributionLoops(a); !errors.Is(err, config.ErrBadAttribution) {
		t.Fatalf("bad backflow config err = %v, want ErrBadAttribution", err)
	}
}
