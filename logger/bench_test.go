package logger

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// bench_test.go —— logger 模块性能取证基准。
//
// 运行：go test -bench . -benchmem -run '^$' -benchtime=1s
// 说明：本文件仅新增，不改任何既有源；内部测试（package logger），可直达未导出符号
// （newFileTextHandler / newFileHandler / ParseFileFormat 等）。

// benchDiscard 所有基准统一输出目标（丢弃，隔离 IO 成本，纯测格式化/装饰/入队路径）。
func benchDiscard() io.Writer { return io.Discard }

// benchHOpts 构造基准用 JSON handler 选项（Level=Info）。
func benchHOpts() *slog.HandlerOptions {
	return &slog.HandlerOptions{Level: slog.LevelInfo}
}

// benchRichAttrs 富负载属性集：1 个 Group（内含敏感词 key "password"）+ 4 个标量，共 5 项。
// 覆盖多 Kind 分发（Group/String/Int/Bool/Float64）与敏感词命中路径。
func benchRichAttrs() []any {
	return []any{
		slog.Group("db",
			slog.String("engine", "mysql"),
			slog.String("password", "s3cret"), // 敏感词 key
		),
		slog.Int("retries", 3),
		slog.String("path", "/var/log/app.log"),
		slog.Bool("cache_hit", true),
		slog.Float64("ratio", 0.75),
	}
}

// benchRichRecord 构造一条与 .With 富负载等价的复用 Record（供 raw Handle 路径，避免每次 .Info 的
// args→Attr 打包分配，纯测 handler 内部格式化与 bufPool 复用效果）。
func benchRichRecord() slog.Record {
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "bench message", 0)
	r.AddAttrs(
		slog.Group("db",
			slog.String("engine", "mysql"),
			slog.String("password", "s3cret"),
		),
		slog.Int("retries", 3),
		slog.String("path", "/var/log/app.log"),
		slog.Bool("cache_hit", true),
		slog.Float64("ratio", 0.75),
	)
	return r
}

// BenchmarkSingleLogPath 单条日志端到端路径（经 *slog.Logger.Info），四 handler × {simple, rich} 对比。
// 输出 io.Discard，allocs/op 即热路径分配（含 slog 打包 + handler 格式化）。
func BenchmarkSingleLogPath(b *testing.B) {
	benchmarks := []struct {
		name string
		h    func() slog.Handler
	}{
		{"console_nocolor", func() slog.Handler {
			return NewConsoleHandler(benchDiscard(), &ConsoleOptions{Level: slog.LevelInfo, NoColor: true})
		}},
		{"console_color", func() slog.Handler {
			return NewConsoleHandler(benchDiscard(), &ConsoleOptions{Level: slog.LevelInfo, NoColor: false})
		}},
		{"filetext", func() slog.Handler {
			return newFileTextHandler(benchDiscard(), &FileTextOptions{Level: slog.LevelInfo})
		}},
		{"json", func() slog.Handler {
			return slog.NewJSONHandler(benchDiscard(), benchHOpts())
		}},
	}
	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			base := slog.New(bm.h())
			rich := base.With(benchRichAttrs()...)

			b.Run("simple", func(b *testing.B) {
				for b.Loop() {
					base.Info("bench message")
				}
			})
			b.Run("rich", func(b *testing.B) {
				for b.Loop() {
					rich.Info("bench message")
				}
			})
		})
	}
}

// BenchmarkHandlerReuseRecord 复用单条 Record 直接 handler.Handle，剥离 slog.Logger 的 args 打包，
// 纯量化 handler 格式化路径的分配（验证 bufPool 生效：console/text 应为个位数 allocs/op）。
func BenchmarkHandlerReuseRecord(b *testing.B) {
	ctx := context.Background()
	handlers := []struct {
		name string
		h    func() slog.Handler
	}{
		{"console_nocolor", func() slog.Handler {
			return NewConsoleHandler(benchDiscard(), &ConsoleOptions{Level: slog.LevelInfo, NoColor: true})
		}},
		{"console_color", func() slog.Handler {
			return NewConsoleHandler(benchDiscard(), &ConsoleOptions{Level: slog.LevelInfo, NoColor: false})
		}},
		{"filetext", func() slog.Handler {
			return newFileTextHandler(benchDiscard(), &FileTextOptions{Level: slog.LevelInfo})
		}},
		{"json", func() slog.Handler {
			return slog.NewJSONHandler(benchDiscard(), benchHOpts())
		}},
	}
	for _, hh := range handlers {
		b.Run(hh.name, func(b *testing.B) {
			h := hh.h()
			rec := benchRichRecord()
			for b.Loop() {
				_ = h.Handle(ctx, rec)
			}
		})
	}
}

