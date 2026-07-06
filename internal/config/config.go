// Package config loads zero-langfuse credentials and settings (DESIGN §9).
// Phase 1 is read-only: the interactive `setup` writer is Phase 2.
//
// Credential resolution (precedence high → low, DESIGN §9.1):
//  1. Env vars: LANGFUSE_PUBLIC_KEY, LANGFUSE_SECRET_KEY, LANGFUSE_BASE_URL
//     (alias LANGFUSE_HOST).
//  2. Config file: $XDG_CONFIG_HOME/zero-langfuse/config.json.
//  3. Defaults.
//
// Credentials are validated lazily: `--dry-run` works with no creds, real
// ingestion calls Validate.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nathanpt/zero-langfuse/internal/pricing"
	"github.com/nathanpt/zero-langfuse/internal/session"
)

const (
	defaultHost     = "https://cloud.langfuse.com"
	defaultPrivacy  = "full-debug"
	defaultFlushAt  = 100
	appConfigSubdir = "zero-langfuse"
	configFileName  = "config.json"
)

// Config is the resolved configuration. Defaults are applied during Load.
type Config struct {
	PublicKey    string
	SecretKey    string
	Host         string
	Privacy      string
	SessionsDir  string
	FlushAt      int
	Pricing      map[string]pricing.TokenPrice
	CaptureFlags map[string]bool
}

// fileConfig mirrors config.json on disk (DESIGN §9.1). Unset fields keep their
// zero value; missing JSON keys are ignored.
type fileConfig struct {
	PublicKey    string                        `json:"publicKey"`
	SecretKey    string                        `json:"secretKey"`
	Host         string                        `json:"host"`
	Privacy      string                        `json:"privacy"`
	SessionsDir  string                        `json:"sessionsDir"`
	FlushAt      int                           `json:"flushAt"`
	Pricing      map[string]pricing.TokenPrice `json:"pricing"`
	CaptureFlags map[string]bool               `json:"captureFlags"`
}

// ConfigPath returns the XDG-aware config file path (no creation).
func ConfigPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, appConfigSubdir, configFileName), nil
}

// Load resolves configuration. `env` is the process environment (or a test
// facsimile). Precedence: env vars override the file; both override defaults.
// A missing file is not an error.
func Load(env map[string]string) (*Config, error) {
	cfg := &Config{
		Host:    defaultHost,
		Privacy: defaultPrivacy,
		FlushAt: defaultFlushAt,
	}

	// 1. file (if present)
	if path, err := ConfigPath(); err == nil {
		if raw, err := os.ReadFile(path); err == nil {
			var fc fileConfig
			if err := json.Unmarshal(raw, &fc); err != nil {
				return nil, fmt.Errorf("config: parse %s: %w", path, err)
			}
			applyFile(cfg, &fc)
		}
	}

	// 2. env overrides file
	if v := env["LANGFUSE_PUBLIC_KEY"]; v != "" {
		cfg.PublicKey = v
	}
	if v := env["LANGFUSE_SECRET_KEY"]; v != "" {
		cfg.SecretKey = v
	}
	// LANGFUSE_BASE_URL wins; LANGFUSE_HOST is the alias.
	if v := env["LANGFUSE_BASE_URL"]; v != "" {
		cfg.Host = v
	} else if v := env["LANGFUSE_HOST"]; v != "" {
		cfg.Host = v
	}

	// 3. SessionsDir auto-resolution (DESIGN §3) when neither file nor env set it.
	if cfg.SessionsDir == "" {
		if dir, err := session.DefaultSessionsDir(); err == nil {
			cfg.SessionsDir = dir
		}
	}
	cfg.Host = normalizeHost(cfg.Host)
	return cfg, nil
}

func applyFile(cfg *Config, fc *fileConfig) {
	if fc.PublicKey != "" {
		cfg.PublicKey = fc.PublicKey
	}
	if fc.SecretKey != "" {
		cfg.SecretKey = fc.SecretKey
	}
	if fc.Host != "" {
		cfg.Host = fc.Host
	}
	if fc.Privacy != "" {
		cfg.Privacy = fc.Privacy
	}
	if fc.SessionsDir != "" {
		cfg.SessionsDir = fc.SessionsDir
	}
	if fc.FlushAt > 0 {
		cfg.FlushAt = fc.FlushAt
	}
	if fc.Pricing != nil {
		cfg.Pricing = fc.Pricing
	}
	if fc.CaptureFlags != nil {
		cfg.CaptureFlags = fc.CaptureFlags
	}
}

// Validate requires credentials for real ingestion. Callers gate `--dry-run`
// behind this, so dry-run works with an empty config.
func (c *Config) Validate() error {
	if c.PublicKey == "" || c.SecretKey == "" {
		return fmt.Errorf("config: credentials missing (set LANGFUSE_PUBLIC_KEY/LANGFUSE_SECRET_KEY or run setup); use --dry-run to skip upload")
	}
	return nil
}

// LoadFileOnly reads the config file alone (no env overrides, no defaults). It
// returns (nil, nil) when no file exists. Used by `setup` so the persisted file
// reflects what the user typed, not transient env values.
func LoadFileOnly() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var fc fileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg := &Config{}
	applyFile(cfg, &fc)
	return cfg, nil
}

// Save writes the config to ConfigPath() (mkdir 0700, file 0600). Written by
// `setup`; credentials never leave the local machine (DESIGN §9.1).
func Save(c *Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: mkdir: %w", err)
	}
	fc := fileConfig{
		PublicKey:    c.PublicKey,
		SecretKey:    c.SecretKey,
		Host:         c.Host,
		Privacy:      c.Privacy,
		SessionsDir:  c.SessionsDir,
		FlushAt:      c.FlushAt,
		Pricing:      c.Pricing,
		CaptureFlags: c.CaptureFlags,
	}
	raw, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// MergeSetup returns a Config for `setup` to save: it starts from `existing`
// (preserving pricing/sessionsDir/flushAt/captureFlags) and overwrites only the
// non-empty provided fields. Callers fill defaults (host/privacy) before
// calling so empty args mean "keep existing".
func MergeSetup(existing *Config, host, publicKey, secretKey, privacy string) *Config {
	c := &Config{}
	if existing != nil {
		*c = *existing
	}
	if host != "" {
		c.Host = normalizeHost(host)
	}
	if publicKey != "" {
		c.PublicKey = publicKey
	}
	if secretKey != "" {
		c.SecretKey = secretKey
	}
	if privacy != "" {
		c.Privacy = privacy
	}
	return c
}

// Mask redacts the middle of a key for safe display (e.g. "pk-lf-ab…xyz").
func Mask(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:6] + "…" + key[len(key)-4:]
}

// normalizeHost cleans a host URL: trims surrounding whitespace and any
// surrounding quote characters. A trailing/leading quote is a common paste or
// typing artifact that would otherwise corrupt every request URL parse.
func normalizeHost(h string) string {
	return strings.Trim(strings.TrimSpace(h), "\"'")
}
