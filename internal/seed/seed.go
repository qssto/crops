// Package seed 读取种子文件（configs/seed/fields.yaml）：主农场 + 扩展农场与地块按 uk_farm / uk_ext upsert，
// 角色绑定按 uk_user_role upsert；cmd/migrate seed 与 evalkit trial schema 共用。
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

// farmFile 是一个农场（含地块）的种子结构；主农场的地块写在顶层 fields，扩展农场写在自身 fields。
type farmFile struct {
	TenantID  uint64      `yaml:"tenant_id"`
	FarmID    uint64      `yaml:"farm_id"`
	Name      string      `yaml:"name"`
	Region    string      `yaml:"region"`
	Timezone  string      `yaml:"timezone"`
	MainCrops []string    `yaml:"main_crops"`
	Fields    []fieldFile `yaml:"fields"`
}

type fieldFile struct {
	ExtFieldCode string  `yaml:"ext_field_code"`
	Crop         string  `yaml:"crop"`
	SowingDate   string  `yaml:"sowing_date"`
	AreaMu       float64 `yaml:"area_mu"`
}

// File 是种子文件结构。
type File struct {
	Farm      farmFile    `yaml:"farm"`
	Fields    []fieldFile `yaml:"fields"`
	Farms     []farmFile  `yaml:"farms"` // P1-L1-A：扩展农场（跨农场隔离测试用）
	UserRoles []struct {
		TenantID  uint64     `yaml:"tenant_id"`
		FarmID    uint64     `yaml:"farm_id"`
		UserID    uint64     `yaml:"user_id"`
		Role      model.Role `yaml:"role"`
		GrantedBy uint64     `yaml:"granted_by"`
	} `yaml:"user_roles"`
	Flags []flagEntry `yaml:"flags"` // P1-L0
}

// FarmData 是校验后的一个农场及其地块。
type FarmData struct {
	Farm   model.FarmProfile
	Fields []model.FieldProfile
}

// Data 是校验后的种子数据；Farm / Fields 是主农场（评测与单农场流程默认用它）。
type Data struct {
	Farm       model.FarmProfile
	Fields     []model.FieldProfile
	ExtraFarms []FarmData
	UserRoles  []model.UserRole
	Flags      []model.FlagWrite // P1-L0：feature_flag 行（At 在 Apply 时填）
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
	d, err := parse(&f)
	if err != nil {
		return nil, fmt.Errorf("seed: %s: %w", path, err)
	}
	return d, nil
}

// farmKey 是农场唯一键（uk_farm）。
type farmKey struct{ tenant, farm uint64 }

// parse 校验并转换种子文件；错误一次列全。
func parse(f *File) (*Data, error) {
	var errs []error
	primary, fields, err := parseFarm("farm", f.Farm, f.Fields)
	if err != nil {
		return nil, err // 主农场无效时后续校验无意义
	}
	d := &Data{Farm: primary, Fields: fields}
	farms := map[farmKey]bool{{primary.TenantID, primary.FarmID}: true}
	// farm_id 全局唯一（鉴权按 farm_id 反查归属租户，同号多租户会失败关闭）；库内唯一索引待迁移补齐
	owners := map[uint64]uint64{primary.FarmID: primary.TenantID}
	codes := map[string]bool{}
	for _, fl := range fields {
		codes[fl.ExtFieldCode] = true
	}
	for i, ff := range f.Farms {
		where := fmt.Sprintf("farms[%d]", i)
		fp, fls, err := parseFarm(where, ff, ff.Fields)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		k := farmKey{fp.TenantID, fp.FarmID}
		if owner, dup := owners[fp.FarmID]; dup {
			errs = append(errs, fmt.Errorf("%s: farm_id %d already seeded under tenant %d (farm_id must be globally unique)",
				where, fp.FarmID, owner))
			continue
		}
		farms[k], owners[fp.FarmID] = true, fp.TenantID
		for _, fl := range fls {
			if codes[fl.ExtFieldCode] { // mcp mock 数据按 ext_field_code 取，跨农场也不得重复
				errs = append(errs, fmt.Errorf("%s: ext_field_code %s used by another farm", where, fl.ExtFieldCode))
			}
			codes[fl.ExtFieldCode] = true
		}
		d.ExtraFarms = append(d.ExtraFarms, FarmData{Farm: fp, Fields: fls})
	}
	d.UserRoles, err = parseUserRoles(f, farms)
	errs = append(errs, err)
	d.Flags, err = loadFlags(f.Flags)
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return d, nil
}

