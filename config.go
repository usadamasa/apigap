package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config は apigap.yaml の形。相対パスは設定ファイルのあるディレクトリ基準で解決する。
type Config struct {
	// BaseURL はシナリオの相対パスを解決する origin。gap はこのホストの通信だけを見る。
	BaseURL string `yaml:"base_url"`
	// Hosts は base_url のホストに加えて gap の対象にするホスト。
	Hosts []string `yaml:"hosts,omitempty"`
	// Cookies はブラウザに注入する Cookie ファイル ({"cookies":[{name,value,domain,path,...}]})。
	Cookies string `yaml:"cookies"`
	// Scenarios はシナリオ YAML を置くディレクトリ。
	Scenarios string `yaml:"scenarios"`
	// Output は capture が書く HAR のパス。
	Output string `yaml:"output"`
	// Headless は --headless=new で起動する。Cloudflare Turnstile 配下では通らない。
	Headless bool `yaml:"headless"`
	// Foreground は Chrome の窓を前面に出す。既定では macOS で `open -g` を使い、
	// 作業中のアプリからフォーカスを奪わない。
	Foreground bool `yaml:"foreground"`
	// SettleTimeoutMs は navigate 後、チャレンジでない Document 応答を待つ上限。
	SettleTimeoutMs int `yaml:"settle_timeout_ms"`
	// APIPrefixes を指定すると、gap はこの prefix に前方一致するパスだけを API とみなす。
	// 空なら同一ホストの document / xhr / fetch / other をすべて対象にする。
	APIPrefixes []string `yaml:"api_prefixes,omitempty"`
	Coverage    Coverage `yaml:"coverage"`
	// Filter は gap が「除外済み」と報告する prefix の一覧ファイル (1 行 1 prefix、# 以降は理由)。
	Filter    string    `yaml:"filter,omitempty"`
	Normalize Normalize `yaml:"normalize"`
}

// Coverage は「リポジトリが既に知っているエンドポイント」の出どころ。
type Coverage struct {
	// Spec は OpenAPI spec。paths のテンプレートが covered の根拠になる。省略可。
	Spec string `yaml:"spec,omitempty"`
	// Code は URL リテラルを走査する Go ソースのディレクトリ。
	Code []string `yaml:"code,omitempty"`
}

// Normalize は観測パスを 1 行に畳むための規則。
type Normalize struct {
	// TrailingSlash は末尾スラッシュ有無を同一視する (Django 系のサイト向け)。
	TrailingSlash bool `yaml:"trailing_slash"`
	// Rules は UUID・数値セグメントの既定に加えて適用する置換。regexp の構文。
	Rules []Rule `yaml:"rules,omitempty"`
}

// Rule は正規表現による置換 1 件。Replace では ${1} の形で部分一致を参照できる。
type Rule struct {
	Pattern string `yaml:"pattern"`
	Replace string `yaml:"replace"`
}

const defaultSettleTimeoutMs = 30000

// LoadConfig は path を読み、パスを解決して返す。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- 利用者が指定した設定ファイル
	if err != nil {
		return nil, fmt.Errorf("設定ファイルの読み込みに失敗しました: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("設定ファイルの解析に失敗しました (%s): %w", path, err)
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("%s: base_url は必須です", path)
	}
	if _, err := url.Parse(cfg.BaseURL); err != nil {
		return nil, fmt.Errorf("%s: base_url が不正です: %w", path, err)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	cfg.Cookies = resolvePath(base, cfg.Cookies)
	cfg.Scenarios = resolvePath(base, cfg.Scenarios)
	cfg.Output = resolvePath(base, cfg.Output)
	cfg.Coverage.Spec = resolvePath(base, cfg.Coverage.Spec)
	cfg.Filter = resolvePath(base, cfg.Filter)
	for i, c := range cfg.Coverage.Code {
		cfg.Coverage.Code[i] = resolvePath(base, c)
	}
	if cfg.SettleTimeoutMs <= 0 {
		cfg.SettleTimeoutMs = defaultSettleTimeoutMs
	}
	for _, r := range cfg.Normalize.Rules {
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return nil, fmt.Errorf("%s: normalize.rules の pattern が不正です (%q): %w", path, r.Pattern, err)
		}
	}
	return &cfg, nil
}

// HostSet は gap が対象にするホストの集合。
func (c *Config) HostSet() map[string]bool {
	u, _ := url.Parse(c.BaseURL)
	hosts := map[string]bool{}
	if u != nil && u.Host != "" {
		hosts[u.Host] = true
	}
	for _, h := range c.Hosts {
		hosts[h] = true
	}
	return hosts
}

// resolvePath は ~ と ${VAR:-default} を展開し、相対パスを base 基準の絶対パスにする。
func resolvePath(base, p string) string {
	if p == "" {
		return ""
	}
	p = expandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return p
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expandEnv は $VAR / ${VAR} / ${VAR:-default} を展開する。os.ExpandEnv に無い
// fallback 構文が要るのは、XDG 変数が未設定の環境が普通にあるため。
func expandEnv(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := envRe.FindStringSubmatch(m)
		name, def := sub[1], sub[2]
		if name == "" {
			name = sub[3]
		}
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		return def
	})
}
