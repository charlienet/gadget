package logger

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// console_logvaluer_internal_test.go —— 渲染链对 slog.LogValuer 的处理契约（P1 修复回归）。
//
// Go 1.27 实测：实现 LogValuer 的值 Kind() 返回 KindLogValuer（新导出 Kind），
// 渲染链判 Kind 前必须 Resolve，否则落入 appendValue 的 default 兜底调 v.Group()
// 触发 panic("Group: bad kind")，直接崩业务进程。
// 对齐标准库 handleState.appendAttr（log/slog/handler.go:476：a.Value = a.Value.Resolve()）。
//
// 注：Go 1.27 无 slog.Attr.Resolve API，逐字段解析由 appendAttr 入口对
// a.Value.Resolve() + Group 元素递归进 appendAttr 自然达成（与标准库同构）。

// logValUser 返回 Group 形态（name/age），验证 Group 嵌套展开语义。
type logValUser struct {
	Name string
	Age  int
}

func (u logValUser) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", u.Name), slog.Int("age", u.Age))
}

// logValStr 返回 KindString，验证解析后走 appendTextValue 引号规则。
type logValStr struct{}

func (logValStr) LogValue() slog.Value { return slog.StringValue("a b") }

// plainStruct 非 LogValuer 普通 struct，验证 %+v 兜底不回归。
type plainStruct struct{ X int }

// outerValuer/innerValuer 验证 Group 元素内再套 LogValuer 的递归解析。
type outerValuer struct{}

func (outerValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("mid", innerValuer{}))
}

type innerValuer struct{}

func (innerValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.Bool("ok", true))
}

// chainSelfValuer 的 LogValue 持续返回自身类型（链式自引用），
// 命中 Value.Resolve 的 maxLogValues=100 保护，应退化为 error 字符串而非栈爆。
//
// 另实测：LogValue 返回含自身惰性值的 Group（selfGroupValuer 形态）时，
// Go 1.27.1 标准库 TextHandler 自身即 fatal error: stack overflow。两者均为
// 进程级 fatal（recover 不可达），但失败方式不同：标准库为有界 stack overflow
// （约 1GB 栈上限），本实现在加深度上限前因 prefix 逐层拼接为 O(depth²) 内存
// 增长、可先触发内核 OOM kill 波及同机进程；现已由渲染深度上限消除
// （见 TestDeepGroupDepthLimit 与 appendAttr 深度守卫），病理形态退化为
// 有界单行 !DEPTH 输出（子进程实测见 TestSelfGroupSubprocessProbe）。
type chainSelfValuer struct{}

func (chainSelfValuer) LogValue() slog.Value { return slog.AnyValue(chainSelfValuer{}) }

// handleNoColor 以 NoColor console handler 渲染单条 record，返回整行。
// defer recover 把渲染 panic 转为测试失败信息（红阶段保留 panic 现场语义）。
func handleNoColor(t *testing.T, attrs ...slog.Attr) string {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("render panic (LogValuer 未解析?): %v", r)
		}
	}()
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &ConsoleOptions{Level: slog.LevelInfo, NoColor: true})
	ts := time.Date(2026, 9, 24, 10, 12, 33, 456_000_000, time.UTC)
	r := slog.NewRecord(ts, slog.LevelInfo, "m", 0)
	r.AddAttrs(attrs...)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}
	return buf.String()
}

// 行为基准①：LogValuer 返回 Group → 点分展开，与 slog.Group attr 语义一致。
func TestLogValuerGroupExpand(t *testing.T) {
	got := handleNoColor(t, slog.Any("user", logValUser{Name: "bob", Age: 30}))
	want := "2026-09-24 10:12:33.456 INFO m user.name=bob user.age=30\n"
	if got != want {
		t.Errorf("golden mismatch:\n got:  %q\nwant: %q", got, want)
	}
}

// 行为基准②：LogValuer 返回 KindString → 解析后按引号规则输出。
func TestLogValuerResolvesToString(t *testing.T) {
	got := handleNoColor(t, slog.Any("k", logValStr{}))
	want := `2026-09-24 10:12:33.456 INFO m k="a b"` + "\n"
	if got != want {
		t.Errorf("golden mismatch:\n got:  %q\nwant: %q", got, want)
	}
}

// 行为基准③：非 LogValuer 普通 struct 保持 %+v 兜底，逐字节不回归。
func TestPlainStructFallbackUnchanged(t *testing.T) {
	got := handleNoColor(t, slog.Any("s", plainStruct{X: 7}))
	want := "2026-09-24 10:12:33.456 INFO m s={X:7}\n"
	if got != want {
		t.Errorf("golden mismatch:\n got:  %q\nwant: %q", got, want)
	}
}

// 行为基准④：嵌套 LogValuer（Group 元素内再套 LogValuer）递归解析展开，不 panic。
func TestNestedLogValuerExpand(t *testing.T) {
	got := handleNoColor(t, slog.Any("user", outerValuer{}))
	want := "2026-09-24 10:12:33.456 INFO m user.mid.ok=true\n"
	if got != want {
		t.Errorf("golden mismatch:\n got:  %q\nwant: %q", got, want)
	}
}

