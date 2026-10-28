package seed

import (
	"os"
	"path/filepath"
	"testing"
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
	tests := []struct {
		name    string
		path    string
		wantN   int
		wantErr bool
	}{
		{name: "repo seed file", path: "../../configs/seed/fields.yaml", wantN: 6},
		{name: "missing farm", path: write("a.yaml", "fields: []\n"), wantErr: true},
		{name: "duplicate code", wantErr: true, path: write("b.yaml", farm+
			"fields:\n  - {ext_field_code: x, crop: 水稻, sowing_date: \"2026-05-01\", area_mu: 1}\n"+
			"  - {ext_field_code: x, crop: 水稻, sowing_date: \"2026-05-01\", area_mu: 1}\n")},
		{name: "bad date", wantErr: true, path: write("c.yaml", farm+
			"fields:\n  - {ext_field_code: x, crop: 水稻, sowing_date: \"5/1\", area_mu: 1}\n")},
		{name: "zero area", wantErr: true, path: write("d.yaml", farm+
			"fields:\n  - {ext_field_code: x, crop: 水稻, sowing_date: \"2026-05-01\", area_mu: 0}\n")},
		{name: "no file", path: filepath.Join(dir, "none.yaml"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Load(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(d.Fields) != tt.wantN || d.Farm.Name != "示范农场1" || d.Farm.Region != "江苏" {
				t.Fatalf("data = %+v", d)
			}
		})
	}
}
