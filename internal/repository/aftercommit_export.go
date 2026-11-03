package repository

import "context"

// AfterCommit 在 tx 所属事务上登记提交后动作（P1 L7-B：service 层指标在事务提交后计数，回滚不计）。
// tx 不是经本包事务入口（Transactor / DecisionRepo / EventSink 等的 inTx）开启的返回 false，调用方自行降级。
func AfterCommit(tx *DB, fn func(context.Context)) bool {
	return onCommit(tx, fn)
}
