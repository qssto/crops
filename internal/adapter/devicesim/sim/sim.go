// Package sim 是本地设备模拟平台（cmd/devicesim 的实现，P1 §6.3 功能任务 4）：按 idem_key 幂等受理、
// 按任务号或外部单号查询、撤销，回执序列可按作业单脚本化（成功 / 失败 / 部分 / 无回执 / 延迟）。
// 只用于本地 / 预发演练与端到端测试（httptest），不对接任何真实设备。状态在内存，进程重启即清空。
package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"agri-steward/internal/adapter/devicesim"
	"agri-steward/internal/clock"
)

// maxRequest 是请求体上限。
const maxRequest = 1 << 20

// codeBadState 是 bad_receipt 脚本写入的未登记平台状态（客户端应判 ErrBadReceipt）。
const codeBadState = "exploded"

// Server 是模拟平台。并发安全。
type Server struct {
	clk     clock.Clock
	devices DeviceFile

	mu      sync.Mutex
	seq     int
	jobs    map[string]*job             // job_id → 任务
	byIdem  map[string]string           // idem_key → job_id
	scripts map[uint64]devicesim.Script // order_id → 脚本
}

// job 是平台任务及其剩余脚本步骤。
type job struct {
	devicesim.Job
	req   devicesim.SubmitRequest
	steps []devicesim.Step
}

// New 构造模拟平台；devices 为设备清单（可为空清单）。
func New(clk clock.Clock, devices DeviceFile) (*Server, error) {
	if clk == nil {
		return nil, errors.New("sim: clock is required")
	}
	if err := devices.validate(); err != nil {
		return nil, fmt.Errorf("sim: devices: %w", err)
	}
	return &Server{clk: clk, devices: devices, jobs: map[string]*job{}, byIdem: map[string]string{},
		scripts: map[uint64]devicesim.Script{}}, nil
}

// Handler 返回 HTTP 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs", s.submit)
	mux.HandleFunc("GET /v1/jobs", s.byExternal)
	mux.HandleFunc("GET /v1/jobs/{id}", s.get)
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("GET /v1/devices", s.listDevices)
	mux.HandleFunc("GET /v1/devices/{id}", s.getDevice)
	mux.HandleFunc("PUT /sim/v1/scripts/{order_id}", s.putScript)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

// SetScript 脚本化某作业单的回执序列（PUT /sim/v1/scripts/{order_id} 的进程内入口，测试用）。
func (s *Server) SetScript(orderID uint64, sc devicesim.Script) error {
	if err := checkScript(sc); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts[orderID] = sc
	return nil
}

func checkScript(sc devicesim.Script) error {
	switch sc.Submit {
	case "", devicesim.SubmitAccept, devicesim.SubmitReject, devicesim.SubmitError, devicesim.SubmitBadReceipt:
	default:
		return fmt.Errorf("sim: unknown submit %q", sc.Submit)
	}
	if sc.DelayMS < 0 || sc.AcceptDelayMS < 0 {
		return errors.New("sim: delay_ms and accept_delay_ms must be >= 0")
	}
	for i, st := range sc.Steps {
		if !knownState[st.State] || st.State == devicesim.JobAccepted {
			return fmt.Errorf("sim: steps[%d]: state %q not in running/finished/partial/failed/canceled", i, st.State)
		}
		if (st.State == devicesim.JobFailed) != (st.FailStage != "") {
			return fmt.Errorf("sim: steps[%d]: fail_stage is required with and only with failed", i)
		}
	}
	return nil
}

var knownState = map[string]bool{devicesim.JobAccepted: true, devicesim.JobRunning: true, devicesim.JobFinished: true,
	devicesim.JobPartial: true, devicesim.JobFailed: true, devicesim.JobCanceled: true}

var finalState = map[string]bool{devicesim.JobFinished: true, devicesim.JobPartial: true, devicesim.JobFailed: true,
	devicesim.JobCanceled: true}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var req devicesim.SubmitRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, devicesim.CodeBadRequest, err.Error())
		return
	}
	if req.IdemKey == "" || req.OrderID == 0 || req.TenantID == 0 || req.FarmID == 0 {
		writeErr(w, http.StatusBadRequest, devicesim.CodeBadRequest, "idem_key, tenant_id, farm_id and order_id are required")
		return
	}
	sc := s.script(req.OrderID)
	if err := wait(r.Context(), time.Duration(sc.DelayMS)*time.Millisecond); err != nil {
		return // 客户端已放弃（超时演练）
	}
	switch sc.Submit {
	case devicesim.SubmitReject:
		writeErr(w, http.StatusUnprocessableEntity, devicesim.CodeRejected, "scripted reject")
		return
	case devicesim.SubmitError:
		writeErr(w, http.StatusServiceUnavailable, devicesim.CodeUnavailable, "scripted platform error")
		return
	}
	j, clash := s.accept(req, sc)
	if clash {
		writeErr(w, http.StatusConflict, devicesim.CodeIdemClash, "idem_key already used by another order")
		return
	}
	if err := wait(r.Context(), time.Duration(sc.AcceptDelayMS)*time.Millisecond); err != nil {
		return // 已建任务但客户端已放弃（结果未知演练）：任务保留，可按 idem_key 查到
	}
	writeJSON(w, http.StatusOK, j)
}

