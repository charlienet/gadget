package logger_test

import (
	"bytes"
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
