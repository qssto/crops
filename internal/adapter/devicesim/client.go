package devicesim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/model"
)

// Name 是本 Adapter 的名称。
const Name = "devicesim"

// DefaultTimeout 是未配置超时时的单次请求超时。
const DefaultTimeout = 5 * time.Second

// maxBody 是响应体上限，防止异常平台把内存打满。
const maxBody = 1 << 20

// Config 是客户端配置（configs/devices.yaml 的 devicesim 段）。
type Config struct {
	BaseURL string
	Timeout time.Duration
	Caps    adapter.Capabilities // 平台声明的能力；Registry 按开放规则校验
	HTTP    *http.Client         // 可选；为空按 Timeout 新建
}

// Client 实现 adapter.DeviceAdapter 与 adapter.CommandEncoder。
type Client struct {
	base *url.URL
	caps adapter.Capabilities
	hc   *http.Client
}

var (
	_ adapter.DeviceAdapter  = (*Client)(nil)
	_ adapter.CommandEncoder = (*Client)(nil)
)

// New 校验配置并构造客户端。
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("devicesim: base_url %q must be http(s)://host[:port]", cfg.BaseURL)
	}
	hc := cfg.HTTP
	if hc == nil {
		t := cfg.Timeout
		if t <= 0 {
			t = DefaultTimeout
		}
		hc = &http.Client{Timeout: t}
	}
	return &Client{base: u, caps: cfg.Caps, hc: hc}, nil
}

// Name 返回 Name。
func (c *Client) Name() string { return Name }

// Capabilities 实现 adapter.DeviceAdapter。
func (c *Client) Capabilities() adapter.Capabilities { return c.caps }

// EncodeCommand 实现 adapter.CommandEncoder：即 POST /v1/jobs 的请求体。
func (c *Client) EncodeCommand(order model.WorkOrder, idemKey string) ([]byte, error) {
	return json.Marshal(submitOf(order, idemKey))
}

func submitOf(order model.WorkOrder, idemKey string) SubmitRequest {
	return SubmitRequest{IdemKey: idemKey, TenantID: order.TenantID, FarmID: order.FarmID, OrderID: order.OrderID,
		RunID: order.RunID, FieldID: order.FieldID, DeviceType: order.DeviceType, WorkType: string(order.WorkType),
		Payload: order.Payload, From: order.TimeWindow.From, To: order.TimeWindow.To}
}

// Dispatch 实现 adapter.DeviceAdapter：POST /v1/jobs（平台按 idem_key 幂等，同键返回同一任务）。
// 返回任务的 external_no / order_id 须与请求一致，否则 ErrBadReceipt（防平台串单）。
func (c *Client) Dispatch(ctx context.Context, order model.WorkOrder, idemKey string) (model.DeviceReceipt, error) {
	if idemKey == "" {
		return model.DeviceReceipt{}, fmt.Errorf("%w: idem key is required", adapter.ErrRejected)
	}
	body, err := c.EncodeCommand(order, idemKey)
	if err != nil {
		return model.DeviceReceipt{}, fmt.Errorf("devicesim: encode order %d: %w", order.OrderID, err)
	}
	return c.job(ctx, http.MethodPost, "/v1/jobs", body, expect{idemKey: idemKey, orderID: order.OrderID})
}

// QueryStatus 实现 adapter.DeviceAdapter：有 device_job_id 按任务号查，否则按外部单号（需 ExternalQuery）。
// 返回任务的 job_id / external_no 须与 ref 中给出的一致。
func (c *Client) QueryStatus(ctx context.Context, ref adapter.StatusRef) (model.DeviceReceipt, error) {
	want := expect{jobID: ref.DeviceJobID, idemKey: ref.IdemKey}
	switch {
	case ref.DeviceJobID != "":
		return c.job(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(ref.DeviceJobID), nil, want)
	case ref.IdemKey == "":
		return model.DeviceReceipt{}, fmt.Errorf("%w: device_job_id or idem_key is required", adapter.ErrRejected)
	case !c.caps.ExternalQuery:
		return model.DeviceReceipt{}, fmt.Errorf("%w: query by external no", adapter.ErrCapability)
	}
	return c.job(ctx, http.MethodGet, "/v1/jobs?external_no="+url.QueryEscape(ref.IdemKey), nil, want)
}

// Cancel 实现 adapter.DeviceAdapter：POST /v1/jobs/{id}/cancel；任务已终结时平台返回当前状态。
func (c *Client) Cancel(ctx context.Context, deviceJobID string) (model.DeviceReceipt, error) {
	if !c.caps.Cancel {
		return model.DeviceReceipt{}, fmt.Errorf("%w: cancel", adapter.ErrCapability)
	}
	if deviceJobID == "" {
		return model.DeviceReceipt{}, fmt.Errorf("%w: device_job_id is required", adapter.ErrRejected)
	}
	return c.job(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(deviceJobID)+"/cancel", nil, expect{jobID: deviceJobID})
}

// expect 是请求方已知的任务标识；非零字段须与平台返回的任务一致。
type expect struct {
	jobID   string
	idemKey string
	orderID uint64
}

