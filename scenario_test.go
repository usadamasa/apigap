package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadScenarios_OrdersByFilenameAndDefaultsName(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"20-detail.yaml": "steps:\n  - navigate: /items/42\n    wait_ms: 8000\n",
		"10-search.yaml": "name: search\nsteps:\n  - navigate: /search?q=widgets\n",
		"notes.txt":      "ignored",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LoadScenarios(dir)
	if err != nil {
		t.Fatalf("LoadScenarios: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d scenarios, want 2", len(got))
	}
	if got[0].Name != "search" || got[1].Name != "20-detail" {
		t.Errorf("names = %q, %q", got[0].Name, got[1].Name)
	}
	if got[0].Steps[0].EffectiveWaitMs() != defaultWaitMs {
		t.Errorf("wait_ms 省略時の既定が効いていない: %d", got[0].Steps[0].EffectiveWaitMs())
	}
	if got[1].Steps[0].EffectiveWaitMs() != 8000 {
		t.Errorf("wait_ms = %d", got[1].Steps[0].EffectiveWaitMs())
	}
}

func TestLoadScenarios_RejectsStepWithoutNavigate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("steps:\n  - wait_ms: 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadScenarios(dir); err == nil {
		t.Fatal("navigate 無しのステップはエラーのはず")
	}
}
