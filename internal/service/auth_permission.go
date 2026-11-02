package service

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/model"
)

// 权限矩阵（P1 §6.2 功能任务 6）：角色 × 操作，未声明即拒绝。由 policy/permission_matrix.yaml 加载，启动时校验。

// Operation 是权限矩阵中的一个操作。
type Operation string

// 已登记的操作；新增操作须同步 policy/permission_matrix.yaml 与 controller 路由表。
const (
	OpAsk                     Operation = "ask"                       // 提问（POST /ask）
	OpAdviceRead              Operation = "advice.read"               // 查看建议 / run / 待审批列表 / 看板
	OpApprovalSubmit          Operation = "approval.submit"           // 提交审批（暂无独立路由，开票由主链路发起）
	OpApprovalDecidePesticide Operation = "approval.decide.pesticide" // 审批用药类作业单（SPRAY）
	OpApprovalDecideOther     Operation = "approval.decide.other"     // 审批非用药类作业单
	OpOrderCancel             Operation = "order.cancel"              // 中止作业（POST /orders/:order_id/cancel）
	OpAuditExport             Operation = "audit.export"              // 导出审计（暂无路由）
	OpRawRead                 Operation = "raw.read"                  // 读 raw_store 原文
	OpFlagWrite               Operation = "flag.write"                // 改开关（admin 路由，只限 platform_admin）
	OpOrderReconcile          Operation = "order.reconcile"           // 人工对账（POST /orders/:order_id/reconcile，P1-L4-B）
	OpRejectionExport         Operation = "rejection.export"          // 驳回原因导出（GET /rejections，P1-L5）
)

// Operations 是全部已登记操作（矩阵校验与测试枚举用）。
var Operations = []Operation{OpAsk, OpAdviceRead, OpApprovalSubmit, OpApprovalDecidePesticide, OpApprovalDecideOther,
	OpOrderCancel, OpAuditExport, OpRawRead, OpFlagWrite, OpOrderReconcile, OpRejectionExport}

// Roles 是全部已登记角色（矩阵校验与测试枚举用）。
var Roles = []model.Role{model.RoleFarmer, model.RoleFarmManager, model.RoleAgronomist, model.RoleOperator,
	model.RoleAdmin, model.RoleAuditor, model.RolePlatformAdmin, model.RoleKBExpert, model.RoleLegal}

// PermissionMatrixVersion 是本包支持的 permission_matrix.yaml 版本。
const PermissionMatrixVersion = 1

// ErrBadPermissionMatrix 是 permission_matrix.yaml 不合法（加载期，启动失败）。
var ErrBadPermissionMatrix = errors.New("service: invalid permission_matrix")

// PermissionMatrix 是加载后的矩阵。构造后只读，并发安全。
type PermissionMatrix struct {
	grants map[model.Role]map[Operation]bool
}

type permissionFile struct {
	Version int                        `yaml:"version"`
	Roles   map[model.Role][]Operation `yaml:"roles"`
}

func (op Operation) valid() bool {
	for _, o := range Operations {
		if o == op {
			return true
		}
	}
	return false
}

// LoadPermissionMatrix 读取并校验 permission_matrix.yaml。
func LoadPermissionMatrix(path string) (*PermissionMatrix, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("service: read permission_matrix: %w", err)
	}
	m, err := ParsePermissionMatrix(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// ParsePermissionMatrix 解析并校验：未知字段 / 重复角色键（yaml 解码即报错）、未知角色、未知操作、角色内重复操作、
// flag.write 授给 platform_admin 以外的角色（含 kb_expert / legal），均视为不合法。未列出的角色视为无任何权限。
func ParsePermissionMatrix(b []byte) (*PermissionMatrix, error) {
	var f permissionFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrBadPermissionMatrix, err)
	}
	if f.Version != PermissionMatrixVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrBadPermissionMatrix, f.Version, PermissionMatrixVersion)
	}
	m := &PermissionMatrix{grants: make(map[model.Role]map[Operation]bool, len(f.Roles))}
	for role, ops := range f.Roles {
		if !role.Valid() {
			return nil, fmt.Errorf("%w: unknown role %q", ErrBadPermissionMatrix, role)
		}
		set := make(map[Operation]bool, len(ops))
		for _, op := range ops {
			switch {
			case !op.valid():
				return nil, fmt.Errorf("%w: role %s: unknown operation %q", ErrBadPermissionMatrix, role, op)
			case set[op]:
				return nil, fmt.Errorf("%w: role %s: duplicate operation %q", ErrBadPermissionMatrix, role, op)
			case op == OpFlagWrite && role != model.RolePlatformAdmin: // GLOBAL / TENANT 开关只允许平台管理员修改（§6.2 功能任务 6）
				return nil, fmt.Errorf("%w: role %s: %s is platform_admin-only", ErrBadPermissionMatrix, role, op)
			}
			set[op] = true
		}
		m.grants[role] = set
	}
	return m, nil
}

// Allows 判断 roles 中任一角色被授予 op；nil 矩阵与未声明组合一律拒绝。
func (m *PermissionMatrix) Allows(op Operation, roles ...model.Role) bool {
	if m == nil {
		return false
	}
	for _, r := range roles {
		if m.grants[r][op] {
			return true
		}
	}
	return false
}
