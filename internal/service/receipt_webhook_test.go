package service

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

func opsTS(d time.Duration) string { return strconv.FormatInt(opsNow.Add(d).Unix(), 10) }

func TestVerifyReceiptSignature(t *testing.T) {
	body := []byte(`{"status":"DONE"}`)
	ts := opsTS(0)
	good := SignReceipt(opsSecret, ts, body)
	tests := []struct {
		name   string
		secret []byte
		ts     string
		sig    string
		body   []byte
		ok     bool
	}{
		{name: "ok", secret: opsSecret, ts: ts, sig: good, body: body, ok: true},
		{name: "sha256 prefix upper hex", secret: opsSecret, ts: ts, sig: "sha256=" + strings.ToUpper(good), body: body, ok: true},
		{name: "wrong secret", secret: []byte("other"), ts: ts, sig: good, body: body},
		{name: "tampered body", secret: opsSecret, ts: ts, sig: good, body: []byte(`{"status":"FAILED"}`)},
		{name: "expired", secret: opsSecret, ts: opsTS(-6 * time.Minute), sig: SignReceipt(opsSecret, opsTS(-6*time.Minute), body), body: body},
		{name: "future", secret: opsSecret, ts: opsTS(6 * time.Minute), sig: SignReceipt(opsSecret, opsTS(6*time.Minute), body), body: body},
		{name: "timestamp swapped", secret: opsSecret, ts: opsTS(time.Second), sig: good, body: body},
		{name: "bad timestamp", secret: opsSecret, ts: "yesterday", sig: good, body: body},
		{name: "bad hex", secret: opsSecret, ts: ts, sig: "zz", body: body},
		{name: "empty signature", secret: opsSecret, ts: ts, sig: "", body: body},
		{name: "empty secret", secret: nil, ts: ts, sig: good, body: body},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyReceiptSignature(tt.secret, tt.ts, tt.sig, tt.body, opsNow)
			if tt.ok != (err == nil) || (err != nil && !errors.Is(err, ErrReceiptAuth)) {
				t.Fatalf("err = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

// hook 生成签名正确的 webhook 请求；body 字段为空时用默认（J-1 DONE 12 亩，device_ts 一分钟前）。
func hook(farm, order uint64, body map[string]any) WebhookInput {
	b := map[string]any{"receipt_id": "rc-1", "device_type": "drone", "device_job_id": "J-1", "status": "DONE",
		"done_area_mu": 12, "device_ts": opsNow.Add(-time.Minute).Format(time.RFC3339)}
	for k, v := range body {
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
	}
	raw, _ := json.Marshal(b)
	ts := opsTS(0)
	return WebhookInput{FarmID: farm, OrderID: order, Timestamp: ts, Signature: SignReceipt(opsSecret, ts, raw), Body: raw,
		Route: "/api/v1/farms/:farm_id/orders/:order_id/receipts", RequestID: "req-1"}
}

func TestWebhook_Rejections(t *testing.T) {
	old := opsNow.Add(-time.Hour).Format(time.RFC3339)
	tests := []struct {
		name       string
		in         func() WebhookInput
		mut        func(f *opsFixture)
		wantErr    error
		wantDenied bool
	}{
		{name: "bad signature", wantErr: ErrReceiptAuth, in: func() WebhookInput {
			in := hook(opsFarm, opsOrder, nil)
			in.Signature = SignReceipt([]byte("other"), in.Timestamp, in.Body)
			return in
		}},
		{name: "signature window expired", wantErr: ErrReceiptAuth, in: func() WebhookInput {
			in := hook(opsFarm, opsOrder, nil)
			in.Timestamp = opsTS(-10 * time.Minute)
			in.Signature = SignReceipt(opsSecret, in.Timestamp, in.Body)
			return in
		}},
		{name: "body not json", wantErr: ErrBadInput, in: func() WebhookInput {
			in := hook(opsFarm, opsOrder, nil)
			in.Body = []byte("not json")
			in.Signature = SignReceipt(opsSecret, in.Timestamp, in.Body)
			return in
		}},
		{name: "missing device_ts", wantErr: ErrBadInput, in: func() WebhookInput { return hook(opsFarm, opsOrder, map[string]any{"device_ts": nil}) }},
		{name: "missing device_type", wantErr: ErrBadInput, in: func() WebhookInput { return hook(opsFarm, opsOrder, map[string]any{"device_type": ""}) }},
		{name: "FAILED without kind", wantErr: ErrBadInput, in: func() WebhookInput {
			return hook(opsFarm, opsOrder, map[string]any{"status": "FAILED", "done_area_mu": nil})
		}},
		{name: "cross farm job", wantErr: ErrForbidden, wantDenied: true, in: func() WebhookInput {
			return hook(opsFarm, opsOrder, map[string]any{"device_job_id": "J-2"})
		}},
		{name: "path farm differs from owner", wantErr: ErrForbidden, wantDenied: true, in: func() WebhookInput { return hook(11, opsOrder, nil) }},
		{name: "other order of same farm", wantErr: ErrForbidden, wantDenied: true, in: func() WebhookInput { return hook(opsFarm, 999, nil) }},
		{name: "unknown job is retryable", wantErr: ErrReceiptUnknownJob, in: func() WebhookInput {
			return hook(opsFarm, opsOrder, map[string]any{"device_job_id": "J-404"})
		}},
		{name: "lookup error", wantErr: errLookupBoom, mut: func(f *opsFixture) { f.w.lookupErr = errLookupBoom },
			in: func() WebhookInput { return hook(opsFarm, opsOrder, map[string]any{"device_ts": old}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			if tt.mut != nil {
				tt.mut(f)
			}
			_, err := f.svc(t).Webhook(t.Context(), tt.in())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if len(f.w.receipts) != 0 || len(f.w.transitions) != 0 || len(f.audit.recs) != 0 || len(f.idem.m) != 0 {
				t.Fatalf("rejected webhook wrote state: receipts %d transitions %d audit %d", len(f.w.receipts), len(f.w.transitions), len(f.audit.recs))
			}
			if got := len(f.denier.denials); (got == 1) != tt.wantDenied || got > 1 {
				t.Fatalf("denials = %+v, want denied %v", f.denier.denials, tt.wantDenied)
			}
			if tt.wantDenied {
				d := f.denier.denials[0]
				if d.Code != DenyResource || d.FarmID != tt.in().FarmID || d.RequestID != "req-1" || !strings.HasPrefix(d.Target, "order:") {
					t.Fatalf("denial = %+v", d)
				}
			}
		})
	}
}

var errLookupBoom = errors.New("lookup boom")

func TestWebhook_Unmatched(t *testing.T) {
	old := opsNow.Add(-time.Hour).Format(time.RFC3339)
	tests := []struct {
		name    string
		in      WebhookInput
		wantRaw bool
	}{
		{name: "job not found after late window", in: hook(opsFarm, opsOrder, map[string]any{"device_job_id": "J-404", "device_ts": old}), wantRaw: true},
		{name: "FAILED without job id", in: hook(opsFarm, opsOrder, map[string]any{"device_job_id": nil, "status": "FAILED",
			"failure_kind": "NOT_EXECUTED", "done_area_mu": nil}), wantRaw: true},
		{name: "ambiguous farm tenant keeps no raw", in: hook(12, opsOrder, map[string]any{"device_job_id": "J-404", "device_ts": old})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newOpsFixture()
			res, err := f.svc(t).Webhook(t.Context(), tt.in)
			if err != nil || res.Status != WebhookUnmatched || res.ReceiptID == "" {
				t.Fatalf("res = %+v, %v", res, err)
			}
			if len(f.audit.recs) != 1 {
				t.Fatalf("audit = %d", len(f.audit.recs))
			}
			a := f.audit.recs[0]
			if a.TenantID != 0 || a.FarmID != 0 || a.Action != string(model.AuditReceiptUnmatched) || a.Result != model.AuditResultFailed ||
				(a.RawObjectID != nil) != tt.wantRaw || a.Actor.Type != model.ActorDevice {
				t.Fatalf("audit = %+v", a)
			}
			if tt.wantRaw && (len(f.raw.puts) != 1 || f.raw.puts[0].Kind != model.RawReceiptRaw) {
				t.Fatalf("raw puts = %+v", f.raw.puts)
			}
			if len(f.w.receipts) != 0 || len(f.w.transitions) != 0 || !strings.Contains(f.log.String(), "alert="+alertReceiptUnmatched) {
				t.Fatalf("unmatched must not touch orders and must alert:\n%s", f.log)
			}
		})
	}
}

func TestWebhook_DoneAndReplay(t *testing.T) {
	f := newOpsFixture()
	s := f.svc(t)
	in := hook(opsFarm, opsOrder, nil)
	res, err := s.Webhook(t.Context(), in)
	if err != nil || res.Status != WebhookProcessed || res.Outcome != model.OutcomeApplied || res.OrderStatus != model.OrderDone {
		t.Fatalf("first = %+v, %v", res, err)
	}
	for i := 0; i < 2; i++ {
		again, err := s.Webhook(t.Context(), in)
		if err != nil || again.Status != WebhookReplayed {
			t.Fatalf("replay %d = %+v, %v", i, again, err)
		}
	}
	if len(f.w.receipts) != 1 || len(f.w.transitions) != 1 || len(f.w.topics(repository.TopicFieldHistoryAppend)) != 1 ||
		len(f.w.memories) != 1 || f.w.doneArea[opsOrder] != 12 {
		t.Fatalf("receipts %d transitions %d history %d memories %d area %v", len(f.w.receipts), len(f.w.transitions),
			len(f.w.topics(repository.TopicFieldHistoryAppend)), len(f.w.memories), f.w.doneArea[opsOrder])
	}
	for _, r := range f.w.receipts {
		if r.ReceiptID != "rc-1" || r.Source != model.SourceWebhook || r.RawObjectID == 0 {
			t.Fatalf("receipt row = %+v", r)
		}
	}
}
