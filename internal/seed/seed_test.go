package seed

import (
	"os"
	"path/filepath"
	"testing"

	"agri-steward/internal/model"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	farm := "farm: {tenant_id: 1, farm_id: 1, name: f}\n"
	field := func(code string) string {
		return "  - {ext_field_code: " + code + ", crop: 水稻, sowing_date: \"2026-05-01\", area_mu: 1}\n"
	}
	farm2 := "farms:\n  - {tenant_id: 1, farm_id: 2, name: g, fields: [{ext_field_code: y, crop: 水稻, sowing_date: \"2026-05-01\", area_mu: 1}]}\n"
	roles := func(lines ...string) string {
		s := "user_roles:\n"
		for _, l := range lines {
			s += "  - " + l + "\n"
		}
		return s
	}
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "missing farm", path: write("a.yaml", "fields: []\n"), wantErr: true},
		{name: "duplicate code", wantErr: true, path: write("b.yaml", farm+"fields:\n"+field("x")+field("x"))},
		{name: "bad date", wantErr: true, path: write("c.yaml", farm+
			"fields:\n  - {ext_field_code: x, crop: 水稻, sowing_date: \"5/1\", area_mu: 1}\n")},
		{name: "zero area", wantErr: true, path: write("d.yaml", farm+
			"fields:\n  - {ext_field_code: x, crop: 水稻, sowing_date: \"2026-05-01\", area_mu: 0}\n")},
		{name: "no file", path: filepath.Join(dir, "none.yaml"), wantErr: true},
		{name: "extra farm ok", path: write("e.yaml", farm+"fields:\n"+field("x")+farm2+
			roles("{tenant_id: 1, farm_id: 2, user_id: 2001, role: farm_manager}"))},
		{name: "extra farm duplicates primary", wantErr: true, path: write("f.yaml", farm+
			"farms:\n  - {tenant_id: 1, farm_id: 1, name: g}\n")},
		{name: "farm_id reused by another tenant", wantErr: true, path: write("f2.yaml", farm+
			"farms:\n  - {tenant_id: 2, farm_id: 1, name: g}\n")},
		{name: "extra farms share farm_id across tenants", wantErr: true, path: write("f3.yaml", farm+
			"farms:\n  - {tenant_id: 1, farm_id: 3, name: g}\n  - {tenant_id: 2, farm_id: 3, name: h}\n")},
		{name: "extra farm missing name", wantErr: true, path: write("g.yaml", farm+
			"farms:\n  - {tenant_id: 1, farm_id: 2}\n")},
		{name: "field code reused across farms", wantErr: true, path: write("h.yaml", farm+"fields:\n"+field("y")+farm2)},
		{name: "unknown role", wantErr: true, path: write("i.yaml", farm+
			roles("{tenant_id: 1, farm_id: 1, user_id: 1, role: boss}"))},
		{name: "platform role on farm", wantErr: true, path: write("j.yaml", farm+
			roles("{tenant_id: 1, farm_id: 1, user_id: 1, role: platform_admin}"))},
		{name: "farm role at platform", wantErr: true, path: write("k.yaml", farm+
			roles("{tenant_id: 0, farm_id: 0, user_id: 1, role: agronomist}"))},
		{name: "farm not seeded", wantErr: true, path: write("l.yaml", farm+
			roles("{tenant_id: 1, farm_id: 9, user_id: 1, role: agronomist}"))},
		{name: "zero user", wantErr: true, path: write("m.yaml", farm+
			roles("{tenant_id: 1, farm_id: 1, user_id: 0, role: agronomist}"))},
		{name: "duplicate binding", wantErr: true, path: write("n.yaml", farm+
			roles("{tenant_id: 1, farm_id: 1, user_id: 1, role: agronomist}", "{tenant_id: 1, farm_id: 1, user_id: 1, role: agronomist}"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_RepoFile(t *testing.T) {
	d, err := Load("../../configs/seed/fields.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Fields) != 6 || d.Farm.Name != "示范农场1" || d.Farm.Region != "江苏" {
		t.Fatalf("primary farm = %+v, %d fields", d.Farm, len(d.Fields))
	}
	if len(d.ExtraFarms) != 1 {
		t.Fatalf("extra farms = %d, want 1", len(d.ExtraFarms))
	}
	f2 := d.ExtraFarms[0]
	if f2.Farm.TenantID != 1 || f2.Farm.FarmID != 2 || f2.Farm.Region == "江苏" || len(f2.Fields) != 2 {
		t.Fatalf("farm 2 = %+v, %d fields", f2.Farm, len(f2.Fields))
	}
	for _, fl := range f2.Fields {
		if fl.TenantID != 1 || fl.FarmID != 2 {
			t.Fatalf("farm 2 field scope = (%d,%d)", fl.TenantID, fl.FarmID)
		}
	}
	want := []model.UserRole{
		{TenantID: 1, FarmID: 1, UserID: 1001, Role: model.RoleFarmManager, GrantedBy: 9001},
		{TenantID: 1, FarmID: 1, UserID: 1002, Role: model.RoleAgronomist, GrantedBy: 9001},
		{TenantID: 1, FarmID: 1, UserID: 1003, Role: model.RoleOperator, GrantedBy: 9001},
		{TenantID: 1, FarmID: 1, UserID: 1004, Role: model.RoleAgronomist, GrantedBy: 9001},
		{TenantID: 0, FarmID: 0, UserID: 9001, Role: model.RolePlatformAdmin},
		{TenantID: 1, FarmID: 2, UserID: 2001, Role: model.RoleFarmManager, GrantedBy: 9001},
	}
	if len(d.UserRoles) != len(want) {
		t.Fatalf("user_roles = %+v", d.UserRoles)
	}
	for i := range want {
		if d.UserRoles[i] != want[i] {
			t.Fatalf("user_roles[%d] = %+v, want %+v", i, d.UserRoles[i], want[i])
		}
	}
}
