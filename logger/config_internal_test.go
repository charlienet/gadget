package logger

import "testing"

// TestConfigOptionsNoColorMapping 在映射层锁定 Config.NoColor 两态 → ConsoleSettings.Color。
//
// 端到端（captureStdout）把 os.Stdout 重定向到管道即非 TTY，NoColor=true（强制关）与
// NoColor=false（自动判定 → 非 TTY 关）输出都无 ANSI，二者不可分辨。故真正的「强制关 vs
// 自动关」契约只能在映射层断言：configOptions 产出的 Option 经 buildOptions 应用后，
// Console.Color 是否为显式 false 指针（强制关）还是 nil（自动）。
func TestConfigOptionsNoColorMapping(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantSet bool // 期望 Console.Color != nil（即显式强制关）
		wantVal bool // wantSet 为 true 时期望的 *Console.Color 值
	}{
		{"console+NoColor=true→强制关", Config{Output: "console", NoColor: true}, true, false},
		{"console+NoColor=false→自动", Config{Output: "console", NoColor: false}, false, false},
		{"both+NoColor=true→强制关", Config{Output: "both", File: "app.log", NoColor: true}, true, false},
		{"both+NoColor=false→自动", Config{Output: "both", File: "app.log", NoColor: false}, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := buildOptions(configOptions(tt.cfg)...)
			if o.Console == nil {
				t.Fatalf("want Console sink declared, got nil (output=%q)", tt.cfg.Output)
			}
			if tt.wantSet {
				if o.Console.Color == nil {
					t.Fatalf("NoColor=true: want Console.Color explicitly set (强制关), got nil（自动）")
				}
				if *o.Console.Color != tt.wantVal {
					t.Errorf("want *Console.Color=%v, got %v", tt.wantVal, *o.Console.Color)
				}
			} else if o.Console.Color != nil {
				t.Errorf("NoColor=false: want Console.Color==nil（自动判定）, got %v", *o.Console.Color)
			}
		})
	}

	// DefaultConfig（NoColor 零值 false）映射后亦应为自动态：Console 声明、Color==nil。
	o := buildOptions(configOptions(DefaultConfig())...)
	if o.Console == nil || o.Console.Color != nil {
		t.Errorf("DefaultConfig: want Console.Color==nil（自动）, got %+v", o.Console)
	}
}
