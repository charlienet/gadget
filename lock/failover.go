package lock

import (
	"errors"
	"fmt"
)

// FailPolicy 定义后端服务失效时的兜底策略。
type FailPolicy uint8

const (
	// FailClosed 失效时拒绝（返回失败值）：适用于锁等"宁可失败也不放行"的能力。
	FailClosed FailPolicy = iota
	// FailOpen 失效时放行（返回成功值）：显式选择的风险由调用方承担。
	FailOpen
)

// ErrBackendUnavailable 表示后端不可用、已按 FailPolicy 执行兜底。
// 后端在自身原语中检测到"服务不可用"类错误时必须包装此哨兵，
// 核心据此触发兜底；命令级错误不得包装。
var ErrBackendUnavailable = errors.New("lock: backend unavailable")

// wrapUnavailable 包装原始错误为兜底哨兵错误。
// 若 err 已被后端包装为 ErrBackendUnavailable（errors.Is 命中），则原样返回，
// 避免叠加出「lock: backend unavailable: lock: backend unavailable: …」的冗余前缀。
// 两条分支的返回值均满足 errors.Is(result, ErrBackendUnavailable)，兼容性不变。
func wrapUnavailable(err error) error {
	if errors.Is(err, ErrBackendUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
}

// fallbackBool 返回 FailOpen 下的放行值（true）。
func (l *Lock) fallbackBool() bool {
	return l.policy == FailOpen
}

// fallbackErr 处理 Lock 阻塞路径的兜底。
func (l *Lock) fallbackErr(err error) error {
	if l.policy == FailOpen {
		return nil
	}
	return wrapUnavailable(err)
}
