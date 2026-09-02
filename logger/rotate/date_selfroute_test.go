package rotate

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- NEW-1 回归：后台任务自反馈链与 Close 挂起 ---

// countingWriter 统计 Write 调用次数（捕获经默认 logger 路由的 Warn 条数）。
type countingWriter struct{ n atomic.Int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n.Add(1); return len(p), nil }

// setSelfRoutedDefault 把默认 slog 链设为「计数文本 + JSON→w」多路，
// 模拟 New(WithFile(...WithDateRotate)) 内部 SetDefault 后、后台 Warn 写回
// 本 writer 的自路由形态。返回恢复函数。
func setSelfRoutedDefault(t *testing.T, w *RotateDateWriter, cw *countingWriter) {
	t.Helper()
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewMultiHandler(
		slog.NewTextHandler(cw, nil),
		slog.NewJSONHandler(w, nil), // Warn 经此写回 w 自身（自路由）
	)))
	t.Cleanup(func() { slog.SetDefault(orig) })
}

// TestFailedRotateDispatchesNoTask 实证修复核心场景：跨日 rotate 因目录运行期
// 损坏而失败时，不得派发后台任务——无任务即无 Warn，「Warn 自路由写回故障
// writer → 再 rotate → 再派发」的自反馈风暴（旧实现 ~15 万次/秒 + Close 挂起）
// 无从触发；行为退化为与旧同步实现一致的 Write 返错。
func TestFailedRotateDispatchesNoTask(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}

	w := &RotateDateWriter{
		Filename: filepath.Join(logs, "app.log"),
		Layout:   "2006-01-02",
		Compress: true,
		MaxAge:   1,
	}
	w.time = "2020-01-01" // 陈旧日期：任何当前 date 都构成「跨日」

	// 运行期目录故障：logs 目录被替换为普通文件（MkdirAll 必失败，root 亦然）
	if err := os.Rename(logs, logs+".old"); err != nil {
		t.Fatalf("rename logs away: %v", err)
	}
	if err := os.WriteFile(logs, []byte("blocker"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	var cw countingWriter
	setSelfRoutedDefault(t, w, &cw)

	gBase := runtime.NumGoroutine()

	// 触发跨日写入：rotate 在打开文件处失败
	if _, err := w.Write([]byte("must fail")); err == nil {
		t.Fatal("expected Write to fail on broken dir")
	}

	// 确定性收敛点：修复后无任何任务 → Wait 立即返回；
	// （若回归复发，风暴任务在此挂起 → 测试超时暴露）
	w.pending.Wait()

	if got := cw.n.Load(); got != 0 {
		t.Errorf("expected zero Warn (no task dispatched on rotate failure), got %d", got)
	}
	// r.time 仅在成功打开后置位：失败路径不得推进日期状态
	if w.time != "2020-01-01" {
		t.Errorf("expected r.time untouched on failed rotate, got %q", w.time)
	}
	// 无 goroutine 风暴：与基线比较留系统 goroutine 抖动余量
	if got := runtime.NumGoroutine(); got > gBase+2 {
		t.Errorf("expected no goroutine storm, base=%d now=%d", gBase, got)
	}

	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestSelfRoutedWarnTerminatesAtFastPath 证明成功跨日下的链长恒为 1：
// 任务失败产生恰好一条 Warn，该 Warn 经自路由写回本 writer 时命中
// 「file 非 nil 且日期一致」快路径——不再派发新任务，风暴无从建立。
func TestSelfRoutedWarnTerminatesAtFastPath(t *testing.T) {
	dir := t.TempDir()
	layout := "2006-01-02"
	yesterday := time.Now().AddDate(0, 0, -1).Format(layout)
	today := time.Now().Format(layout)

	w := &RotateDateWriter{
		Filename: filepath.Join(dir, "app.log"),
		Layout:   layout,
		Compress: true,
		MaxAge:   0, // 只保留压缩路径：恰好一条 Warn，计数确定
	}
	if err := w.rotate(yesterday); err != nil {
		t.Fatalf("rotate first day: %v", err)
	}
	if _, err := w.file.WriteString("yd\n"); err != nil {
		t.Fatalf("seed yesterday: %v", err)
	}

	// 让后台压缩注定失败：.gz 目标路径用目录占位 → 任务产生恰好一条 Warn
	ydRaw := filepath.Join(dir, "app."+yesterday+".log")
	if err := os.Mkdir(ydRaw+".gz", 0o755); err != nil {
		t.Fatalf("mkdir gz blocker: %v", err)
	}

	var cw countingWriter
	setSelfRoutedDefault(t, w, &cw)

	gBase := runtime.NumGoroutine()

	// 跨日：成功打开 → 派发恰好一个后台任务
	if err := w.rotate(today); err != nil {
		t.Fatalf("date switch rotate: %v", err)
	}
	w.pending.Wait() // 确定性收敛：任务完成（含其 Warn 自路由写入）后才返回

	// 链长恒为 1：Warn 有界（恰好 1 条），自路由未派生新任务
	if got := cw.n.Load(); got != 1 {
		t.Errorf("expected exactly 1 self-routed Warn (chain length 1), got %d", got)
	}
	if got := runtime.NumGoroutine(); got > gBase+2 {
		t.Errorf("expected no lingering task goroutines, base=%d now=%d", gBase, got)
	}

	// 自路由 Warn 落在当前健康文件（快路径成功），旧文件未被误动
	todayFile := filepath.Join(dir, "app."+today+".log")
	data, err := os.ReadFile(todayFile)
	if err != nil || !strings.Contains(string(data), "rotate: compress previous date file failed") {
		t.Errorf("expected self-routed warn landed in current file, got %q err=%v", data, err)
	}
	if _, err := os.Stat(ydRaw); err != nil {
		t.Errorf("failed compress must keep raw file: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestBackgroundCleanupFailureWarnsOnce 覆盖后台任务的 cleanup 失败 Warn 分支。
// 派发后用 cleanupMu 挂起任务，确定性销毁目录再放行——任务 ReadDir 必失败，
// 产生恰好一条 Warn（经自路由落回当前健康 writer 的快路径，不派生新任务）。
func TestBackgroundCleanupFailureWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	layout := "2006-01-02"
	yesterday := time.Now().AddDate(0, 0, -1).Format(layout)
	today := time.Now().Format(layout)

	w := &RotateDateWriter{
		Filename: filepath.Join(dir, "app.log"),
		Layout:   layout,
		MaxAge:   1, // 触发 cleanup；Compress 默认 false，Warn 来源唯一
	}
	if err := w.rotate(yesterday); err != nil {
		t.Fatalf("rotate first day: %v", err)
	}

	// 先占住 cleanupMu：跨日派发的任务将在锁上挂起，制造确定性窗口
	w.cleanupMu.Lock()

	var cw countingWriter
	setSelfRoutedDefault(t, w, &cw)

	if err := w.rotate(today); err != nil {
		t.Fatalf("healthy date switch must succeed: %v", err)
	}
	// 任务已派发且被挂起：此刻毁掉目录，任务恢复后 cleanup 必失败
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	w.cleanupMu.Unlock() // 放行任务

	w.pending.Wait()
	if got := cw.n.Load(); got != 1 {
		t.Errorf("expected exactly 1 cleanup Warn from background task, got %d", got)
	}

	// Warn 经自路由写入当前文件句柄（目录已删但 inode 存活，快路径写入成功）；
	// Close 正常收敛
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
