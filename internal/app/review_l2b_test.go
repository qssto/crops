package app

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/config"
	"agri-steward/internal/controller"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// TestBuildReview：未启用 → 评审服务为 DisabledReviewService（kb_expert 得 ErrReviewUnavailable，路由照挂）；
// 启用缺依赖 / 未启用却传连接 → Build 失败（不静默降级）；齐全装配真实服务。
func TestBuildReview(t *testing.T) {
	on := config.DefaultEvalReview()
	on.Enabled = true
	full := EvalReviewDeps{Config: on, DB: &repository.DB{}, Objects: repository.QualityObjects{}, Bucket: "agri-steward-eval"}
	tests := []struct {
		name    string
		deps    EvalReviewDeps
		wantErr error
		enabled bool
	}{
		{name: "disabled by default", deps: EvalReviewDeps{}},
		{name: "disabled with db", deps: EvalReviewDeps{Config: config.DefaultEvalReview(), DB: &repository.DB{}}, wantErr: ErrEvalReviewDeps},
		{name: "enabled no db", deps: EvalReviewDeps{Config: on, Objects: full.Objects, Bucket: full.Bucket}, wantErr: ErrEvalReviewDeps},
		{name: "enabled no bucket", deps: EvalReviewDeps{Config: on, DB: full.DB, Objects: full.Objects}, wantErr: ErrEvalReviewDeps},
		{name: "enabled", deps: full, enabled: true},
	}
	expert := model.Scope{UserID: 9002, Roles: []model.Role{model.RoleKBExpert}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fullDeps(t)
			d.EvalReview = tt.deps
			a, err := Build(d)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if a.Review == nil || len(a.reviewAdminMounts(controller.Guard{})) != 1 {
				t.Fatalf("review = %v, mounts must always include review routes", a.Review)
			}
			if tt.enabled { // 假连接不可查询，只验装配
				return
			}
			if _, err := a.Review.List(t.Context(), expert, service.ReviewListQuery{}); !errors.Is(err, service.ErrReviewUnavailable) {
				t.Fatalf("disabled list err = %v, want ErrReviewUnavailable", err)
			}
		})
	}
}

// TestOpenEvalReview：未启用不连库（nil, nil）；启用但账号 / 口令未从环境变量取到 → 失败（不拨号、不回落到业务库账号）。
func TestOpenEvalReview(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{EvalReview: config.DefaultEvalReview()}
	if db, err := OpenEvalReview(t.Context(), cfg, qualityClock{}, log); db != nil || err != nil {
		t.Fatalf("disabled = %v, %v, want nil, nil", db, err)
	}
	cfg.EvalReview.Enabled = true
	cfg.MySQL.User, cfg.MySQL.Password = "app", "app-secret"
	db, err := OpenEvalReview(t.Context(), cfg, qualityClock{}, log)
	if db != nil || err == nil || !strings.Contains(err.Error(), "eval_review.user") ||
		!strings.Contains(err.Error(), "eval_review.password") {
		t.Fatalf("no secrets = %v, %v, want missing eval_review.user / password", db, err)
	}
}

// TestReviewLoops：P2-L2b 已登记；未启用不起任务；启用缺依赖 worker 启动失败；齐全起 online-judge-review-enqueue。
func TestReviewLoops(t *testing.T) {
	if !slices.ContainsFunc(p2Loops, func(e p2LoopEntry) bool { return e.name == "P2-L2b" }) {
		t.Fatal("P2-L2b not registered")
	}
	if loops, err := reviewLoops(&App{}); err != nil || loops != nil {
		t.Fatalf("disabled = %v, %v, want nil, nil", loops, err)
	}
	on := config.DefaultEvalReview()
	on.Enabled = true
	a := &App{DB: &repository.DB{}, Locker: repository.NewLocker(nil), Keys: testKeys(t), Clock: qualityClock{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), evalReview: EvalReviewDeps{Config: on}}
	if _, err := reviewLoops(a); !errors.Is(err, ErrEvalReviewDeps) {
		t.Fatalf("no eval db err = %v, want ErrEvalReviewDeps", err)
	}
	a.evalReview.DB = &repository.DB{}
	if _, err := reviewLoops(a); !errors.Is(err, ErrEvalReviewDeps) {
		t.Fatalf("no redis (enqueue cursor) err = %v, want ErrEvalReviewDeps", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"}) // 不建连，装配不做 IO
	t.Cleanup(func() {
		if err := rdb.Close(); err != nil {
			t.Errorf("close redis: %v", err)
		}
	})
	a.Redis = rdb
	loops, err := reviewLoops(a)
	if err != nil || len(loops) != 1 || loops[0].Name != service.ReviewEnqueueJob || loops[0].Run == nil {
		t.Fatalf("enabled = %v, %v", loops, err)
	}
	var _ service.ReviewObjects = repository.QualityObjects{}
	var _ service.ReviewStore = repository.EvalReviewRepo{}
	var _ service.JudgeWriteback = repository.QualityRepo{}
	var _ service.ReviewEnqueueSource = repository.QualityRepo{}
	var _ service.ReviewEnqueueSink = repository.EvalReviewRepo{}
	var _ service.ReviewEnqueueCursorStore = repository.ReviewEnqueueCursor{}
}
