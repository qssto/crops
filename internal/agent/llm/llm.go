// Package llm 是 ChatModel 的薄封装：档位（温度 / max_tokens / 超时 / 重试）、限流、脱敏钩子与 token 计量。
// 模型只出现在主链路的 classify / choose / explain 三个节点，均以「JSON 任务」形式调用：
// system = 分层提示词 + 任务提示词，user = {"task": ..., "input": ...}，回复必须是 JSON。
// 任何不可用（限流、超时、重试耗尽、回复非 JSON）都返回 ErrUnavailable，由主链路走降级。
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Tier 是模型档位名（configs/model.yaml 的 tiers 键）。
type Tier string

// TierConfig 是一个档位的调用参数。
type TierConfig struct {
	Temperature float32
	MaxTokens   int
	Timeout     time.Duration // 单次尝试超时
	MaxRetries  int           // 失败后的重试次数（总尝试 = 1 + MaxRetries）
}

// Limiter 是按档位的调用限流；false 表示超限（走降级）。实现：internal/app 的 Redis 限流（rl:model:{tier}）。
type Limiter interface {
	Allow(ctx context.Context, tier Tier) (bool, error)
}

// Redactor 在发往模型前处理用户可见文本。P0 默认原样透传。
// TODO(L5): 接入脱敏（手机号 / 身份证 / 坐标），与 L5 打分器的 pii 断言对齐。
type Redactor func(string) string

// Passthrough 是默认脱敏钩子：原样返回。
func Passthrough(s string) string { return s }

// Usage 是 token 计量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Add 累加。
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
}

// 错误。ErrUnavailable 是降级判据；ErrRateLimited 包装在 ErrUnavailable 之下。
var (
	ErrUnavailable = errors.New("llm: model unavailable")
	ErrRateLimited = errors.New("llm: rate limited")
	ErrBadReply    = errors.New("llm: reply is not valid JSON")
	ErrUnknownTier = errors.New("llm: unknown tier")
)

// Config 是 Client 的构造参数。Model / Prompts / Tiers 必填，Limiter / Redact 可空。
type Config struct {
	Model   einomodel.BaseChatModel
	Prompts *Prompts
	Tiers   map[Tier]TierConfig
	Limiter Limiter
	Redact  Redactor
	// Backoff 是第 n 次重试前的等待基数（第 n 次等 n × Backoff）；0 表示不等待。
	Backoff time.Duration
}

// Client 是线程安全的模型调用封装。
type Client struct {
	model   einomodel.BaseChatModel
	prompts *Prompts
	tiers   map[Tier]TierConfig
	limiter Limiter
	redact  Redactor
	backoff time.Duration

	mu    sync.Mutex
	usage map[Tier]Usage
}