// accept 按 idem_key 幂等建任务：同键同单返回原任务，同键异单冲突。
func (s *Server) accept(req devicesim.SubmitRequest, sc devicesim.Script) (devicesim.Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byIdem[req.IdemKey]; ok {
		j := s.jobs[id]
		return j.Job, !sameOrder(j.req, req)
	}
	s.seq++
	id := "sim-" + strconv.Itoa(s.seq)
	state := devicesim.JobAccepted
	if sc.Submit == devicesim.SubmitBadReceipt {
		state = codeBadState
	}
	j := &job{Job: devicesim.Job{JobID: id, ExternalNo: req.IdemKey, OrderID: req.OrderID, State: state,
		UpdatedAt: s.clk.Now().UTC(), ResultCode: "ACCEPTED"}, req: req, steps: append([]devicesim.Step(nil), sc.Steps...)}
	s.jobs[id] = j
	s.byIdem[req.IdemKey] = id
	return j.Job, false
}

func sameOrder(a, b devicesim.SubmitRequest) bool {
	return a.TenantID == b.TenantID && a.FarmID == b.FarmID && a.OrderID == b.OrderID && a.RunID == b.RunID &&
		a.FieldID == b.FieldID && a.DeviceType == b.DeviceType && a.WorkType == b.WorkType &&
		bytes.Equal(a.Payload, b.Payload)
}

func (s *Server) script(orderID uint64) devicesim.Script {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc, ok := s.scripts[orderID]; ok {
		return sc
	}
	return devicesim.DefaultScript()
}

func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	s.query(w, r.PathValue("id"))
}

func (s *Server) byExternal(w http.ResponseWriter, r *http.Request) {
	no := r.URL.Query().Get("external_no")
	if no == "" {
		writeErr(w, http.StatusBadRequest, devicesim.CodeBadRequest, "external_no is required")
		return
	}
	s.mu.Lock()
	id := s.byIdem[no]
	s.mu.Unlock()
	s.query(w, id)
}

// query 每次查询推进一步脚本（未终结且有剩余步骤时），返回推进后的任务。
func (s *Server) query(w http.ResponseWriter, id string) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if ok && !finalState[j.State] && j.State != codeBadState && len(j.steps) > 0 {
		st := j.steps[0]
		j.steps = j.steps[1:]
		j.State, j.FailStage, j.DoneMu = st.State, st.FailStage, st.DoneMu
		j.ResultCode, j.UpdatedAt = resultCode(st), s.clk.Now().UTC()
	}
	var out devicesim.Job
	if ok {
		out = j.Job
	}
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, devicesim.CodeNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func resultCode(st devicesim.Step) string {
	switch st.State {
	case devicesim.JobFailed:
		return "FAILED_" + st.FailStage
	case devicesim.JobCanceled:
		return "CANCELED"
	case devicesim.JobRunning:
		return "RUNNING"
	}
	return "OK"
}

// cancel 撤销未终结任务；已终结时返回当前状态（幂等）。
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j, ok := s.jobs[r.PathValue("id")]
	if ok && !finalState[j.State] && j.State != codeBadState {
		j.State, j.FailStage, j.DoneMu, j.steps = devicesim.JobCanceled, "", nil, nil
		j.ResultCode, j.UpdatedAt = "CANCELED", s.clk.Now().UTC()
	}
	var out devicesim.Job
	if ok {
		out = j.Job
	}
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, devicesim.CodeNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putScript(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseUint(r.PathValue("order_id"), 10, 64)
	if err != nil || orderID == 0 {
		writeErr(w, http.StatusBadRequest, devicesim.CodeBadRequest, "order_id must be a positive integer")
		return
	}
	var sc devicesim.Script
	if err := decode(r, &sc); err != nil {
		writeErr(w, http.StatusBadRequest, devicesim.CodeBadRequest, err.Error())
		return
	}
	if err := s.SetScript(orderID, sc); err != nil {
		writeErr(w, http.StatusBadRequest, devicesim.CodeBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(b); err != nil {
		// 响应头已发出，无法再改状态码；通常是客户端已断开（如超时演练），记日志供排查。
		slog.Warn("sim: write response body", "status", status, "err", err)
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, devicesim.ErrorBody{Code: code, Message: msg})
}