// BenchmarkDecorateOverhead 以 JSON handler 为底，量化每加一层装饰器的 ns/op / allocs 增量。
// 负载统一为富 attr，经 InfoContext 下发（+trace 分支的 ctx 含 trace_id/req_id 触发提取）。
func BenchmarkDecorateOverhead(b *testing.B) {
	jsonBase := func() slog.Handler { return slog.NewJSONHandler(benchDiscard(), benchHOpts()) }

	sensitiveCtx := context.Background()
	traceCtx := WithReqID(WithTraceID(context.Background(), "trace-abc-123"), "req-xyz-789")

	b.Run("baseline_json", func(b *testing.B) {
		lg := slog.New(jsonBase()).With(benchRichAttrs()...)
		for b.Loop() {
			lg.InfoContext(sensitiveCtx, "bench message")
		}
	})
	b.Run("+sensitive", func(b *testing.B) {
		lg := slog.New(NewSensitiveHandler(jsonBase(), &SensitiveOptions{Keys: []string{"password"}})).With(benchRichAttrs()...)
		for b.Loop() {
			lg.InfoContext(sensitiveCtx, "bench message")
		}
	})
	b.Run("+sampling", func(b *testing.B) {
		// First:1 Thereafter:1 → 全部保留，测采样计数与锁的透传成本（非丢弃快路径）
		lg := slog.New(NewSamplingHandler(jsonBase(), &SamplingOptions{First: 1, Thereafter: 1})).With(benchRichAttrs()...)
		for b.Loop() {
			lg.InfoContext(sensitiveCtx, "bench message")
		}
	})
	b.Run("+trace", func(b *testing.B) {
		lg := slog.New(NewTraceHandler(jsonBase())).With(benchRichAttrs()...)
		for b.Loop() {
			lg.InfoContext(traceCtx, "bench message") // ctx 含 trace_id/req_id，走提取+注入路径
		}
	})
	b.Run("+async", func(b *testing.B) {
		ah := NewAsyncHandler(jsonBase(), 1<<20, false) // 大队列，避免丢弃干扰
		lg := slog.New(ah).With(benchRichAttrs()...)
		b.ResetTimer()
		for b.Loop() {
			lg.InfoContext(sensitiveCtx, "bench message")
		}
		b.StopTimer()
		_ = ah.Close(30 * time.Second) // 排空后台队列，不计入计时
	})
}

// BenchmarkAsyncThroughput 异步吞吐（对 io.Discard）：
//   - pool_large：队列充足，b.RunParallel 高并发入队，测并发写入吞吐；
//   - saturated_drop：极小队列 + 高并发，触发「队列满丢弃」快路径，ReportMetric 记录 dropped 数。
//
// 并行度 = GOMAXPROCS（逻辑 processor 数）。
func BenchmarkAsyncThroughput(b *testing.B) {
	b.Run("pool_large", func(b *testing.B) {
		ah := NewAsyncHandler(slog.NewJSONHandler(benchDiscard(), benchHOpts()), 1<<20, false)
		lg := slog.New(ah)
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				lg.Info("async bench", "k", "v")
			}
		})
		b.StopTimer()
		total, dropped := ah.Stats()
		b.ReportMetric(float64(dropped), "dropped")
		b.Logf("total=%d dropped=%d", total, dropped)
		_ = ah.Close(60 * time.Second)
	})

	b.Run("saturated_drop", func(b *testing.B) {
		// 队列仅 8：绝大多数被丢弃，测丢弃快路径吞吐（不阻塞主业务）
		ah := NewAsyncHandler(slog.NewJSONHandler(benchDiscard(), benchHOpts()), 8, false)
		lg := slog.New(ah)
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				lg.Info("async bench", "k", "v")
			}
		})
		b.StopTimer()
		total, dropped := ah.Stats()
		b.ReportMetric(float64(dropped), "dropped")
		b.Logf("total=%d dropped=%d", total, dropped)
		_ = ah.Close(30 * time.Second)
	})
}

// BenchmarkNew 构造成本：带 / 不带 File sink。
// New 每次替换并关闭上一个默认实例（registerLogger/close 遍历）；bench 串行执行，互相关闭可接受。
// 不写日志 → lumberjack 惰性不实际 open fd。b.Cleanup 关最后残留实例防句柄泄漏。
func BenchmarkNew(b *testing.B) {
	b.Run("no_file", func(b *testing.B) {
		b.Cleanup(func() { _ = Close(2 * time.Second) })
		for b.Loop() {
			_ = New(WithConsole(WithConsoleWriter(io.Discard), WithConsoleColor(false)))
		}
	})
	b.Run("with_file", func(b *testing.B) {
		path := filepath.Join(b.TempDir(), "bench.log")
		b.Cleanup(func() { _ = Close(2 * time.Second) })
		for b.Loop() {
			_ = New(WithConsole(WithConsoleWriter(io.Discard), WithConsoleColor(false)), WithFile(path))
		}
	})
}

// BenchmarkInit 配置驱动的 Init 成本（console / file）。Init 内部走 New，替换默认实例时会关闭
// 上一个，串行 bench 可接受；临时目录 + b.Cleanup 关句柄防 fd 泄漏。
func BenchmarkInit(b *testing.B) {
	b.Run("console", func(b *testing.B) {
		b.Cleanup(func() { _ = Close(2 * time.Second) })
		for b.Loop() {
			_ = Init(Config{Level: "info", Output: "console"})
		}
	})
	b.Run("file", func(b *testing.B) {
		path := filepath.Join(b.TempDir(), "init.log")
		b.Cleanup(func() { _ = Close(2 * time.Second) })
		for b.Loop() {
			_ = Init(Config{Level: "info", Output: "file", File: path, FileFormat: "json"})
		}
	})
}

// BenchmarkParseFileFormat ParseFileFormat 各分支成本（yaml 字符串 → 枚举，含归一化与 error 路径）。
func BenchmarkParseFileFormat(b *testing.B) {
	cases := []struct{ name, in string }{
		{"empty", ""},
		{"json", "json"},
		{"text", "text"},
		{"mixed_upper", "  TEXT  "},
		{"unknown_err", "yaml"},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				_, _ = ParseFileFormat(c.in)
			}
		})
	}
}

// BenchmarkNewFileHandler newFileHandler 两后端选择（JSON / 自研 text）的构造开销。
func BenchmarkNewFileHandler(b *testing.B) {
	b.Run("json", func(b *testing.B) {
		for b.Loop() {
			_ = newFileHandler(benchDiscard(), FormatJSON, benchHOpts())
		}
	})
	b.Run("text", func(b *testing.B) {
		for b.Loop() {
			_ = newFileHandler(benchDiscard(), FormatText, benchHOpts())
		}
	})
}
