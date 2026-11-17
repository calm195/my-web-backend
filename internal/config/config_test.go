package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func configEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SITE_DATABASE_PATH", filepath.Join(dir, "site.db"))
	t.Setenv("SITE_ASSET_DIR", filepath.Join(dir, "assets"))
	t.Setenv("SITE_PUBLIC_ORIGIN", "https://localhost:8443")
	t.Setenv("SITE_ADDR", "127.0.0.1:8080")
	t.Setenv("SITE_CSRF_KEY", strings.Repeat("a1", 32))
	t.Setenv("SITE_RATE_LIMIT_KEY", strings.Repeat("b2", 32))
}
func TestServerConfigurationRejectsUnsafeValues(t *testing.T) {
	configEnv(t)
	if _, err := Load(true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, value string }{{"SITE_ADDR", "0.0.0.0:8080"}, {"SITE_DATABASE_PATH", "relative.db"}, {"SITE_PUBLIC_ORIGIN", "http://localhost:8443"}, {"SITE_PUBLIC_ORIGIN", "https://localhost:8443/admin"}, {"SITE_CSRF_KEY", "placeholder"}, {"SITE_RATE_LIMIT_KEY", ""}} {
		t.Run(c.name+"="+c.value, func(t *testing.T) {
			t.Setenv(c.name, c.value)
			if _, err := Load(true); err == nil {
				t.Fatal("危险配置未被拒绝")
			}
		})
	}
}
func TestMaintenanceDoesNotRequireSessionSecrets(t *testing.T) {
	configEnv(t)
	t.Setenv("SITE_CSRF_KEY", "")
	t.Setenv("SITE_RATE_LIMIT_KEY", "")
	if _, err := Load(false); err != nil {
		t.Fatal(err)
	}
}
