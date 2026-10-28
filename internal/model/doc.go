// Package model 是五个核心契约（AdviceCard、WorkOrder、Citation、RunState、ApprovalTicket）、
// 表结构体、状态常量与 WorkOrder 迁移表。本包不依赖任何内部包（母文档 §4.6）。
//
// 契约 v1 于 P0 L1 冻结，JSON Schema 见 contracts/*.schema.json，变更规则见 docs/contracts.md：
// 改字段必须升 ContractVersion 并经契约 Owner 批准，同步所有下游。
package model

// ContractVersion 是契约版本号，与 contracts/*.schema.json 的 x-contract-version、$id 中的 /v{n}/ 一致。
const ContractVersion = 1
