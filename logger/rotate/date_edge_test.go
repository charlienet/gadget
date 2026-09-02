package rotate

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- B-1 回归：同日进程重启（新 writer 指向已存在当日文件）不得截断既有日志 ---

func TestReopenSameDayPreservesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// 第一个进程会话：写入当日早期日志
	w1 := &RotateDateWriter{Filename: path, Layout: "2006-01-02"}
	if _, err := w1.Write([]byte("PRECIOUS-EARLIER-TODAY\n")); err != nil {
		t.Fatalf("first session write: %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("first session close: %v", err)
	}

	// 第二个 writer 模拟同日重启：Stat 命中当日已存在文件 → chown → O_APPEND 打开
	w2 := &RotateDateWriter{Filename: path, Layout: "2006-01-02"}
	if _, err := w2.Write([]byte("after-restart\n")); err != nil {
		t.Fatalf("restart session write: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("restart session close: %v", err)
	}

	dated := filepath.Join(dir, "app."+time.Now().Format("2006-01-02")+".log")
	data, err := os.ReadFile(dated)
	if err != nil {
		t.Fatalf("read dated file: %v", err)
	}
	// 旧内容在前、新内容在后，零丢失（O_TRUNC 缺陷修复断言）
	if got, want := string(data), "PRECIOUS-EARLIER-TODAY\nafter-restart\n"; got != want {
		t.Errorf("expected appended content, got %q, want %q", got, want)
	}
}

// --- ③ dateFilePattern 模式生成（表驱动） ---

func TestDateFilePatternTable(t *testing.T) {
	tests := []struct {
		in       string
		wantBase string
		wantExt  string
	}{
		{"", "", ""}, // 空输入：先判空（m-7：Base("") 产 "." 曾使该分支不可达）
		{"app.log", "app.", ".log"},
		{"plain", "plain.", ""},
		{".hidefile", ".hidefile.", ""},  // 隐藏文件：base=文件名+.，无扩展名
		{"a.b.log", "a.b.", ".log"},      // 多段名：只切最后一段扩展
		{"/x/y/app.log", "app.", ".log"}, // 取 Base 后再拆
		{filepath.Join("logs", "av.test"), "av.", ".test"},
	}

	for _, tt := range tests {
		base, ext := dateFilePattern(tt.in)
		if base != tt.wantBase || ext != tt.wantExt {
			t.Errorf("dateFilePattern(%q) = (%q, %q), want (%q, %q)", tt.in, base, ext, tt.wantBase, tt.wantExt)
		}
	}
}

// --- ③ rotate：日期切换触发压缩+清理（m-1 后台）、同日重开幂等复用 ---

func TestRotateDateSwitchCompressAndCleanup(t *testing.T) {
	dir := t.TempDir()
	layout := "2006-01-02"
	// 不变式：真实跨日的新文件日期 = 今天，恒不早于 cleanup cutoff，不会被误删；
	// 测试同样遵循——旧日期 -4 天（超 MaxAge=3 → 压缩后被清理），
	// 新日期 -1 天（保留期内 → 后台 cleanup 不会触碰）。
	oldDate := time.Now().AddDate(0, 0, -4).Format(layout)
	newDate := time.Now().AddDate(0, 0, -1).Format(layout)

	w := &RotateDateWriter{
		Filename: filepath.Join(dir, "app.log"),
		Layout:   layout,
		Compress: true,
		MaxAge:   3,
	}
	defer w.Close()

	// 第一日：首次打开（oldDate==""，跳过压缩/清理分支）
	if err := w.rotate(oldDate); err != nil {
		t.Fatalf("first rotate: %v", err)
	}
	if w.time != oldDate {
		t.Fatalf("expected time=%s, got %q", oldDate, w.time)
	}
	if _, err := w.file.WriteString("old day data\n"); err != nil {
		t.Fatalf("write via handle: %v", err)
	}

	// 切日：oldDate != date → 压缩+清理派发到后台（m-1），rotate 本身不等待
	if err := w.rotate(newDate); err != nil {
		t.Fatalf("date switch rotate: %v", err)
	}
	w.pending.Wait() // 确定性等待后台任务收敛（禁止 sleep 轮询）
	if w.time != newDate {
		t.Fatalf("expected time=%s, got %q", newDate, w.time)
	}
	newFile := filepath.Join(dir, "app."+newDate+".log")
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("expected new date file opened: %v", err)
	}
	// 上一日原始文件与 .gz 均超出 MaxAge → 被清理
	if _, err := os.Stat(filepath.Join(dir, "app."+oldDate+".log")); !os.IsNotExist(err) {
		t.Errorf("expected stale raw file cleaned, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "app."+oldDate+".log.gz")); !os.IsNotExist(err) {
		t.Errorf("expected stale gz file cleaned, err=%v", err)
	}

	// 同日重复 rotate：跳过压缩/清理，复用已存在文件（stat→chown→open 幂等路径）
	if err := w.rotate(newDate); err != nil {
		t.Fatalf("idempotent rotate: %v", err)
	}
	if _, err := w.file.WriteString("second\n"); err != nil {
		t.Fatalf("write after reopen: %v", err)
	}
	data, err := os.ReadFile(newFile)
	if err != nil || !strings.Contains(string(data), "second") {
		t.Errorf("expected appended content in reused file, got %q err=%v", data, err)
	}
}

// --- ③ rotate：内部 close 失败即返回错误 ---

func TestRotateCloseError(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "stale.log"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Close(); err != nil { // 预关句柄：下一次 close 必然报错
		t.Fatalf("close: %v", err)
	}

	w := &RotateDateWriter{Filename: filepath.Join(dir, "app.log"), file: f}
	err = w.rotate("2025-05-05")
	if err == nil {
		t.Fatal("expected rotate to fail on closed handle")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("expected 'file already closed' error, got: %v", err)
	}
	// m-8：close 失败后句柄引用已被清空（不再自中毒）
	if w.file != nil {
		t.Error("expected r.file cleared to nil after failed close")
	}
}

// --- ③ close 失败自愈：首次 Write 报错后，下一次 Write 重新走 rotate 成功（m-8）---

func TestWriteRecoversAfterCloseFailure(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "stale.log"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	w := &RotateDateWriter{Filename: filepath.Join(dir, "heal.log"), Layout: "2006-01-02", file: f}

	// 第一次 Write：rotate 内 close 坏句柄失败 → 本次写入报错
	if _, err := w.Write([]byte("first")); err == nil {
		t.Fatal("expected first write to fail on closed handle")
	}
	// 第二次 Write：file 已置 nil → 重新 rotate 打开 → 成功（旧代码会永久 "file already closed"）
	if _, err := w.Write([]byte("recovered\n")); err != nil {
		t.Fatalf("expected recovery on second write, got: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close after recovery: %v", err)
	}

	dated := filepath.Join(dir, "heal."+time.Now().Format("2006-01-02")+".log")
	data, err := os.ReadFile(dated)
	if err != nil || string(data) != "recovered\n" {
		t.Errorf("expected recovered content only, got %q err=%v", data, err)
	}
}

// --- ③ Write/rotate：目标目录无法创建（路径段是普通文件）---

func TestWriteMkdirError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	// blocker 是文件，作为父目录 MkdirAll 必失败（ENOTDIR，root 亦不绕过）
	w := &RotateDateWriter{Filename: filepath.Join(blocker, "sub", "app.log")}
	n, err := w.Write([]byte("nope"))
	if err == nil {
		t.Fatalf("expected Write to fail, wrote n=%d", n)
	}
	if n != 0 {
		t.Errorf("expected n=0 on error, got %d", n)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected ENOTDIR cause, got: %v", err)
	}
}

