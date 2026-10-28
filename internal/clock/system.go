// Package clock 定义时间接缝。生产代码一律经注入的 Clock 取时间，不直接读系统时间
// （internal/archtest 扫描保证：time.Now 只允许出现在本包的 system.go）。
// 评测用的 FrozenClock 在 internal/evalkit/env。
package clock

import "time"

// Clock 返回当前时间（UTC）。
type Clock interface {
	Now() time.Time
}

// System 是生产实现：系统时间，统一转 UTC（存储一律 UTC，母文档 §4.6.2-9）。
type System struct{}

// Now 实现 Clock。
func (System) Now() time.Time { return time.Now().UTC() }
