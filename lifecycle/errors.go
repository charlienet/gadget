package lifecycle

import (
	"errors"
	"strings"
)

// 哨兵错误，供调用方用 [errors.Is] 判定关闭过程中的各类失败。
var (
	// ErrTimeout 表示某个组件的 Stop 未在允许的步超时内返回。
	// 超时的 goroutine 不会被 kill，关闭流程继续执行后续步骤。
	ErrTimeout error = errors.New("lifecycle: step timeout")

	// ErrPanicked 表示某个组件的 Stop 发生了 panic，已被 recover 捕获。
	ErrPanicked error = errors.New("lifecycle: component panicked")

	// ErrBudgetExhausted 表示关闭总预算耗尽，剩余组件被跳过。
	ErrBudgetExhausted error = errors.New("lifecycle: total budget exhausted")
)

// SkippedError 列出因总预算耗尽而根本没有被调用 Stop 的组件名。
// 它 Unwrap 到 [ErrBudgetExhausted]，可用 errors.As 取出名单。
type SkippedError struct {
	Names []string
}

// Error 明示这些组件未被停止。
func (e *SkippedError) Error() string {
	return "lifecycle: components were NOT stopped (budget exhausted): " + strings.Join(e.Names, ", ")
}

// Unwrap 返回 [ErrBudgetExhausted]，使 errors.Is(err, ErrBudgetExhausted) 成立。
func (e *SkippedError) Unwrap() error { return ErrBudgetExhausted }
