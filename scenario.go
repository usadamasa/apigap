package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Scenario はシナリオ YAML 1 ファイル。steps を上から順に実行する。
type Scenario struct {
	Name  string `yaml:"name"`
	Steps []Step `yaml:"steps"`
}

// Step はページを 1 枚開く。fetch 系のステップは持たない: Runtime ドメインを使うと
// Cloudflare Turnstile に検知されるため、観測はページ自身が投げる通信に限る。
type Step struct {
	// Navigate は開く URL。/ 始まりなら base_url に対する相対パス。
	Navigate string `yaml:"navigate"`
	// WaitMs はページが落ち着いてからさらに留まる時間。遅延ロードの XHR を拾うため。
	WaitMs int `yaml:"wait_ms,omitempty"`
}

// defaultWaitMs は wait_ms 省略時の滞在時間。DOMContentLoaded よりかなり後に飛ぶ
// リクエストがあるので、短すぎると観測したい通信を取り逃がす。
const defaultWaitMs = 3000

// EffectiveWaitMs は wait_ms の既定値込みの滞在時間。
func (s Step) EffectiveWaitMs() int {
	if s.WaitMs > 0 {
		return s.WaitMs
	}
	return defaultWaitMs
}

// LoadScenarios は dir 直下の *.yaml をファイル名順に読む。
func LoadScenarios(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("シナリオディレクトリの読み込みに失敗しました: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && (filepath.Ext(e.Name()) == ".yaml" || filepath.Ext(e.Name()) == ".yml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	scenarios := make([]Scenario, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path) // #nosec G304 -- 設定で指定されたディレクトリ配下
		if err != nil {
			return nil, err
		}
		var s Scenario
		if err := yaml.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("%s の解析に失敗しました: %w", path, err)
		}
		if s.Name == "" {
			s.Name = name[:len(name)-len(filepath.Ext(name))]
		}
		for i, st := range s.Steps {
			if st.Navigate == "" {
				return nil, fmt.Errorf("%s: steps[%d] に navigate がありません", path, i)
			}
		}
		scenarios = append(scenarios, s)
	}
	return scenarios, nil
}
