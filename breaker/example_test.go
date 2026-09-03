package breaker

import (
	"errors"
	"fmt"
	"time"
)

// ExampleExecute 演示推荐的一站式用法：Allow 判断 + 自动 Report。
func ExampleExecute() {
	b := New(WithThreshold(1), WithCooldown(10*time.Millisecond))

	// 首次调用失败：阈值 1 → 直接进入 Open
	_, err := Execute(b, func() (int, error) {
		return 0, errors.New("dial tcp: connection refused")
	})
	fmt.Println("first err:", err)

	// Open：快速失败，fn 不执行，lastErr 原样返回
	v, err := Execute(b, func() (int, error) {
		fmt.Println("!! 不应执行")
		return 42, nil
	})
	fmt.Printf("rejected: v=%d err=%v\n", v, err)

	// 冷却结束：探测放行，成功自动闭合恢复
	time.Sleep(15 * time.Millisecond)
	v, err = Execute(b, func() (int, error) {
		return 42, nil
	})
	fmt.Printf("probe: v=%d err=%v state=%v\n", v, err, b.State())

	// Output:
	// first err: dial tcp: connection refused
	// rejected: v=0 err=dial tcp: connection refused
	// probe: v=42 err=<nil> state=closed
}

// ExampleBreaker_Report 演示 hook/中间件场景的 TwoStep 用法：
// Allow 与 Report 分离。注意契约：Allow 放行后必须最终调用
// Success/Fail/Report 之一，否则半开探测标记泄漏。
func ExampleBreaker_Report() {
	counted := errors.New("connection refused") // 连接类：计入熔断
	neutral := errors.New("WRONGTYPE")          // 命令类：不计入

	b := New(
		WithThreshold(1),
		WithCooldown(10*time.Millisecond),
		WithClassifier(func(err error) bool { return errors.Is(err, counted) }),
	)

	// 命令类错误不触发熔断
	if err := b.Allow(); err == nil {
		b.Report(neutral) // 执行业务后必须上报结果
	}
	fmt.Println("after neutral:", b.State())

	// 连接类错误达阈值 → Open
	if err := b.Allow(); err == nil {
		b.Report(counted)
	}
	fmt.Println("after counted:", b.State())

	// Open：快速失败，返回 lastErr 原文
	fmt.Println("allow:", b.Allow())

	// 冷却结束：放行半开探测，探测失败回 Open
	time.Sleep(15 * time.Millisecond)
	if err := b.Allow(); err == nil {
		b.Report(counted)
	}
	fmt.Println("after failed probe:", b.State())

	// Output:
	// after neutral: closed
	// after counted: open
	// allow: connection refused
	// after failed probe: open
}