func (e expect) check(j Job) error {
	switch {
	case e.jobID != "" && j.JobID != e.jobID:
		return fmt.Errorf("%w: job_id %q, want %q", adapter.ErrBadReceipt, j.JobID, e.jobID)
	case e.idemKey != "" && j.ExternalNo != e.idemKey:
		return fmt.Errorf("%w: external_no %q, want %q", adapter.ErrBadReceipt, j.ExternalNo, e.idemKey)
	case e.orderID != 0 && j.OrderID != e.orderID:
		return fmt.Errorf("%w: order_id %d, want %d", adapter.ErrBadReceipt, j.OrderID, e.orderID)
	}
	return nil
}

// job 发请求、核对任务标识并把平台任务转为标准回执。
func (c *Client) job(ctx context.Context, method, path string, body []byte, want expect) (model.DeviceReceipt, error) {
	raw, err := c.do(ctx, method, path, body)
	if err != nil {
		return model.DeviceReceipt{}, err
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return model.DeviceReceipt{}, fmt.Errorf("%w: devicesim %s %s: %w", adapter.ErrBadReceipt, method, path, err)
	}
	if err := want.check(j); err != nil {
		return model.DeviceReceipt{}, fmt.Errorf("devicesim %s %s: %w", method, path, err)
	}
	r, err := receiptOf(j)
	if err != nil {
		return model.DeviceReceipt{}, fmt.Errorf("devicesim %s %s: %w", method, path, err)
	}
	return r, nil
}

// do 执行请求：超时 → ErrTimeout，连接失败 / 5xx → ErrUnavailable，404 → ErrJobNotFound，其余 4xx → ErrRejected。
// 2xx 响应头之后读 body 失败归 ErrTimeout（平台可能已受理，结果未知，需按 idem_key 查询 / 对账）；
// body 超过 maxBody 显式报错（2xx 为 ErrBadReceipt）。
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, rd)
	if err != nil {
		return nil, fmt.Errorf("devicesim: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return nil, fmt.Errorf("%w: %s %s: %w", adapter.ErrTimeout, method, path, err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("devicesim: %s %s: %w", method, path, ctxErr)
		}
		return nil, fmt.Errorf("%w: %s %s: %w", adapter.ErrUnavailable, method, path, err)
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	err = errors.Join(err, resp.Body.Close())
	switch {
	case err != nil && ok:
		return nil, fmt.Errorf("%w: %s %s: read body after HTTP %d (result unknown): %w",
			adapter.ErrTimeout, method, path, resp.StatusCode, err)
	case err != nil:
		return nil, fmt.Errorf("%w: %s %s: read body: %w", adapter.ErrUnavailable, method, path, err)
	case len(raw) > maxBody && ok:
		return nil, fmt.Errorf("%w: %s %s: body exceeds %d bytes", adapter.ErrBadReceipt, method, path, maxBody)
	}
	if len(raw) > maxBody {
		raw = raw[:maxBody] // 非 2xx 只取错误摘要，按状态码归类
	}
	switch {
	case ok:
		return raw, nil
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %s %s: HTTP %d %s", adapter.ErrUnavailable, method, path, resp.StatusCode, errMsg(raw))
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s %s: %s", adapter.ErrJobNotFound, method, path, errMsg(raw))
	default:
		return nil, fmt.Errorf("%w: %s %s: HTTP %d %s", adapter.ErrRejected, method, path, resp.StatusCode, errMsg(raw))
	}
}

func errMsg(raw []byte) string {
	var e ErrorBody
	if json.Unmarshal(raw, &e) == nil && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return strings.TrimSpace(string(raw[:min(len(raw), 200)]))
}

// stateMap 是 devicesim 协议状态 → 标准回执状态。
var stateMap = map[string]model.AttemptStatus{
	JobAccepted: model.AttemptAccepted,
	JobRunning:  model.AttemptExecuting,
	JobFinished: model.AttemptDone,
	JobPartial:  model.AttemptPartiallyDone,
	JobFailed:   model.AttemptFailed,
	JobCanceled: model.AttemptCanceled,
}

// stageMap 是 devicesim 失败阶段 → FailureKind。
var stageMap = map[string]model.FailureKind{
	StageBeforeStart: model.FailureNotExecuted,
	StageInProgress:  model.FailureInExecution,
	StageUnknown:     model.FailureUnknown,
}

// receiptOf 把平台任务转为标准回执；未登记的状态 / 阶段与自相矛盾的回执返回 ErrBadReceipt。
func receiptOf(j Job) (model.DeviceReceipt, error) {
	st, ok := stateMap[j.State]
	if !ok {
		return model.DeviceReceipt{}, fmt.Errorf("%w: unknown state %q", adapter.ErrBadReceipt, j.State)
	}
	r := model.DeviceReceipt{Status: st, DeviceJobID: j.JobID, DoneAreaMu: j.DoneMu, DeviceTime: j.UpdatedAt.UTC(),
		Code: j.ResultCode}
	if j.FailStage != "" {
		k, ok := stageMap[j.FailStage]
		if !ok {
			return model.DeviceReceipt{}, fmt.Errorf("%w: unknown fail_stage %q", adapter.ErrBadReceipt, j.FailStage)
		}
		r.FailureKind = k
	}
	if err := r.Validate(); err != nil {
		return model.DeviceReceipt{}, err
	}
	return r, nil
}
