package lifecycle

import (
	"log/slog"
	"os"
	"syscall"
	"time"
)

const defaultStepTimeout = 5 * time.Second

// defaultSignals 为触发关闭的默认 OS 信号。
var defaultSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT}

// options 保存 Manager 的可配置项，在 [New] 时统一校验。
type options struct {
	stepTimeout  time.Duration
	totalTimeout time.Duration // 0 表示未设置，不设总时限
	signals      []os.Signal
	logger       *slog.Logger
}

// Option 用于配置 [Manager]。所有校验都在 [New] 应用选项时进行，
// 非法配置直接 panic——选项错误属于程序期错误，不应延后到运行期暴露。
type Option func(*options)

// WithStepTimeout 设置单个组件 Stop 的最大允许耗时，默认 5 秒。
// d <= 0 时 panic。
func WithStepTimeout(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			panic("lifecycle: WithStepTimeout requires d > 0")
		}
		o.stepTimeout = d
	}
}

// WithTotalTimeout 设置整个关闭流程的总预算（根 ctx 的超时）。默认为 0，
// 表示未设置总时限、不对整体设限。d <= 0 时 panic。
func WithTotalTimeout(d time.Duration) Option {
	return func(o *options) {
		if d <= 0 {
			panic("lifecycle: WithTotalTimeout requires d > 0")
		}
		o.totalTimeout = d
	}
}

// WithSignals 设置触发关闭的 OS 信号，默认为 SIGTERM、SIGINT。
// 传入零个信号、或信号切片中含 nil 元素时 panic。
func WithSignals(sigs ...os.Signal) Option {
	return func(o *options) {
		if len(sigs) == 0 {
			panic("lifecycle: WithSignals requires at least one signal")
		}
		for _, s := range sigs {
			if s == nil {
				panic("lifecycle: WithSignals does not accept nil signal")
			}
		}
		o.signals = append([]os.Signal(nil), sigs...)
	}
}

// WithLogger 设置关闭过程日志的输出目标，默认不输出。l 为 nil 时 panic。
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l == nil {
			panic("lifecycle: WithLogger does not accept nil logger")
		}
		o.logger = l
	}
}
