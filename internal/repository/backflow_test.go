package repository

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
)

// TestBackflowFinishDraftStatus：只允许推进到 DRAFTED（须带 draft_ref）或 PII_FAILED；MERGED / DROPPED 待专家流程。
func TestBackflowFinishDraftStatus(t *testing.T) {
	s := model.Scope{TenantID: 1, FarmID: 2}
	for _, c := range []struct{ status, ref string }{
		{model.BackflowDrafted, ""}, {model.BackflowMerged, "bf-x"}, {model.BackflowDropped, ""}, {"", ""},
	} {
		err := BackflowRepo{}.FinishDraft(context.Background(), s, 1, c.status, c.ref, time.Unix(0, 0))
		if !errors.Is(err, ErrBackflowStatus) {
			t.Errorf("FinishDraft(%q, %q) err = %v, want ErrBackflowStatus", c.status, c.ref, err)
		}
	}
}

// TestPresignedFetcher：200 取回正文；非 200 报错且错误里不带预签名 URL（URL 即凭据）；超限报 ErrRawTooLarge。
func TestPresignedFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"data":{}}`)) //nolint:errcheck // 测试桩
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", RawFetchMaxBytes+1))) //nolint:errcheck // 测试桩
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	f := PresignedFetcher{Client: srv.Client()}
	ctx := context.Background()
	body, err := f.Fetch(ctx, srv.URL+"/ok")
	if err != nil || string(body) != `{"data":{}}` {
		t.Fatalf("ok = %q, %v", body, err)
	}
	secret := srv.URL + "/denied?X-Amz-Signature=deadbeef"
	if _, err := f.Fetch(ctx, secret); err == nil || strings.Contains(err.Error(), "deadbeef") {
		t.Fatalf("denied err = %v, want error without url", err)
	}
	if _, err := f.Fetch(ctx, srv.URL+"/big"); !errors.Is(err, ErrRawTooLarge) {
		t.Fatalf("big err = %v, want ErrRawTooLarge", err)
	}
}
