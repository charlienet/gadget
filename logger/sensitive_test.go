package logger_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/charlienet/gadget/logger"
)

// 指定 Keys 命中后打码，非敏感字段保留
func TestSensitiveFilter(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSensitiveKeys("password"))
	l.Info("login", "password", "secret123", "user", "bob")

	got := buf.String()
	if !strings.Contains(got, "******") {
		t.Errorf("expected masked value in output, got: %s", got)
	}
	if strings.Contains(got, "secret123") {
		t.Errorf("expected secret value masked, got: %s", got)
	}
	if !strings.Contains(got, "bob") {
		t.Errorf("expected non-sensitive value kept, got: %s", got)
	}
}

// 启用敏感过滤但未配置 Keys 时，使用内置默认敏感词集兜底
func TestSensitiveDefaultKeys(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSensitiveKeys())
	l.Info("m", "token", "abc")

	if got := buf.String(); strings.Contains(got, "abc") {
		t.Errorf("expected default sensitive key masked, got: %s", got)
	}
}

// 自定义掩码生效
func TestSensitiveMask(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSensitiveMask("[REDACTED]"))
	l.Info("m", "password", "pw")

	got := buf.String()
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected custom mask in output, got: %s", got)
	}
	if strings.Contains(got, "pw") {
		t.Errorf("expected value masked, got: %s", got)
	}
}

// key 匹配大小写不敏感
func TestSensitiveKeyCaseInsensitive(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSensitiveKeys("password"))
	l.Info("m", "Password", "secret")

	got := buf.String()
	if !strings.Contains(got, "******") {
		t.Errorf("expected masked value in output, got: %s", got)
	}
	if strings.Contains(got, "secret") {
		t.Errorf("expected value masked case-insensitively, got: %s", got)
	}
}

// 自定义匹配函数优先于默认子串匹配：只匹配 phone，内置词集（如 token）不再生效
func TestSensitiveMatch(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false),
		logger.WithSensitiveMatch(func(k string) bool { return k == "phone" }))
	l.Info("m", "phone", "123", "token", "abc")

	got := buf.String()
	if strings.Contains(got, "123") {
		t.Errorf("expected phone value masked, got: %s", got)
	}
	if !strings.Contains(got, "abc") {
		t.Errorf("expected token value kept (custom match overrides defaults), got: %s", got)
	}
	if !strings.Contains(got, "******") {
		t.Errorf("expected mask in output, got: %s", got)
	}
}

// Group 内属性递归打码
func TestSensitiveGroup(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSensitiveKeys("password"))
	l.Info("m", slog.Group("db", slog.String("password", "pw")))

	got := buf.String()
	if strings.Contains(got, "pw") {
		t.Errorf("expected group inner value masked, got: %s", got)
	}
	if !strings.Contains(got, "******") {
		t.Errorf("expected mask in output, got: %s", got)
	}
}

// A5：内置默认词集精确匹配——secretary/tokenizer 等含敏感词前缀的合法字段不误伤
func TestSensitiveBuiltinExactMatch(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSensitiveKeys())
	l.Info("m",
		"secretary", "val1", // 含 "secret" 前缀，不误伤
		"tokenizer", "val2", // 含 "token" 前缀，不误伤
		"token", "abc", // 精确命中，打码
	)

	got := buf.String()
	if !strings.Contains(got, "secretary=val1") {
		t.Errorf("expected secretary value kept (exact match), got: %s", got)
	}
	if !strings.Contains(got, "tokenizer=val2") {
		t.Errorf("expected tokenizer value kept (exact match), got: %s", got)
	}
	if strings.Contains(got, "=abc") {
		t.Errorf("expected token value masked, got: %s", got)
	}
	if !strings.Contains(got, "******") {
		t.Errorf("expected mask in output, got: %s", got)
	}
}

// A5：新增内置词 salt/cookie/jwt/x-api-key/client_id/session 精确命中打码（大小写不敏感）
func TestSensitiveBuiltinExtendedKeys(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSensitiveKeys())
	l.Info("m",
		"salt", "s1",
		"cookie", "c1",
		"jwt", "j1",
		"X-API-Key", "k1", // 精确匹配大小写不敏感
		"client_id", "cid1",
		"session", "sid1",
	)

	got := buf.String()
	for _, secret := range []string{"=s1", "=c1", "=j1", "=k1", "=cid1", "=sid1"} {
		if strings.Contains(got, secret) {
			t.Errorf("expected %s masked, got: %s", secret, got)
		}
	}
	if !strings.Contains(got, "******") {
		t.Errorf("expected mask in output, got: %s", got)
	}
}

