package app

import (
	"errors"
	"testing"
	"time"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/config"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
	"agri-steward/internal/service/quality"
)

type qualityClock struct{}

func (qualityClock) Now() time.Time { return time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC) }

func qualityApp(t *testing.T, d QualityDeps) *App {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	return &App{Keys: keys, Clock: qualityClock{}, Locker: &repository.Locker{}, quality: d}
}

func enabledQuality() QualityDeps {
	q := config.Quality{Enabled: true, Interval: time.Hour, SamplePerCategory: 5, SampleSalt: "p2l3", JudgeTier: "standard",
		JudgeQPM: 6, JudgesDir: "eval/judges", BaselineName: "baseline_p1.r2", ReportTimezone: "Asia/Shanghai"}
	return QualityDeps{Config: q, Model: &config.Model{Tiers: map[string]config.ModelTier{"standard": {RatePerMin: 60}}},
		Objects: repository.QualityObjects{}, Bucket: "agri-steward-eval", KB: knowledge.NewStore(nil), Repo: "../.."}
}

// 缺省不启用：不起任务（行为不变）；启用后缺依赖或档位不存在即 worker 启动失败；依赖齐全起 quality-daily。
func TestBuildQualityLoops(t *testing.T) {
	if loops, err := buildQualityLoops(qualityApp(t, QualityDeps{})); err != nil || loops != nil {
		t.Fatalf("disabled = %v, %v, want nil, nil", loops, err)
	}
	noObj := enabledQuality()
	noObj.Objects = nil
	badTier := enabledQuality()
	badTier.Config.JudgeTier = "premium"
	for name, d := range map[string]QualityDeps{"no objects": noObj, "unknown tier": badTier} {
		if _, err := buildQualityLoops(qualityApp(t, d)); !errors.Is(err, ErrQualityDeps) {
			t.Fatalf("%s err = %v, want ErrQualityDeps", name, err)
		}
	}
	loops, err := buildQualityLoops(qualityApp(t, enabledQuality()))
	if err != nil || len(loops) != 1 || loops[0].Name != LoopQualityDaily || loops[0].Run == nil {
		t.Fatalf("enabled = %v, %v", loops, err)
	}
	var _ quality.Objects = repository.QualityObjects{}
}
