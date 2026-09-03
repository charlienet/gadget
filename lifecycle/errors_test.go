package lifecycle

import (
	"errors"
	"strings"
	"testing"
)

func TestSkippedErrorContract(t *testing.T) {
	e := &SkippedError{Names: []string{"a", "b"}}
	if !strings.Contains(e.Error(), "were NOT stopped") {
		t.Fatalf("Error() 文案应明示未停止，实得 %q", e.Error())
	}
	for _, n := range e.Names {
		if !strings.Contains(e.Error(), n) {
			t.Fatalf("Error() 应包含组件名 %q，实得 %q", n, e.Error())
		}
	}
	if !errors.Is(e.Unwrap(), ErrBudgetExhausted) {
		t.Fatalf("Unwrap() 应返回 ErrBudgetExhausted，实得 %v", e.Unwrap())
	}
	if !errors.Is(error(e), ErrBudgetExhausted) {
		t.Fatal("errors.Is(*SkippedError, ErrBudgetExhausted) 应为 true")
	}
}

// 三哨兵错误彼此独立且可被 errors.Is 识别。
func TestSentinelErrorsDistinct(t *testing.T) {
	all := []error{ErrTimeout, ErrPanicked, ErrBudgetExhausted}
	for i, a := range all {
		if a == nil {
			t.Fatalf("哨兵 %d 不应为 nil", i)
		}
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Fatalf("哨兵 %d 与 %d 不应互相匹配", i, j)
			}
		}
	}
}
