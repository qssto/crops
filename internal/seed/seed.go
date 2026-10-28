// Package seed 读取地块种子文件（configs/seed/fields.yaml）并按 uk_farm / uk_ext upsert；
// cmd/migrate seed 与 evalkit trial schema 共用。
package seed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// File 是种子文件结构。
type File struct {
	Farm struct {
		TenantID  uint64   `yaml:"tenant_id"`
		FarmID    uint64   `yaml:"farm_id"`
		Name      string   `yaml:"name"`
		Region    string   `yaml:"region"`
		Timezone  string   `yaml:"timezone"`
		MainCrops []string `yaml:"main_crops"`
	} `yaml:"farm"`
	Fields []struct {
		ExtFieldCode string  `yaml:"ext_field_code"`
		Crop         string  `yaml:"crop"`
		SowingDate   string  `yaml:"sowing_date"`
		AreaMu       float64 `yaml:"area_mu"`
	} `yaml:"fields"`
}

// Data 是校验后的种子数据。
type Data struct {
	Farm   model.FarmProfile
	Fields []model.FieldProfile
}

// Load 读取并校验种子文件。
func Load(path string) (*Data, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("seed: read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("seed: parse %s: %w", path, err)
	}
	if f.Farm.TenantID == 0 || f.Farm.FarmID == 0 || f.Farm.Name == "" {
		return nil, fmt.Errorf("seed: %s: farm tenant_id / farm_id / name required", path)
	}
	d := &Data{Farm: model.FarmProfile{TenantID: f.Farm.TenantID, FarmID: f.Farm.FarmID, Name: f.Farm.Name,
		Region: f.Farm.Region, Timezone: f.Farm.Timezone, MainCrops: f.Farm.MainCrops}}
	seen := map[string]bool{}
	var errs []error
	for i, fl := range f.Fields {
		sow, err := time.Parse(time.DateOnly, fl.SowingDate)
		switch {
		case fl.ExtFieldCode == "":
			errs = append(errs, fmt.Errorf("fields[%d]: ext_field_code required", i))
		case seen[fl.ExtFieldCode]:
			errs = append(errs, fmt.Errorf("fields[%d]: duplicate %s", i, fl.ExtFieldCode))
		case err != nil:
			errs = append(errs, fmt.Errorf("fields[%d]: sowing_date: %w", i, err))
		case fl.AreaMu <= 0:
			errs = append(errs, fmt.Errorf("fields[%d]: area_mu must be > 0", i))
		}
		seen[fl.ExtFieldCode] = true
		d.Fields = append(d.Fields, model.FieldProfile{TenantID: d.Farm.TenantID, FarmID: d.Farm.FarmID,
			ExtFieldCode: fl.ExtFieldCode, Crop: fl.Crop, SowingDate: sow, AreaMu: fl.AreaMu})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("seed: %s: %w", path, err)
	}
	return d, nil
}

// Apply 在一个事务内 upsert 农场与全部地块，可重复执行。
func Apply(ctx context.Context, db *repository.DB, d *Data, now time.Time) error {
	repo := repository.ProfileRepo{}
	return db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		if err := repo.UpsertFarm(ctx, tx, d.Farm, now); err != nil {
			return err
		}
		for _, f := range d.Fields {
			if err := repo.UpsertField(ctx, tx, f, now); err != nil {
				return err
			}
		}
		return nil
	})
}
