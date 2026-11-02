package sample

import (
	"fmt"
	"time"

	"agri-steward/internal/model"
)

// P1-L4-B：设备回执脚本与回执投递步骤（样本 fixtures.device.scripts / fixtures.device.steps，expect.work_done）。
// 旧样本不声明时 RecordingAdapter 行为不变（只走 fixtures.device.receipts 全局序列）。

// 脚本可声明的设备错误（RecordingAdapter 返回对应的 adapter 错误）。
const (
	DeviceErrTimeout     = "TIMEOUT"     // adapter.ErrTimeout：平台是否受理未知
	DeviceErrUnavailable = "UNAVAILABLE" // adapter.ErrUnavailable：平台不可用，可退避重试
	DeviceErrRejected    = "REJECTED"    // adapter.ErrRejected：请求被拒，重试无效
)

var (
	scriptMethods = map[string]bool{model.DeviceMethodDispatch: true, model.DeviceMethodQueryStatus: true, model.DeviceMethodCancel: true}
	scriptErrors  = map[string]bool{DeviceErrTimeout: true, DeviceErrUnavailable: true, DeviceErrRejected: true}
)

// DeviceScript 是一个方法、一张作业单的回执序列：Method 取 Dispatch / QueryStatus / Cancel；
// Order 是作业单在本 trial 中首次出现在 Adapter（DryRun / Dispatch）的次序（1 起），0 表示任一作业单。
// 同一调用先匹配 Order 精确的脚本，再匹配 Order=0 的脚本；脚本用完后回落到 fixtures.device.receipts 与原有默认值。
type DeviceScript struct {
	Method string       `yaml:"method"`
	Order  int          `yaml:"order,omitempty"`
	Steps  []ScriptStep `yaml:"steps"`
}

// ScriptStep 是脚本中的一步：Error 非空时返回对应错误（不返回回执），否则返回 Receipt。
// Accepted 只对 Dispatch 的错误步有意义：true 表示平台其实已受理（之后按 idem_key 的 QueryStatus 能查到任务）。
type ScriptStep struct {
	ReceiptFixture `yaml:",inline"`
	Error          string `yaml:"error,omitempty"`
	Accepted       bool   `yaml:"accepted,omitempty"`
}

// ReceiptStep 是回执投递步骤（Runner 在审批脚本与 outbox 排空之后按序执行）：
// Advance 推进 FrozenClock；Receipt 非空时经回执 webhook（签名）投递给第 Order 张作业单的最近一次尝试；
// Repeat 为 true 时原样重投上一条（回执重放）；Cancel 为 true 时以提问用户中止第 Order 张作业单；
// Scan 为 true 时触发一轮回执超时扫描。每步之后排空一轮 outbox。
type ReceiptStep struct {
	Advance string          `yaml:"advance,omitempty"` // time.ParseDuration 格式，如 45m
	Order   int             `yaml:"order,omitempty"`   // 默认 1
	Receipt *ReceiptFixture `yaml:"receipt,omitempty"`
	Repeat  bool            `yaml:"repeat,omitempty"`
	Cancel  bool            `yaml:"cancel,omitempty"`
	Scan    bool            `yaml:"scan,omitempty"`
}

// WorkDoneExpect 是 partially_done 打分器的断言：存在一张作业单终态为 Status、done_area_mu 为 DoneAreaMu，
// 且该记录被 LocalOrderRepo 读到（后续 PHI 推算可用），并已回写 field.history。
type WorkDoneExpect struct {
	Status     string  `yaml:"status"` // DONE / PARTIALLY_DONE
	DoneAreaMu float64 `yaml:"done_area_mu"`
}

// AdvanceDuration 解析 Advance（空为 0）。
func (s ReceiptStep) AdvanceDuration() (time.Duration, error) {
	if s.Advance == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s.Advance)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("advance %q must be a non-negative duration", s.Advance)
	}
	return d, nil
}

// checkDeviceScripts 校验脚本与投递步骤（由 checkDeviceFixture 调用）。
func checkDeviceScripts(d *DeviceFixture) []string {
	var msgs []string
	for i, sc := range d.Scripts {
		at := fmt.Sprintf("fixtures.device.scripts[%d]", i)
		if !scriptMethods[sc.Method] {
			msgs = append(msgs, fmt.Sprintf("%s.method %q not in [Dispatch QueryStatus Cancel]", at, sc.Method))
		}
		if sc.Order < 0 {
			msgs = append(msgs, fmt.Sprintf("%s.order %d must be >= 0", at, sc.Order))
		}
		if len(sc.Steps) == 0 {
			msgs = append(msgs, at+".steps is empty")
		}
		for j, st := range sc.Steps {
			msgs = append(msgs, checkScriptStep(fmt.Sprintf("%s.steps[%d]", at, j), sc.Method, st)...)
		}
	}
	for i, st := range d.Steps {
		at := fmt.Sprintf("fixtures.device.steps[%d]", i)
		if _, err := st.AdvanceDuration(); err != nil {
			msgs = append(msgs, at+": "+err.Error())
		}
		if st.Order < 0 {
			msgs = append(msgs, fmt.Sprintf("%s.order %d must be >= 0", at, st.Order))
		}
		if st.Receipt != nil {
			if err := probeReceipt(*st.Receipt); err != nil {
				msgs = append(msgs, fmt.Sprintf("%s.receipt: %v", at, err))
			}
		}
		if st.Repeat && st.Receipt != nil {
			msgs = append(msgs, at+": repeat and receipt are exclusive")
		}
		if st.Cancel && (st.Repeat || st.Receipt != nil) {
			msgs = append(msgs, at+": cancel excludes receipt / repeat")
		}
		if st.Advance == "" && st.Receipt == nil && !st.Repeat && !st.Cancel && !st.Scan {
			msgs = append(msgs, at+": empty step")
		}
	}
	return msgs
}

func checkScriptStep(at, method string, st ScriptStep) []string {
	if st.Error != "" {
		var msgs []string
		if !scriptErrors[st.Error] {
			msgs = append(msgs, fmt.Sprintf("%s.error %q not in [TIMEOUT UNAVAILABLE REJECTED]", at, st.Error))
		}
		if st.Status != "" {
			msgs = append(msgs, at+": error and status are exclusive")
		}
		if st.Accepted && method != model.DeviceMethodDispatch {
			msgs = append(msgs, at+".accepted only applies to Dispatch")
		}
		return msgs
	}
	if st.Accepted {
		return []string{at + ".accepted requires error"}
	}
	if err := probeReceipt(st.ReceiptFixture); err != nil {
		return []string{fmt.Sprintf("%s: %v", at, err)}
	}
	return nil
}

// probeReceipt 补任务号后校验回执自洽（与 checkDeviceFixture 同口径）。
func probeReceipt(f ReceiptFixture) error {
	r := f.Receipt()
	r.DeviceJobID = "recorded"
	return r.Validate()
}

// checkWorkDone 校验 expect.work_done。
func checkWorkDone(w *WorkDoneExpect) []string {
	if w == nil {
		return nil
	}
	var msgs []string
	if s := model.WorkOrderStatus(w.Status); s != model.OrderDone && s != model.OrderPartiallyDone {
		msgs = append(msgs, fmt.Sprintf("expect.work_done.status %q not in [DONE PARTIALLY_DONE]", w.Status))
	}
	if w.DoneAreaMu < 0 {
		msgs = append(msgs, "expect.work_done.done_area_mu must be >= 0")
	}
	return msgs
}
