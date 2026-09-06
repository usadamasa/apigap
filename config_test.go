package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_ResolvesPaths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APIGAP_TEST_UNSET", "")
	t.Setenv("APIGAP_TEST_SET", "/from-env")
	src := `base_url: https://app.example.com
hosts: [cdn.example.org]
cookies: ${APIGAP_TEST_UNSET:-~/.cache}/myapp/cookies.json
scenarios: capture/scenarios
output: ${APIGAP_TEST_SET}/capture.har
coverage:
  code: [internal/client, cmd/myapp]
filter: capture/endpoint-filter.txt
normalize:
  rules:
    - pattern: '/docs/([A-Z]{2}-\d+)'
      replace: '/docs/{docId}'
`
	path := filepath.Join(dir, "apigap.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := map[string]string{
		"cookies":   filepath.Join(home, ".cache", "myapp", "cookies.json"),
		"scenarios": filepath.Join(dir, "capture", "scenarios"),
		"output":    "/from-env/capture.har",
		"filter":    filepath.Join(dir, "capture", "endpoint-filter.txt"),
		"code[1]":   filepath.Join(dir, "cmd", "myapp"),
	}
	got := map[string]string{
		"cookies": cfg.Cookies, "scenarios": cfg.Scenarios, "output": cfg.Output,
		"filter": cfg.Filter, "code[1]": cfg.Coverage.Code[1],
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	if cfg.Coverage.Spec != "" {
		t.Errorf("spec 未指定なら空のまま: %q", cfg.Coverage.Spec)
	}
	if cfg.SettleTimeoutMs != defaultSettleTimeoutMs {
		t.Errorf("settle_timeout_ms の既定 = %d", cfg.SettleTimeoutMs)
	}
	hosts := cfg.HostSet()
	if !hosts["app.example.com"] || !hosts["cdn.example.org"] || len(hosts) != 2 {
		t.Errorf("HostSet = %v", hosts)
	}
}

func TestLoadConfig_RejectsBadRule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apigap.yaml")
	src := "base_url: https://example.com\nnormalize:\n  rules:\n    - pattern: '('\n      replace: x\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("不正な正規表現はエラーのはず")
	}
}

func TestLoadConfig_RequiresBaseURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apigap.yaml")
	if err := os.WriteFile(path, []byte("output: x.har\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("base_url 無しはエラーのはず")
	}
}
