package quality

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/service/quality/judge"
)

// online-judge-sample（§6.4 技术实现 3）：对前一日 outcome=ADVICE 的卡按 category 定额抽样，先取生产 rl:model:{tier}
// 令牌、再取 rl:judge:{tier} 令牌，调 judge 后写 online_judge_result；UNKNOWN 与 1 分置 NEED_EXPERT。不写 run_event。
const (
	// maxTokenWaits 是一张卡取不到令牌时的最多等待次数（每次等一个令牌的补充间隔），之后当日余下的卡记「未评」。
	maxTokenWaits = 3
	expertScore   = 1 // 1 分推专家巡检
)

// Sampler 是单农场一日的线上 judge 抽样。
type Sampler struct {
	Store    SampleStore
	Cards    CardReader
	KB       KnowledgeOpener
	Judge    judge.Client
	Prompt   judge.Prompt
	Version  string // judge.Version(judges_dir, fact_consistency, tier)
	Tokens   TokenTaker
	ModelKey string // as:{env}:rl:model:{tier}
	JudgeKey string // as:{env}:rl:judge:{tier}
	ModelQPM int    // 生产档位 rate_per_min（0 不限）
	JudgeQPM int
	Quota    int // 每个 category 抽几张
	Salt     string
	Clock    clock.Clock
	Logger   *slog.Logger
	// Wait 等待 d（取不到令牌时）；nil 用定时器。测试注入以免真睡。
	Wait func(ctx context.Context, d time.Duration) error
}

// SampleResult 是单农场一日的抽样结果：Judged < Sampled 表示有卡没评上（judge 不可用或限流），当日事实一致率「未评」。
type SampleResult struct {
	Sampled int
	Judged  int
}

// Unjudged 报告是否有抽中的卡没有结果。
func (r SampleResult) Unjudged() bool { return r.Judged < r.Sampled }

// errJudgeDown 表示 judge 不可用（模型错误或令牌耗尽）：余下的卡不再调用。
var errJudgeDown = errors.New("quality: judge unavailable")

// Sample 抽样并评分；同 (advice, 维度, judge_version) 已有结果的卡不重复评（补跑幂等）。renew 在每张卡后续期任务锁。
// judge 不可用只记未评、不返回错误；读库、读知识失败返回错误（下个周期重试）。
func (s *Sampler) Sample(ctx context.Context, scope model.Scope, day Day, renew func(context.Context) error) (SampleResult, error) {
	var res SampleResult
	done, err := s.judged(ctx, scope, day.Date)
	if err != nil {
		return res, err
	}
	cats, err := s.Store.AdviceCategories(ctx, scope, day.From, day.To)
	if err != nil {
		return res, err
	}
	down := false
	for _, cat := range cats {
		refs, err := s.Store.SampleAdvice(ctx, scope, cat, day.From, day.To, s.Salt, s.Quota)
		if err != nil {
			return res, err
		}
		for _, ref := range refs {
			res.Sampled++
			if done[ref.ID] {
				res.Judged++
				continue
			}
			if down {
				continue
			}
			err := s.judgeOne(ctx, scope, day.Date, cat, ref.RunID)
			switch {
			case errors.Is(err, errJudgeDown):
				s.Logger.WarnContext(ctx, "online judge unavailable, rest of day unjudged",
					"farm_id", scope.FarmID, "date", day.Date, "err", err)
				down = true
				continue
			case err != nil:
				return res, err
			}
			res.Judged++
			if renew != nil {
				if err := renew(ctx); err != nil {
					return res, fmt.Errorf("quality: renew sample lock: %w", err)
				}
			}
		}
	}
	return res, nil
}

// judged 是该日已有当前 judge 版本结果的 advice_id。
func (s *Sampler) judged(ctx context.Context, scope model.Scope, date string) (map[uint64]bool, error) {
	rows, err := s.Store.JudgeResults(ctx, scope, date, judge.DimFactConsistency)
	if err != nil {
		return nil, err
	}
	done := make(map[uint64]bool, len(rows))
	for _, r := range rows {
		if r.JudgeVersion == s.Version {
			done[r.AdviceID] = true
		}
	}
	return done, nil
}

