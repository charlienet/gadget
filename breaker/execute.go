package breaker

// Execute 是一站式包装：Allow 判断 + 自动 Report 结果。
//
// Allow 拒绝时返回（零值, lastErr）——lastErr 原样返回（不包装、非哨兵），
// fn 不会被执行。fn 返回后自动 Report(err)，消除 TwoStep 的探测泄漏坑
// （Allow 放行后忘记 Success/Fail/Report）。
//
// fn panic 原样重新抛出给调用方（与 retry.Do 哲学一致，本包不吞 panic，
// 也不改变正常路径的返回值语义）。唯一差别：若 panic 发生在半开探测中，
// 抛出前先释放单飞标记并按探测失败语义回 Open（重置冷却），确保状态机
// 不会因标记永久滞留而死锁。Closed 状态下的 panic 不做任何计数。
func Execute[T any](b *Breaker, fn func() (T, error)) (T, error) {
	if err := b.Allow(); err != nil {
		var zero T
		return zero, err
	}

	var (
		result T
		err    error
	)
	returned := false
	defer func() {
		if returned {
			b.Report(err)
			return
		}
		// returned == false 只可能是 fn panic 中断执行（正常返回路径已置 true）：
		// 先释放半开单飞标记（等价探测失败），再把 panic 原样重抛，不吞 panic。
		if r := recover(); r != nil {
			b.probePanicked(r)
			panic(r)
		}
	}()

	result, err = fn()
	returned = true
	return result, err
}
