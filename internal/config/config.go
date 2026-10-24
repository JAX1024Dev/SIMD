package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, e := time.ParseDuration(n.Value)
	*d = Duration(v)
	return e
}
func (d Duration) Value() time.Duration { return time.Duration(d) }

type Device struct {
	ID       string   `yaml:"id"`
	Name     string   `yaml:"name"`
	SIMLabel string   `yaml:"sim_label"`
	Phone    string   `yaml:"phone"`
	Driver   string   `yaml:"driver"`
	Port     string   `yaml:"port"`
	Baud     int      `yaml:"baud"`
	RTSCTS   bool     `yaml:"rts_cts"`
	Enabled  *bool    `yaml:"enabled"`
	Targets  []string `yaml:"bark_targets"`
}

func (d Device) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

type Target struct {
	ID       string `yaml:"id"`
	URL      string `yaml:"url"`
	KeyEnv   string `yaml:"key_env"`
	Key      string `yaml:"-"`
	PathMode string `yaml:"path_mode"`
}
type Config struct {
	Listen         string   `yaml:"listen"`
	TokenEnv       string   `yaml:"token_env"`
	Token          string   `yaml:"-"`
	Database       string   `yaml:"database"`
	ScanInterval   Duration `yaml:"scan_interval"`
	StatusInterval Duration `yaml:"status_interval"`
	AssemblyWait   Duration `yaml:"assembly_wait"`
	SendTTL        Duration `yaml:"send_ttl"`
	ForwardTTL     Duration `yaml:"forward_ttl"`
	BarkTimeout    Duration `yaml:"bark_timeout"`
	RetentionDays  int      `yaml:"retention_days"`
	Devices        []Device `yaml:"devices"`
	BarkTargets    []Target `yaml:"bark_targets"`
}

func Load(path string) (Config, error) {
	c := Config{Listen: "127.0.0.1:8080", TokenEnv: "SIMD_API_TOKEN", Database: "data/simd.db", ScanInterval: Duration(time.Minute), StatusInterval: Duration(30 * time.Second), AssemblyWait: Duration(5 * time.Minute), SendTTL: Duration(5 * time.Minute), ForwardTTL: Duration(24 * time.Hour), BarkTimeout: Duration(10 * time.Second), RetentionDays: 90}
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	if e = d.Decode(&c); e != nil {
		return c, fmt.Errorf("configuration: %w", e)
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return c, errors.New("configuration must contain one YAML document")
	}
	if _, _, e = net.SplitHostPort(c.Listen); e != nil {
		return c, errors.New("listen must be host:port")
	}
	c.Token = os.Getenv(c.TokenEnv)
	if len(c.Token) < 24 {
		return c, errors.New("API token environment variable must contain at least 24 bytes")
	}
	for name, v := range map[string]Duration{"scan_interval": c.ScanInterval, "status_interval": c.StatusInterval, "assembly_wait": c.AssemblyWait, "send_ttl": c.SendTTL, "forward_ttl": c.ForwardTTL, "bark_timeout": c.BarkTimeout} {
		if v <= 0 || v.Value() > 365*24*time.Hour {
			return c, fmt.Errorf("%s must be positive and at most 365 days", name)
		}
	}
	if c.RetentionDays < 1 || c.RetentionDays > 36500 {
		return c, errors.New("retention_days must be between 1 and 36500")
	}
	if c.AssemblyWait.Value() > 24*time.Hour {
		return c, errors.New("assembly_wait must not exceed the 24-hour assembly matching window")
	}
	if strings.TrimSpace(c.Database) == "" || c.Database == ":memory:" {
		return c, errors.New("persistent database path required")
	}
	if !filepath.IsAbs(c.Database) {
		c.Database, e = filepath.Abs(c.Database)
		if e != nil {
			return c, e
		}
	}
	targets := map[string]bool{}
	for i := range c.BarkTargets {
		t := &c.BarkTargets[i]
		if !validID(t.ID) || targets[t.ID] {
			return c, errors.New("Bark target ids must be unique and use letters, digits, dash or underscore")
		}
		targets[t.ID] = true
		u, err := url.Parse(t.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return c, fmt.Errorf("target %s requires an HTTPS base URL without credentials or query", t.ID)
		}
		t.URL = strings.TrimRight(t.URL, "/")
		if t.PathMode == "" {
			t.PathMode = "push"
		}
		if t.PathMode != "push" && t.PathMode != "key" {
			return c, fmt.Errorf("target %s path_mode must be push or key", t.ID)
		}
		t.Key = os.Getenv(t.KeyEnv)
		if t.Key == "" || len(t.Key) > 512 {
			return c, fmt.Errorf("target %s key environment variable is empty", t.ID)
		}
	}
	ids, ports := map[string]bool{}, map[string]bool{}
	enabled := 0
	for i := range c.Devices {
		d := &c.Devices[i]
		if !validID(d.ID) || ids[d.ID] {
			return c, errors.New("device ids must be unique and use letters, digits, dash or underscore")
		}
		ids[d.ID] = true
		if d.Name == "" {
			d.Name = d.ID
		}
		if d.SIMLabel == "" {
			d.SIMLabel = d.ID
		}
		for _, label := range []string{d.Name, d.SIMLabel} {
			if len(label) > 128 {
				return c, errors.New("device name and SIM label must be at most 128 UTF-8 bytes")
			}
			for _, r := range label {
				if r < 32 || r == 127 {
					return c, errors.New("device labels cannot contain control characters")
				}
			}
		}
		if d.Driver == "" {
			d.Driver = "ml307r"
		}
		if d.Driver != "ml307r" {
			return c, fmt.Errorf("unsupported driver for %s", d.ID)
		}
		if d.Baud == 0 {
			d.Baud = 115200
		}
		if !validBaud(d.Baud) {
			return c, fmt.Errorf("unsupported baud for %s", d.ID)
		}
		if !d.IsEnabled() {
			continue
		}
		enabled++
		if !filepath.IsAbs(d.Port) {
			return c, fmt.Errorf("device %s requires absolute serial path", d.ID)
		}
		port := filepath.Clean(d.Port)
		if resolved, err := filepath.EvalSymlinks(port); err == nil {
			port = resolved
		}
		if ports[port] {
			return c, errors.New("enabled devices cannot share a serial port")
		}
		ports[port] = true
		seen := map[string]bool{}
		for _, id := range d.Targets {
			if !targets[id] || seen[id] {
				return c, fmt.Errorf("invalid or duplicate Bark target for %s", d.ID)
			}
			seen[id] = true
		}
	}
	if enabled == 0 {
		return c, errors.New("at least one enabled device required")
	}
	return c, nil
}
func validID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func validBaud(n int) bool {
	switch n {
	case 300, 1200, 2400, 4800, 9600, 19200, 38400, 57600, 115200, 230400:
		return true
	}
	return false
}
