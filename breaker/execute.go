package breaker

// Execute 是一站式包装：Allow 判断 + 自动 Report 结果。
//
// Allow 拒绝时返回（零值, lastErr）——lastErr 原样返回（不包装、非哨兵），
// fn 不会被执行。fn 返回后自动 Report(err)，消除 TwoStep 的探测泄漏坑
// （Allow 放行后忘记 Success/Fail/Report）。
//
// fn panic 直接穿透、不计数（与 retry.Do 哲学一致，本包不做 recover）。
// 注意：半开探测中 panic 会滞留探测标记（单飞被占用，后续 Allow 持续
// 拒绝），调用方应自行保证 fn 不 panic。
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
		}
	}()

	result, err = fn()
	returned = true
	return result, err
}
