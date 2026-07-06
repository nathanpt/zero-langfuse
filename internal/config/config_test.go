package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nathanpt/zero-langfuse/internal/pricing"
)

func writeFile(t *testing.T, dir, content string) {
	t.Helper()
	path := filepath.Join(dir, appConfigSubdir, configFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != defaultHost {
		t.Errorf("Host = %q, want %q", cfg.Host, defaultHost)
	}
	if cfg.Privacy != defaultPrivacy {
		t.Errorf("Privacy = %q, want %q", cfg.Privacy, defaultPrivacy)
	}
	if cfg.FlushAt != defaultFlushAt {
		t.Errorf("FlushAt = %d, want %d", cfg.FlushAt, defaultFlushAt)
	}
	if cfg.SessionsDir == "" {
		t.Error("SessionsDir should auto-resolve")
	}
}

func TestLoadFileValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, `{
		"publicKey": "pk-file",
		"secretKey": "sk-file",
		"host": "https://lf.example.com",
		"privacy": "conversations",
		"flushAt": 5,
		"pricing": {"glm-5.2": {"input": 0.5, "output": 2}}
	}`)
	cfg, err := Load(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicKey != "pk-file" || cfg.SecretKey != "sk-file" {
		t.Errorf("creds = %q/%q, want pk-file/sk-file", cfg.PublicKey, cfg.SecretKey)
	}
	if cfg.Host != "https://lf.example.com" {
		t.Errorf("Host = %q", cfg.Host)
	}
	if cfg.Privacy != "conversations" {
		t.Errorf("Privacy = %q", cfg.Privacy)
	}
	if cfg.FlushAt != 5 {
		t.Errorf("FlushAt = %d, want 5", cfg.FlushAt)
	}
	if p, ok := cfg.Pricing["glm-5.2"]; !ok || p.Input != 0.5 {
		t.Errorf("pricing not loaded: %+v", cfg.Pricing)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, `{"publicKey":"pk-file","secretKey":"sk-file","host":"https://file.example.com"}`)
	cfg, err := Load(map[string]string{
		"LANGFUSE_PUBLIC_KEY": "pk-env",
		"LANGFUSE_SECRET_KEY": "sk-env",
		"LANGFUSE_BASE_URL":   "https://env.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicKey != "pk-env" || cfg.SecretKey != "sk-env" {
		t.Errorf("env should override file: %q/%q", cfg.PublicKey, cfg.SecretKey)
	}
	if cfg.Host != "https://env.example.com" {
		t.Errorf("LANGFUSE_BASE_URL should override file host: %q", cfg.Host)
	}
}

func TestLangfuseHostAlias(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(map[string]string{"LANGFUSE_HOST": "https://alias.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://alias.example.com" {
		t.Errorf("LANGFUSE_HOST alias: Host = %q", cfg.Host)
	}
	// BASE_URL takes precedence over HOST when both set.
	cfg2, _ := Load(map[string]string{
		"LANGFUSE_HOST":     "https://alias.example.com",
		"LANGFUSE_BASE_URL": "https://base.example.com",
	})
	if cfg2.Host != "https://base.example.com" {
		t.Errorf("BASE_URL should win over HOST: %q", cfg2.Host)
	}
}

func TestValidateMissingCreds(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, _ := Load(map[string]string{})
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should error on missing creds")
	}
}

func TestValidateWithCreds(t *testing.T) {
	cfg := &Config{PublicKey: "pk", SecretKey: "sk"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate should pass with creds: %v", err)
	}
}

func TestConfigPathHonorsXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, appConfigSubdir, configFileName)
	if p != want {
		t.Errorf("ConfigPath = %q, want %q", p, want)
	}
}

func TestLoadHealsQuotedHost(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// A trailing/leading quote is a common paste artifact; Load must strip it.
	writeFile(t, dir, `{"host":"http://192.168.0.21:3000\"","publicKey":"pk","secretKey":"sk"}`)
	cfg, err := Load(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "http://192.168.0.21:3000" {
		t.Errorf("host not healed = %q, want http://192.168.0.21:3000", cfg.Host)
	}
}

func TestLoadHealsWrappedHost(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, `{"host":"\"https://lf.example.com\"","publicKey":"pk","secretKey":"sk"}`)
	cfg, _ := Load(map[string]string{})
	if cfg.Host != "https://lf.example.com" {
		t.Errorf("wrapped host not healed = %q", cfg.Host)
	}
}

func TestSaveRoundTripPreservesPricing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	c := &Config{
		PublicKey: "pk-lf-abc", SecretKey: "sk-lf-xyz",
		Host: "https://lf.example.com", Privacy: "conversations", FlushAt: 7,
		Pricing: map[string]pricing.TokenPrice{"glm-5.2": {Input: 0.5, Output: 2}},
	}
	if err := Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// File perms 0600.
	p, _ := ConfigPath()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %o, want 0600", perm)
	}
	// LoadFileOnly round-trips, including pricing.
	got, err := LoadFileOnly()
	if err != nil {
		t.Fatalf("LoadFileOnly: %v", err)
	}
	if got.PublicKey != "pk-lf-abc" || got.SecretKey != "sk-lf-xyz" {
		t.Errorf("creds not round-tripped: %+v", got)
	}
	if got.FlushAt != 7 {
		t.Errorf("FlushAt = %d, want 7", got.FlushAt)
	}
	if p, ok := got.Pricing["glm-5.2"]; !ok || p.Input != 0.5 {
		t.Errorf("pricing not round-tripped: %+v", got.Pricing)
	}
}

func TestLoadFileOnlyMissingIsNil(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := LoadFileOnly()
	if err != nil || got != nil {
		t.Errorf("LoadFileOnly on absent file = (%+v, %v), want (nil, nil)", got, err)
	}
}

func TestMergeSetupPreservesAndOverwrites(t *testing.T) {
	existing := &Config{
		PublicKey: "pk-old", SecretKey: "sk-old", Host: "https://old.example.com",
		Privacy: "full-debug", FlushAt: 5,
		Pricing: map[string]pricing.TokenPrice{"glm-5.2": {Input: 1.4}},
	}
	// New host + keys; pricing must survive.
	got := MergeSetup(existing, "https://new.example.com", "pk-new", "sk-new", "")
	if got.Host != "https://new.example.com" || got.PublicKey != "pk-new" || got.SecretKey != "sk-new" {
		t.Errorf("overwrite failed: %+v", got)
	}
	if got.Privacy != "full-debug" {
		t.Errorf("privacy not preserved: %q", got.Privacy)
	}
	if p, ok := got.Pricing["glm-5.2"]; !ok || p.Input != 1.4 {
		t.Errorf("pricing not preserved by setup: %+v", got.Pricing)
	}
	// nil existing → fresh config with provided fields.
	got2 := MergeSetup(nil, "https://h", "pk", "sk", "conversations")
	if got2.PublicKey != "pk" || got2.Privacy != "conversations" || got2.Pricing != nil {
		t.Errorf("MergeSetup(nil, …) = %+v", got2)
	}
}

func TestMask(t *testing.T) {
	if m := Mask("pk-lf-abcdefghijkl"); m != "pk-lf-…ijkl" {
		t.Errorf("Mask = %q", m)
	}
	if m := Mask("short"); !strings.HasPrefix(m, "***") {
		t.Errorf("Mask short = %q, want all-stars", m)
	}
}