// A5：用户 WithSensitiveKeys 追加的词保持子串匹配（显式指定即有意）
func TestSensitiveUserKeysSubstring(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSensitiveKeys("phone"))
	l.Info("m", "phone_number", "123", "phone", "456")

	got := buf.String()
	if strings.Contains(got, "=123") || strings.Contains(got, "=456") {
		t.Errorf("expected user substring keys masked, got: %s", got)
	}
	if !strings.Contains(got, "******") {
		t.Errorf("expected mask in output, got: %s", got)
	}
}

// A6：SensitiveString 对消息文本按内置词集打码（供消息显式打码使用）
func TestSensitiveString(t *testing.T) {
	got := logger.SensitiveString("password: abc token: xyz")
	for _, kw := range []string{"password", "token"} {
		if strings.Contains(got, kw) {
			t.Errorf("expected %q masked in text, got: %s", kw, got)
		}
	}
	if !strings.Contains(got, "******") {
		t.Errorf("expected mask in output, got: %s", got)
	}

	// 大小写不敏感
	if got := logger.SensitiveString("PASSWORD is 123"); !strings.Contains(got, "******") {
		t.Errorf("expected case-insensitive mask, got: %s", got)
	}
}

// N-2：链路追踪保留属性豁免——用户子串词 "id" 不得打码 TraceHandler 注入的
// trace_id / req_id；同 key 子串规则的其它属性（sessionid）照常打码
func TestSensitiveReservedTraceKeysExempt(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSensitiveKeys("id"))

	ctx := logger.WithTraceID(context.Background(), "trace-keep")
	ctx = logger.WithReqID(ctx, "req-keep")
	l.InfoContext(ctx, "login", "sessionid", "sess-hide")

	got := buf.String()
	if !strings.Contains(got, "trace_id=trace-keep") {
		t.Errorf("expected trace_id kept intact, got: %s", got)
	}
	if !strings.Contains(got, "req_id=req-keep") {
		t.Errorf("expected req_id kept intact, got: %s", got)
	}
	if strings.Contains(got, "sess-hide") || !strings.Contains(got, "sessionid=******") {
		t.Errorf("expected sessionid still masked by substring key, got: %s", got)
	}
}

// N-2：Group 递归路径同样豁免（attrHasSensitive / maskAttr 共用被包装的 match）；
// 手动写入的保留名属性与系统注入走同一豁免
func TestSensitiveReservedKeysInsideGroup(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false), logger.WithSensitiveKeys("id"))

	l.Info("m", slog.Group("meta",
		slog.String("trace_id", "g-trace-keep"),
		slog.String("req_id", "g-req-keep"),
		slog.String("userid", "g-user-hide"),
	))

	got := buf.String()
	if !strings.Contains(got, "meta.trace_id=g-trace-keep") || !strings.Contains(got, "meta.req_id=g-req-keep") {
		t.Errorf("expected reserved keys kept inside group, got: %s", got)
	}
	if strings.Contains(got, "g-user-hide") || !strings.Contains(got, "meta.userid=******") {
		t.Errorf("expected userid masked inside group, got: %s", got)
	}
}

// N-2：自定义 Match 函数路径同样保留豁免（match 入口统一包装）
func TestSensitiveReservedKeysExemptWithCustomMatch(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New(logger.WithOutput(&buf), logger.WithColor(false),
		logger.WithSensitiveMatch(func(k string) bool { return strings.Contains(k, "id") }))

	ctx := logger.WithTraceID(context.Background(), "custom-keep")
	l.InfoContext(ctx, "m", "deviceid", "dev-hide")

	got := buf.String()
	if !strings.Contains(got, "trace_id=custom-keep") {
		t.Errorf("expected trace_id exempt under custom match, got: %s", got)
	}
	if strings.Contains(got, "dev-hide") {
		t.Errorf("expected deviceid masked, got: %s", got)
	}
}