func (s *Sampler) judgeOne(ctx context.Context, scope model.Scope, date, cat string, runID uint64) error {
	card, err := s.Cards.GetByRun(ctx, scope, runID)
	if err != nil {
		return err
	}
	m, err := s.material(ctx, scope, card)
	if err != nil {
		return err
	}
	if err := s.take(ctx); err != nil {
		return err
	}
	v, err := s.Judge.Judge(ctx, s.Prompt, m)
	if err != nil {
		return fmt.Errorf("%w: %w", errJudgeDown, err)
	}
	score := v.Score
	if score != nil {
		if verr := judge.Validate(v, m.EvaluatedText()); verr != nil {
			s.Logger.InfoContext(ctx, "online judge verdict rejected, counted UNKNOWN", "run_id", runID, "err", verr)
			score = nil
		}
	}
	status := model.JudgeStatusScored
	if score == nil || *score == expertScore {
		status = model.JudgeStatusNeedExpert
	}
	_, err = s.Store.InsertJudgeResult(ctx, scope, model.OnlineJudgeResult{TenantID: scope.TenantID, FarmID: scope.FarmID,
		RunID: runID, AdviceID: card.AdviceID, StatDate: date, Category: cat, Dimension: judge.DimFactConsistency,
		JudgeVersion: s.Version, JudgeScore: score, Status: status}, s.Clock.Now())
	return err
}

// onlineFixture 是线上材料的 fixture：只放卡片自身的定位信息（线上没有评测样本的预期）。
type onlineFixture struct {
	Category       model.Category `json:"category"`
	Crop           string         `json:"crop"`
	PhenologyStage string         `json:"phenology_stage,omitempty"`
	FieldID        uint64         `json:"field_id"`
}

func (s *Sampler) material(ctx context.Context, scope model.Scope, card model.AdviceCard) (judge.Material, error) {
	fx, err := json.Marshal(onlineFixture{Category: card.Category, Crop: card.Crop, PhenologyStage: card.PhenologyStage,
		FieldID: card.FieldID})
	if err != nil {
		return judge.Material{}, fmt.Errorf("quality: marshal fixture run %d: %w", card.RunID, err)
	}
	m := judge.Material{Dimension: judge.DimFactConsistency, Fixture: fx,
		Output: judge.FinalOutput{Outcome: card.Outcome, RefuseReason: card.RefuseReason}}
	var kb judge.EntryLoader
	if len(card.Citations) > 0 {
		ref, err := s.Store.RunKnowledgeRef(ctx, scope, card.RunID)
		if err != nil {
			return judge.Material{}, err
		}
		if kb, err = s.KB.Open(ctx, ref); err != nil {
			return judge.Material{}, fmt.Errorf("quality: open knowledge %s for run %d: %w", ref, card.RunID, err)
		}
	}
	if err := m.SetCard(ctx, kb, &card); err != nil {
		return judge.Material{}, fmt.Errorf("quality: material run %d: %w", card.RunID, err)
	}
	return m, nil
}

// take 先取生产模型令牌、再取 judge 令牌；都取到才调 judge。judge 令牌取不到（或出错）时退还已取的模型令牌，
// 不让抽样白占主链额度。取不到时按 judge QPM 的补充间隔等待，超过次数返回 errJudgeDown。
func (s *Sampler) take(ctx context.Context) error {
	for attempt := 0; ; attempt++ {
		ok, err := s.takeBoth(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if attempt >= maxTokenWaits {
			return fmt.Errorf("%w: rate limited (%s / %s)", errJudgeDown, s.ModelKey, s.JudgeKey)
		}
		if err := s.wait(ctx, time.Minute/time.Duration(max(s.JudgeQPM, 1))); err != nil {
			return err
		}
	}
}

// takeBoth 取一对令牌：模型令牌取到而 judge 令牌被拒或出错时，退还模型令牌（退还失败与原错误一并返回）。
func (s *Sampler) takeBoth(ctx context.Context) (bool, error) {
	ok, err := s.Tokens.Take(ctx, s.ModelKey, s.ModelQPM, s.Clock.Now())
	if err != nil {
		return false, fmt.Errorf("quality: take %s: %w", s.ModelKey, err)
	}
	if !ok {
		return false, nil
	}
	ok, err = s.Tokens.Take(ctx, s.JudgeKey, s.JudgeQPM, s.Clock.Now())
	if err == nil && ok {
		return true, nil
	}
	if err != nil {
		err = fmt.Errorf("quality: take %s: %w", s.JudgeKey, err)
	}
	if rerr := s.Tokens.Refund(context.WithoutCancel(ctx), s.ModelKey, s.ModelQPM, s.Clock.Now()); rerr != nil {
		err = errors.Join(err, fmt.Errorf("quality: refund %s: %w", s.ModelKey, rerr))
	}
	return false, err
}

func (s *Sampler) wait(ctx context.Context, d time.Duration) error {
	if s.Wait != nil {
		return s.Wait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
