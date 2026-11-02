package repository

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// UserRoleRepo 是身份解析读 user_role 的 SQL（P1 §6.2 定版 SQL 照抄；文档未给的另行标注）。
// 每次请求查唯一索引，不缓存（绑定撤销即时生效）。绑定只经种子 / 运维后台增删，不开放业务 API。
type UserRoleRepo struct{ DB *gorm.DB }

// ListRoles 返回用户在 (tenant_id, farm_id) 与平台 (0,0) 上的全部绑定。
func (r UserRoleRepo) ListRoles(ctx context.Context, tenantID, farmID, userID uint64) ([]model.RoleBinding, error) {
	var rows []model.RoleBinding
	err := r.DB.WithContext(ctx).Raw(
		`SELECT tenant_id, farm_id, role FROM user_role
 WHERE (tenant_id=? AND farm_id=? AND user_id=?) OR (tenant_id=0 AND farm_id=0 AND user_id=?)`,
		tenantID, farmID, userID, userID).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repository: list user_role user %d farm (%d,%d): %w", userID, tenantID, farmID, err)
	}
	return rows, nil
}

// FarmHasRole 判断农场是否有某角色的绑定（走 idx_farm_role；SQL 文档未给，自写）。
func (r UserRoleRepo) FarmHasRole(ctx context.Context, tenantID, farmID uint64, role model.Role) (bool, error) {
	var hit []int
	err := r.DB.WithContext(ctx).Raw(
		`SELECT 1 FROM user_role WHERE tenant_id=? AND farm_id=? AND role=? LIMIT 1`,
		tenantID, farmID, string(role)).Scan(&hit).Error
	if err != nil {
		return false, fmt.Errorf("repository: user_role farm (%d,%d) role %s: %w", tenantID, farmID, role, err)
	}
	return len(hit) > 0, nil
}

// FarmTenants 返回 farm_id 在 farm_profile 中的归属租户（最多 2 个，调用方据此判定不存在 / 唯一 / 歧义）。
// 身份解析在 Scope 建立之前执行，故不带 tenant 条件；SQL 文档未给，自写（farm_profile 量小，无 farm_id 单列索引）。
func (r UserRoleRepo) FarmTenants(ctx context.Context, farmID uint64) ([]uint64, error) {
	var tenants []uint64
	err := r.DB.WithContext(ctx).Raw(`SELECT tenant_id FROM farm_profile WHERE farm_id=? LIMIT 2`, farmID).
		Scan(&tenants).Error
	if err != nil {
		return nil, fmt.Errorf("repository: farm_profile owner of farm %d: %w", farmID, err)
	}
	return tenants, nil
}

// UpsertUserRoleTx 按 uk_user_role upsert 一条绑定（种子导入用，只在调用方事务内执行）。
func (UserRoleRepo) UpsertUserRoleTx(ctx context.Context, tx *gorm.DB, ur model.UserRole, now time.Time) error {
	_, err := execInTx(ctx, tx,
		`INSERT INTO user_role (tenant_id, farm_id, user_id, role, granted_by, created_at) VALUES (?, ?, ?, ?, ?, ?) AS new
  ON DUPLICATE KEY UPDATE granted_by=new.granted_by`,
		ur.TenantID, ur.FarmID, ur.UserID, string(ur.Role), ur.GrantedBy, now)
	if err != nil {
		return fmt.Errorf("repository: upsert user_role (%d,%d) user %d %s: %w", ur.TenantID, ur.FarmID, ur.UserID, ur.Role, err)
	}
	return nil
}
