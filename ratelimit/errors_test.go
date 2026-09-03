package ratelimit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestExceededErrorIsAndAs(t *testing.T) {
	err := error(&ExceededError{Key: "user:1", N: 2, RetryAfter: time.Second})
	if !errors.Is(err, ErrExceeded) {
		t.Fatal("ExceededError 必须满足 errors.Is(err, ErrExceeded)")
	}
	var xe *ExceededError
	if !errors.As(err, &xe) {
		t.Fatal("errors.As 必须可取出 *ExceededError")
	}
	if xe.Key != "user:1" || xe.N != 2 || xe.RetryAfter != time.Second {
		t.Fatalf("字段回填异常: %+v", xe)
	}
	for _, frag := range []string{"user:1", "n=2"} {
		if !strings.Contains(err.Error(), frag) {
			t.Fatalf("Error() 缺少 %q: %v", frag, err)
		}
	}
}

func TestInvalidArgumentSentinel(t *testing.T) {
	err := error(&invalidError{msg: "ratelimit: bad"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalidError 必须可判 ErrInvalidArgument，got %v", err)
	}
}

func TestWrapFailOpenDualSentinel(t *testing.T) {
	inner := fmt.Errorf("%w: dial tcp refused", ErrBackendUnavailable)
	err := wrapFailOpen(inner)

	if !errors.Is(err, ErrFailOpen) {
		t.Fatal("必须可判 ErrFailOpen")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatal("必须保留后端原始错误的可判性")
	}
	if !strings.Contains(err.Error(), "dial tcp refused") {
		t.Fatalf("必须保留后端错误原文: %v", err)
	}
	// 非兜底错误不得被误判。
	plain := errors.New("WRONGTYPE")
	if errors.Is(plain, ErrFailOpen) {
		t.Fatal("普通错误不应含 ErrFailOpen")
	}
}

func TestWrapUnavailable(t *testing.T) {
	// 未包装过的错误挂链哨兵。
	w := wrapUnavailable(errors.New("connection reset"))
	if !errors.Is(w, ErrBackendUnavailable) {
		t.Fatalf("应可判 ErrBackendUnavailable，got %v", w)
	}
	// 已含哨兵的错误原样返回（去重，不叠冗余前缀）。
	once := wrapUnavailable(errors.New("dial refused"))
	twice := wrapUnavailable(once)
	if twice != once {
		t.Fatalf("重复包装应原样返回，got %v", twice)
	}
	if strings.Count(once.Error(), ErrBackendUnavailable.Error()) != 1 {
		t.Fatalf("哨兵前缀不得重复出现: %v", once)
	}
}

func TestSentinelsIndependent(t *testing.T) {
	// 五个哨兵彼此独立（Is 不得互串）。
	all := []error{ErrExceeded, ErrBackendUnavailable, ErrFailOpen, ErrInvalidArgument, ErrClosed}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Fatalf("哨兵 %v 与 %v 互串", a, b)
			}
		}
	}
}