// 行为基准⑤：链式自引用 LogValuer 命中标准库 maxLogValues 深度保护，
// 输出退化 error 字符串（Go 1.27.1 实测形态：AnyValue(fmt.Errorf("LogValue called
// too many times on Value of type %T"))，经 appendAny 的 %+v 兜底渲染），不栈爆。
func TestLogValuerChainDepthGuardNoPanic(t *testing.T) {
	got := handleNoColor(t, slog.Any("user", chainSelfValuer{}))
	if !strings.Contains(got, "user=LogValue called too many times on Value of type logger.chainSelfValuer") {
		t.Errorf("expected maxLogValues degradation output, got: %q", got)
	}
}

// 行为基准⑥（既定语义锁定）：前置字段挑选判据不改——LogValuer（Kind 为
// KindLogValuer，非显式 KindString）即使解析后是 string 形态也不参与 service/env
// 等前置，按普通 attr 渲染。
func TestFrontFieldIgnoresLogValuer(t *testing.T) {
	got := handleNoColor(t, slog.Any(AttrService, logValStr{}))
	want := `2026-09-24 10:12:33.456 INFO m service="a b"` + "\n"
	if got != want {
		t.Errorf("expected LogValuer NOT front-picked (bare value before msg):\n got:  %q\nwant: %q", got, want)
	}
}

// 回归锁：正常三层 Group 嵌套（远低于深度上限）点分展开逐字节不变——
// depth 参数仅拦截病理/超深输入，不改变常规路径渲染。
func TestThreeLevelGroupExpandUnchanged(t *testing.T) {
	got := handleNoColor(t, slog.Group("k", slog.Group("l2", slog.Group("l3", slog.Bool("ok", true)))))
	want := "2026-09-24 10:12:33.456 INFO m k.l2.l3.ok=true\n"
	if got != want {
		t.Errorf("golden mismatch:\n got:  %q\nwant: %q", got, want)
	}
}

// selfGroupValuer 的 LogValue 返回含自身惰性值的 Group（病理自引用形态）：
// 每次 Resolve 只解析一层、标准库 maxLogValues 不跨调用累计，无法拦截渲染层
// 递归，必须由 appendAttr 的渲染深度上限接住（见 TestSelfGroupSubprocessProbe）。
type selfGroupValuer struct{}

func (s selfGroupValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("self", s))
}

// TestDeepGroupDepthLimit：程序化构造 150 层嵌套 slog.Group（最内层 bool attr），
// 断言渲染不 panic、输出单行、含 !DEPTH 退化标记、行长度有界（< 100KB）。
// 深度上限前该用例红在无 !DEPTH（全量展开巨行）。
func TestDeepGroupDepthLimit(t *testing.T) {
	const levels = 150
	attr := slog.Bool("ok", true)
	for i := levels; i > 0; i-- {
		attr = slog.Attr{Key: fmt.Sprintf("l%d", i), Value: slog.GroupValue(attr)}
	}
	got := handleNoColor(t, attr)
	if !strings.Contains(got, "!DEPTH") {
		t.Errorf("expected !DEPTH degradation at depth cap, got %d bytes, tail: %q",
			len(got), tailForTest(got, 80))
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("expected single line, got %d newlines", strings.Count(got, "\n"))
	}
	if len(got) >= 100*1024 {
		t.Errorf("expected bounded line < 100KB, got %d bytes", len(got))
	}
}

// tailForTest 取 s 末尾 n 字节（测试失败信息用，避免整巨行刷屏）。
func tailForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// selfGroupChildEnv 标记子进程模式：探针用例 exec 自身测试二进制跑该子测试，
// 病理形态修复前在子进程内栈爆/OOM kill（进程级 fatal、recover 不可达），
// 主测试进程只观察子退出码与输出。
const selfGroupChildEnv = "GADGET_LOGGER_SELFGROUP_CHILD"

// TestSelfGroupSubprocessProbe：selfGroup 病理形态子进程探针。
// 子进程模式：渲染 selfGroupValuer 并把整行打到 stdout，正常退出即未被击杀；
// 父进程模式：exec 自身断言退出码 0 且输出含 !DEPTH。
// 若实测形态在 Resolve 阶段即被标准库深度保护先接住，则 !DEPTH 不出现而
// 输出为 error 退化串——本用例据实断言实际拦截层（见 !DEPTH 失败信息）。
func TestSelfGroupSubprocessProbe(t *testing.T) {
	if os.Getenv(selfGroupChildEnv) == "1" {
		got := handleNoColor(t, slog.Any("root", selfGroupValuer{}))
		fmt.Print(got)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestSelfGroupSubprocessProbe$")
	cmd.Env = append(os.Environ(), selfGroupChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child err=%v output:\n%s", err, out)
	if err != nil {
		t.Fatalf("child process killed (stack overflow / OOM, 深度守卫缺失?): %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("!DEPTH")) {
		t.Fatalf("child exited ok but no !DEPTH in output (实际拦截层为 Resolve?):\n%s", out)
	}
}
