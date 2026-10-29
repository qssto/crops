package repository

import (
	"context"
	"sync"

	"gorm.io/gorm"
)

// commitHooks 收集一次事务内登记的提交后动作（SSE 发布等）；事务提交成功后按登记顺序执行，回滚则丢弃。
type commitHooks struct {
	mu  sync.Mutex
	fns []func(context.Context)
}

type commitHooksKey struct{}

// inTx 开事务执行 fn，提交成功后执行 fn 内经 onCommit 登记的动作。
// 本包所有会调用 EventRepo.AppendTx 的事务入口（Transactor / DecisionRepo / EventSink / OrderDraftRepo）都走这里。
func inTx(ctx context.Context, db *gorm.DB, fn func(tx *gorm.DB) error) error {
	h := &commitHooks{}
	if err := db.WithContext(context.WithValue(ctx, commitHooksKey{}, h)).Transaction(fn); err != nil {
		return err
	}
	h.mu.Lock()
	fns := h.fns
	h.fns = nil
	h.mu.Unlock()
	for _, f := range fns {
		f(ctx)
	}
	return nil
}

// onCommit 在 tx 所属事务上登记提交后动作；tx 不是经 inTx 开启的返回 false（调用方自行决定降级）。
func onCommit(tx *gorm.DB, fn func(context.Context)) bool {
	if tx == nil || tx.Statement == nil || tx.Statement.Context == nil {
		return false
	}
	h, ok := tx.Statement.Context.Value(commitHooksKey{}).(*commitHooks)
	if !ok {
		return false
	}
	h.mu.Lock()
	h.fns = append(h.fns, fn)
	h.mu.Unlock()
	return true
}