// --- ③ chown：目标文件已存在时执行 chown；osChown 失败向上传播 ---

func TestChownErrorPropagation(t *testing.T) {
	orig := osChown
	osChown = func(string, int, int) error { return errors.New("chown boom") }
	defer func() { osChown = orig }()

	dir := t.TempDir()
	// 预置目标日期文件 → rotate 进入 stat 命中 → chown 分支
	if err := os.WriteFile(filepath.Join(dir, "app.2025-03-03.log"), []byte("old"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	w := &RotateDateWriter{Filename: filepath.Join(dir, "app.log"), Layout: "2006-01-02"}
	err := w.rotate("2025-03-03")
	if err == nil || !strings.Contains(err.Error(), "chown boom") {
		t.Fatalf("expected chown error propagation, got: %v", err)
	}
}

// --- ③ compressFile：目标 .gz 创建失败 → 错误经 rotate 传播 ---

func TestCompressFileCreateError(t *testing.T) {
	dir := t.TempDir()
	date := "2025-04-04"
	raw := filepath.Join(dir, dateFilename("app.log", date))
	if err := os.WriteFile(raw, []byte("data"), 0644); err != nil {
		t.Fatalf("seed raw: %v", err)
	}
	// 占位目录挡住 app.2025-04-04.log.gz 的创建
	if err := os.Mkdir(raw+".gz", 0755); err != nil {
		t.Fatalf("mkdir gz blocker: %v", err)
	}

	w := &RotateDateWriter{Filename: filepath.Join(dir, "app.log"), Layout: "2006-01-02", Compress: true}
	if err := w.compressFile(date); err == nil {
		t.Error("expected compressFile to fail when .gz path is a directory")
	}

	// m-1：跨日压缩已移入后台——压缩失败不再使 rotate 失败，
	// 仅由后台任务经 slog.Warn 上报；新日期文件正常打开，写入路径不受阻
	w.time = date
	if err := w.rotate("2025-04-05"); err != nil {
		t.Fatalf("rotate must not fail on backgrounded compress error, got: %v", err)
	}
	newDated := filepath.Join(dir, dateFilename("app.log", "2025-04-05"))
	if _, err := os.Stat(newDated); err != nil {
		t.Errorf("expected new date file opened despite compress failure: %v", err)
	}
	w.pending.Wait() // 后台任务收敛：错误被 Warn 消化，此处不 panic 即通过

	// 压缩失败保留原始文件（未误删）
	if _, err := os.Stat(raw); err != nil {
		t.Errorf("raw file must survive failed compress: %v", err)
	}
	defer w.Close()
}

// --- ③ cleanupOld：目录不存在时返回 ReadDir 错误 ---

func TestCleanupOldReadDirError(t *testing.T) {
	w := &RotateDateWriter{
		Filename: filepath.Join(t.TempDir(), "missing-dir", "app.log"),
		Layout:   "2006-01-02",
		MaxAge:   3,
	}
	if err := w.cleanupOld(); err == nil {
		t.Fatal("expected cleanupOld to fail on unreadable dir")
	}
}

// --- ③ cleanupOld：前缀不匹配 / 空日期中段 的跳过分支（陈旧文件不受误删影响） ---

func TestCleanupOldSkipBranches(t *testing.T) {
	dir := t.TempDir()
	w := &RotateDateWriter{Filename: filepath.Join(dir, "app.log"), Layout: "2006-01-02", MaxAge: 3}

	// 干扰文件：前缀不匹配（readme.txt）+ base+ext 直连产生空日期中段（"app." + "" + ".log"）
	strays := []string{"readme.txt", "app..log"}
	for _, name := range strays {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	if err := w.cleanupOld(); err != nil {
		t.Fatalf("cleanupOld: %v", err)
	}
	for _, name := range strays {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected skipped file %q kept, err=%v", name, err)
		}
	}
}

// --- Close：无打开文件时幂等成功 ---

func TestCloseWithoutFile(t *testing.T) {
	w := &RotateDateWriter{Filename: filepath.Join(t.TempDir(), "never.log")}
	if err := w.Close(); err != nil {
		t.Errorf("Close on fresh writer: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("double Close: %v", err)
	}
}

// --- m-1：跨日压缩在后台完成后旧文件转 .gz，且当前写入不被压缩阻塞 ---

func TestDateSwitchCompressInBackgroundNonBlocking(t *testing.T) {
	dir := t.TempDir()
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	w := &RotateDateWriter{Filename: filepath.Join(dir, "bg.log"), Layout: "2006-01-02", Compress: true}

	// 构造"昨日已有日志"的状态
	if err := w.rotate(yesterday); err != nil {
		t.Fatalf("rotate yesterday: %v", err)
	}
	if _, err := w.file.WriteString("yesterday-data\n"); err != nil {
		t.Fatalf("seed yesterday data: %v", err)
	}

	// 跨日 rotate：派发后台压缩后立即返回（不同步 gzip——大文件时曾阻塞午夜首条日志）
	if err := w.rotate(today); err != nil {
		t.Fatalf("date switch rotate: %v", err)
	}
	// 写入路径不受压缩进度影响：同日快路径直接落当前文件
	if _, err := w.Write([]byte("today-not-blocked\n")); err != nil {
		t.Fatalf("write during background compress: %v", err)
	}
	todayFile := filepath.Join(dir, "bg."+today+".log")
	if data, err := os.ReadFile(todayFile); err != nil || string(data) != "today-not-blocked\n" {
		t.Errorf("expected current write independent of compress, got %q err=%v", data, err)
	}

	// 确定性等待后台收敛（Close 内置 Wait 策略 + 显式接缝，禁止 sleep 轮询）
	w.pending.Wait()

	// 旧文件已转 .gz 且内容完整，原始文件删除
	raw := filepath.Join(dir, "bg."+yesterday+".log")
	if _, err := os.Stat(raw); !os.IsNotExist(err) {
		t.Errorf("expected raw yesterday file removed after background compress, err=%v", err)
	}
	gzf, err := os.Open(raw + ".gz")
	if err != nil {
		t.Fatalf("expected .gz produced: %v", err)
	}
	defer gzf.Close()
	zr, err := gzip.NewReader(gzf)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if string(decompressed) != "yesterday-data\n" {
		t.Errorf("gz content lost/altered: %q", decompressed)
	}

	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// --- NEW-1：rotate 失败（目录损坏）零派发——不产生任务、不产生 Warn，返回错误 ---

func TestRotateFailureDispatchesNothing(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	// 父路径段是普通文件：MkdirAll 失败。NEW-1 修复后派发点位于成功打开之后，
	// 该失败路径零派发（旧版派发先于打开，此场景正是 goroutine 风暴入口）
	w := &RotateDateWriter{Filename: filepath.Join(blocker, "sub", "app.log"), Layout: "2006-01-02", MaxAge: 1}
	w.time = "2025-01-01"
	if err := w.rotate("2025-01-02"); err == nil {
		t.Fatal("expected rotate to fail when parent path segment is a file")
	}
	w.pending.Wait() // 立即返回：计数器为零（若误派发，此处将等待/挂起）
	if w.file != nil {
		t.Error("failed rotate must leave file nil")
	}
}