// parseFarm 校验一个农场及其地块。
func parseFarm(where string, ff farmFile, raw []fieldFile) (model.FarmProfile, []model.FieldProfile, error) {
	if ff.TenantID == 0 || ff.FarmID == 0 || ff.Name == "" {
		return model.FarmProfile{}, nil, fmt.Errorf("%s: tenant_id / farm_id / name required", where)
	}
	farm := model.FarmProfile{TenantID: ff.TenantID, FarmID: ff.FarmID, Name: ff.Name,
		Region: ff.Region, Timezone: ff.Timezone, MainCrops: ff.MainCrops}
	seen := map[string]bool{}
	var errs []error
	var out []model.FieldProfile
	for i, fl := range raw {
		sow, err := time.Parse(time.DateOnly, fl.SowingDate)
		switch {
		case fl.ExtFieldCode == "":
			errs = append(errs, fmt.Errorf("%s fields[%d]: ext_field_code required", where, i))
		case seen[fl.ExtFieldCode]:
			errs = append(errs, fmt.Errorf("%s fields[%d]: duplicate %s", where, i, fl.ExtFieldCode))
		case err != nil:
			errs = append(errs, fmt.Errorf("%s fields[%d]: sowing_date: %w", where, i, err))
		case fl.AreaMu <= 0:
			errs = append(errs, fmt.Errorf("%s fields[%d]: area_mu must be > 0", where, i))
		}
		seen[fl.ExtFieldCode] = true
		out = append(out, model.FieldProfile{TenantID: farm.TenantID, FarmID: farm.FarmID,
			ExtFieldCode: fl.ExtFieldCode, Crop: fl.Crop, SowingDate: sow, AreaMu: fl.AreaMu})
	}
	return farm, out, errors.Join(errs...)
}

// parseUserRoles 校验角色绑定：角色已知；平台角色只绑 (0,0)、农场角色只绑种子内的农场；user_id 非 0；不重复。
func parseUserRoles(f *File, farms map[farmKey]bool) ([]model.UserRole, error) {
	type key struct {
		farmKey
		user uint64
		role model.Role
	}
	seen := map[key]bool{}
	var errs []error
	var out []model.UserRole
	for i, ur := range f.UserRoles {
		k := key{farmKey{ur.TenantID, ur.FarmID}, ur.UserID, ur.Role}
		platform := ur.TenantID == 0 && ur.FarmID == 0
		switch {
		case ur.UserID == 0:
			errs = append(errs, fmt.Errorf("user_roles[%d]: user_id required", i))
		case !ur.Role.Valid():
			errs = append(errs, fmt.Errorf("user_roles[%d]: unknown role %q", i, ur.Role))
		case ur.Role.Platform() != platform:
			errs = append(errs, fmt.Errorf("user_roles[%d]: role %s must bind at %s", i, ur.Role, bindAt(ur.Role)))
		case !platform && !farms[k.farmKey]:
			errs = append(errs, fmt.Errorf("user_roles[%d]: farm (%d,%d) not in seed", i, ur.TenantID, ur.FarmID))
		case seen[k]:
			errs = append(errs, fmt.Errorf("user_roles[%d]: duplicate binding", i))
		default:
			seen[k] = true
			out = append(out, model.UserRole{TenantID: ur.TenantID, FarmID: ur.FarmID, UserID: ur.UserID,
				Role: ur.Role, GrantedBy: ur.GrantedBy})
		}
	}
	return out, errors.Join(errs...)
}

func bindAt(r model.Role) string {
	if r.Platform() {
		return "platform (0,0)"
	}
	return "a seeded farm"
}

// Apply 在一个事务内 upsert 全部农场、地块与角色绑定，并预建 (0,0) 与各农场的审计链 head（P1 L7），可重复执行。
func Apply(ctx context.Context, db *repository.DB, d *Data, now time.Time) error {
	profiles, roles := repository.ProfileRepo{}, repository.UserRoleRepo{}
	farms := append([]FarmData{{Farm: d.Farm, Fields: d.Fields}}, d.ExtraFarms...)
	return db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		if err := repository.InitChainHeadTx(ctx, tx, repository.ChainKey{}, now); err != nil {
			return err
		}
		for _, fd := range farms {
			if err := profiles.UpsertFarm(ctx, tx, fd.Farm, now); err != nil {
				return err
			}
			if err := repository.InitChainHeadTx(ctx, tx,
				repository.ChainKey{TenantID: fd.Farm.TenantID, FarmID: fd.Farm.FarmID}, now); err != nil {
				return err
			}
			// 租户级链 (tenant, 0)：TENANT 级开关变更等不落到具体农场的审计。
			if err := repository.InitChainHeadTx(ctx, tx, repository.ChainKey{TenantID: fd.Farm.TenantID}, now); err != nil {
				return err
			}
			for _, f := range fd.Fields {
				if err := profiles.UpsertField(ctx, tx, f, now); err != nil {
					return err
				}
			}
		}
		for _, ur := range d.UserRoles {
			if err := roles.UpsertUserRoleTx(ctx, tx, ur, now); err != nil {
				return err
			}
		}
		return applyFlags(ctx, tx, d.Flags, now)
	})
}
