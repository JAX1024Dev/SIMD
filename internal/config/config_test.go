package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAndStrictValidation(t *testing.T) {
	t.Setenv("SIMD_API_TOKEN", "a-very-long-private-token-value")
	t.Setenv("BARK_KEY", "key")
	base := "devices:\n  - id: sim1\n    port: /dev/ttyS0\n    bark_targets: [phone]\nbark_targets:\n  - id: phone\n    url: https://api.day.app\n    key_env: BARK_KEY\n"
	for _, tc := range []struct {
		name, text string
		ok         bool
	}{{"valid", base, true}, {"unknown", base + "typo: true\n", false}, {"http", strings.Replace(base, "https://", "http://", 1), false}, {"inline_secret", base + "token: bad\n", false}, {"zero", base + "scan_interval: 0s\n", false}, {"duplicate_port", strings.Replace(base, "bark_targets:\n", "  - id: sim2\n    port: /dev/ttyS0\nbark_targets:\n", 1), false}, {"multiple_docs", base + "---\nlisten: 127.0.0.1:9\n", false}} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			os.WriteFile(p, []byte(tc.text), 0600)
			c, e := Load(p)
			if (e == nil) != tc.ok {
				t.Fatalf("%v", e)
			}
			if e == nil && (c.ScanInterval.Value() != time.Minute || c.RetentionDays != 90 || c.Devices[0].Baud != 115200) {
				t.Fatalf("defaults %#v", c)
			}
		})
	}
}

func TestBarkPathModes(t *testing.T) {
	t.Setenv("SIMD_API_TOKEN", "a-very-long-private-token-value")
	t.Setenv("BARK_KEY", "key")
	for _, mode := range []string{"push", "key", "fallback", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			text := "devices:\n  - id: sim1\n    port: /dev/ttyS0\nbark_targets:\n  - id: phone\n    url: https://bark.example\n    key_env: BARK_KEY\n    path_mode: " + mode + "\n"
			os.WriteFile(p, []byte(text), 0600)
			c, e := Load(p)
			valid := mode == "push" || mode == "key"
			if (e == nil) != valid {
				t.Fatal(e)
			}
			if e == nil && c.BarkTargets[0].PathMode != mode {
				t.Fatal("path mode not preserved")
			}
		})
	}
}
