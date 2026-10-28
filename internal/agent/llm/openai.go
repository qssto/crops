package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// OpenAI 兼容端点的环境变量（只从环境读取，绝不写入配置文件）。
const (
	EnvBaseURL = "ONEAPI_BASE_URL"
	EnvAPIKey  = "ONEAPI_API_KEY"
	EnvModel   = "ONEAPI_MODEL"

	chatCompletionsPath = "/chat/completions"
	maxErrBody          = 512 // 上游错误体只截取前若干字节写进错误，避免日志膨胀
)

// ErrMissingEnv 表示 OpenAI 兼容实现所需的环境变量未设置。
var ErrMissingEnv = errors.New("llm: openai-compatible model needs env " + EnvBaseURL + " / " + EnvAPIKey + " / " + EnvModel)

// OpenAIConfig 是 OpenAI 兼容 /chat/completions 端点参数。
type OpenAIConfig struct {
	BaseURL string // 如 https://oneapi.example.com/v1
	APIKey  string // secret
	Model   string
	HTTP    *http.Client // 可空，默认 http.DefaultClient；超时由 Client 档位的 ctx 控制
}

// OpenAIConfigFromEnv 从 ONEAPI_* 环境变量读取配置；任一缺失返回 ErrMissingEnv（列出缺失项）。
func OpenAIConfigFromEnv() (OpenAIConfig, error) {
	cfg := OpenAIConfig{BaseURL: os.Getenv(EnvBaseURL), APIKey: os.Getenv(EnvAPIKey), Model: os.Getenv(EnvModel)}
	var missing []string
	for name, v := range map[string]string{EnvBaseURL: cfg.BaseURL, EnvAPIKey: cfg.APIKey, EnvModel: cfg.Model} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return OpenAIConfig{}, fmt.Errorf("%w (missing %s)", ErrMissingEnv, strings.Join(missing, ", "))
	}
	return cfg, nil
}

// OpenAI 是 OpenAI 兼容 ChatModel 的最小实现（标准库 net/http，非流式）。
// 说明：任务原定使用 eino-ext openai 组件，但本环境未获准新增该依赖，故以此适配器代替；接口同为 BaseChatModel，可无缝替换。
type OpenAI struct {
	cfg OpenAIConfig
}

var _ einomodel.BaseChatModel = (*OpenAI)(nil)

// NewOpenAI 校验并构造适配器。
func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	if cfg.BaseURL == "" || cfg.APIKey == "" || cfg.Model == "" {
		return nil, ErrMissingEnv
	}
	if cfg.HTTP == nil {
		cfg.HTTP = http.DefaultClient
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &OpenAI{cfg: cfg}, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float32      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Generate 调 POST {base}/chat/completions。
func (o *OpenAI) Generate(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (msg *schema.Message, err error) {
	common := einomodel.GetCommonOptions(&einomodel.Options{}, opts...)
	req := chatRequest{Model: o.cfg.Model, Temperature: common.Temperature, MaxTokens: common.MaxTokens}
	if common.Model != nil && *common.Model != "" {
		req.Model = *common.Model
	}
	for _, m := range msgs {
		if m != nil {
			req.Messages = append(req.Messages, chatMessage{Role: string(m.Role), Content: m.Content})
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm openai: marshal request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.BaseURL+chatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm openai: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	resp, err := o.cfg.HTTP.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("llm openai: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			msg, err = nil, fmt.Errorf("llm openai: close response: %w", cerr)
		}
	}()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("llm openai: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if len(raw) > maxErrBody {
			raw = raw[:maxErrBody]
		}
		return nil, fmt.Errorf("llm openai: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("llm openai: decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return nil, errors.New("llm openai: response has no choices")
	}
	out := schema.AssistantMessage(cr.Choices[0].Message.Content, nil)
	out.ResponseMeta = &schema.ResponseMeta{FinishReason: cr.Choices[0].FinishReason}
	if cr.Usage != nil {
		out.ResponseMeta.Usage = &schema.TokenUsage{PromptTokens: cr.Usage.PromptTokens,
			CompletionTokens: cr.Usage.CompletionTokens, TotalTokens: cr.Usage.TotalTokens}
	}
	return out, nil
}

// Stream 以单帧流返回 Generate 的结果（主链路只用 Generate）。
func (o *OpenAI) Stream(ctx context.Context, msgs []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	m, err := o.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{m}), nil
}
