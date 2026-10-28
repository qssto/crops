package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"agri-steward/internal/model"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func newTestAuth(t *testing.T, now time.Time) *AuthService {
	t.Helper()
	grants := map[uint64][]model.Grant{
		1001: {{TenantID: 1, FarmID: 1, Roles: []model.Role{model.RoleFarmManager}}},
		9001: {{TenantID: 0, FarmID: 0, Roles: []model.Role{model.RolePlatformAdmin}}},
	}
	a, err := NewAuthService(testSecret, grants, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAuthenticate(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	a := newTestAuth(t, now)
	good, err := a.Issue(1001, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewAuthService("ffffffffffffffffffffffffffffffff", nil, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	forged, err := other.Issue(1001, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := a.Issue(1001, -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{Subject: "1001",
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	noExp, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{Subject: "1001"}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		token   string
		wantUID uint64
		wantErr error
	}{
		{name: "ok", token: good, wantUID: 1001},
		{name: "missing", token: "", wantErr: ErrUnauthenticated},
		{name: "garbage", token: "abc.def.ghi", wantErr: ErrUnauthenticated},
		{name: "wrong secret", token: forged, wantErr: ErrUnauthenticated},
		{name: "expired", token: expired, wantErr: ErrUnauthenticated},
		{name: "alg none", token: none, wantErr: ErrUnauthenticated},
		{name: "no exp", token: noExp, wantErr: ErrUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, err := a.Authenticate(tt.token)
			if !errors.Is(err, tt.wantErr) || uid != tt.wantUID {
				t.Fatalf("uid=%d err=%v, want %d %v", uid, err, tt.wantUID, tt.wantErr)
			}
		})
	}
}

func TestResolveScope(t *testing.T) {
	a := newTestAuth(t, time.Now().UTC())
	tests := []struct {
		name    string
		uid     uint64
		farm    uint64
		wantErr error
	}{
		{name: "granted farm", uid: 1001, farm: 1},
		{name: "other farm", uid: 1001, farm: 2, wantErr: ErrForbidden},
		{name: "farm 0", uid: 1001, farm: 0, wantErr: ErrForbidden},
		{name: "platform admin not on farm path", uid: 9001, farm: 1, wantErr: ErrForbidden},
		{name: "unknown user", uid: 42, farm: 1, wantErr: ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := a.ResolveScope(tt.uid, tt.farm)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && (s.TenantID != 1 || s.FarmID != 1 || s.UserID != 1001 || !s.HasRole(model.RoleFarmManager)) {
				t.Fatalf("scope = %+v", s)
			}
		})
	}
}

func TestNewAuthService_WeakSecret(t *testing.T) {
	if _, err := NewAuthService("short", nil, fixedClock{}); !errors.Is(err, ErrWeakSecret) {
		t.Fatalf("err = %v", err)
	}
}

func TestScopeContext(t *testing.T) {
	if _, ok := ScopeFrom(context.Background()); ok {
		t.Fatal("empty ctx must have no scope")
	}
	ctx := WithScope(context.Background(), model.Scope{FarmID: 1})
	if s, ok := ScopeFrom(ctx); !ok || s.FarmID != 1 {
		t.Fatalf("scope = %+v %v", s, ok)
	}
}