// New 校验配置并构造 Client。
func New(cfg Config) (*Client, error) {
	var errs []error
	if cfg.Model == nil {
		errs = append(errs, errors.New("llm: model is required"))
	}
	if cfg.Prompts == nil {
		errs = append(errs, errors.New("llm: prompts are required"))
	}
	if len(cfg.Tiers) == 0 {
		errs = append(errs, errors.New("llm: at least one tier is required"))
	}
	for name, t := range cfg.Tiers {
		if t.Timeout <= 0 || t.MaxTokens <= 0 || t.MaxRetries < 0 {
			errs = append(errs, fmt.Errorf("llm: tier %s needs timeout > 0, max_tokens > 0, max_retries >= 0", name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	redact := cfg.Redact
	if redact == nil {
		redact = Passthrough
	}
	tiers := make(map[Tier]TierConfig, len(cfg.Tiers))
	for k, v := range cfg.Tiers {
		tiers[k] = v
	}
	return &Client{model: cfg.Model, prompts: cfg.Prompts, tiers: tiers, limiter: cfg.Limiter,
		redact: redact, backoff: cfg.Backoff, usage: map[Tier]Usage{}}, nil
}

// PromptVersion 返回提示词版本（写入 RunMeta.PromptVersion）。
func (c *Client) PromptVersion() string { return c.prompts.Version() }

// HasTier 判断档位是否已配置。
func (c *Client) HasTier(t Tier) bool { _, ok := c.tiers[t]; return ok }

// Usage 返回各档位累计 token（副本）。
func (c *Client) Usage() map[Tier]Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[Tier]Usage, len(c.usage))
	for k, v := range c.usage {
		out[k] = v
	}
	return out
}

// taskMessage 是发给模型的 user 消息体。
type taskMessage struct {
	Task  Task `json:"task"`
	Input any  `json:"input"`
}

// Do 以 tier 档位执行一个 JSON 任务，回复解码进 out。返回本次（含重试）的 token 用量。
// 限流、超时、重试耗尽与回复非 JSON 均返回包装 ErrUnavailable 的错误；父 ctx 取消时返回 ctx 错误。
func (c *Client) Do(ctx context.Context, tier Tier, task Task, input any, out any) (Usage, error) {
	cfg, ok := c.tiers[tier]
	if !ok {
		return Usage{}, fmt.Errorf("%w: %s", ErrUnknownTier, tier)
	}
	system, err := c.prompts.System(task)
	if err != nil {
		return Usage{}, err
	}
	body, err := json.Marshal(taskMessage{Task: task, Input: input})
	if err != nil {
		return Usage{}, fmt.Errorf("llm: marshal %s input: %w", task, err)
	}
	if c.limiter != nil {
		allowed, err := c.limiter.Allow(ctx, tier)
		if err != nil {
			return Usage{}, fmt.Errorf("%w: limiter: %w", ErrUnavailable, err)
		}
		if !allowed {
			return Usage{}, fmt.Errorf("%w: %w (tier %s)", ErrUnavailable, ErrRateLimited, tier)
		}
	}
	msgs := []*schema.Message{schema.SystemMessage(system), schema.UserMessage(c.redact(string(body)))}
	var total Usage
	var last error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := c.wait(ctx, attempt); err != nil {
				return total, err
			}
		}
		u, err := c.attempt(ctx, cfg, msgs, out)
		total.Add(u)
		c.meter(tier, u)
		if err == nil {
			return total, nil
		}
		if ctx.Err() != nil {
			return total, fmt.Errorf("llm %s: %w", task, ctx.Err())
		}
		last = err
	}
	return total, fmt.Errorf("%w: %s after %d attempts: %w", ErrUnavailable, task, cfg.MaxRetries+1, last)
}

func (c *Client) attempt(ctx context.Context, cfg TierConfig, msgs []*schema.Message, out any) (Usage, error) {
	actx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	resp, err := c.model.Generate(actx, msgs, einomodel.WithTemperature(cfg.Temperature), einomodel.WithMaxTokens(cfg.MaxTokens))
	if err != nil {
		return Usage{}, err
	}
	if resp == nil {
		return Usage{}, fmt.Errorf("%w: empty response", ErrBadReply)
	}
	u := usageOf(resp)
	if err := json.Unmarshal([]byte(stripFence(resp.Content)), out); err != nil {
		return u, fmt.Errorf("%w: %w", ErrBadReply, err)
	}
	return u, nil
}

func (c *Client) wait(ctx context.Context, attempt int) error {
	if c.backoff <= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(attempt) * c.backoff)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("llm: retry wait: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

func (c *Client) meter(tier Tier, u Usage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.usage[tier]
	cur.Add(u)
	c.usage[tier] = cur
}

func usageOf(m *schema.Message) Usage {
	if m.ResponseMeta == nil || m.ResponseMeta.Usage == nil {
		return Usage{}
	}
	u := m.ResponseMeta.Usage
	return Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
}

// stripFence 去掉模型常见的 ```json 围栏。
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimPrefix(s, "json")
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}
