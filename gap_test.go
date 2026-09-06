package main

import (
	"encoding/json"
	"strconv"
	"testing"
)

// testConfig は架空サイト example.com を対象にした最小の設定。api_prefixes を
// 空にしてあるので API 判定は _resourceType に委ねられる。
func testConfig() *Config {
	return &Config{BaseURL: "https://example.com"}
}

// refRule はサイト固有の採番を 1 行に畳む置換。この採番はハイフンとドットを含み、
// 既定の正規化 (UUID / 全桁数字) では畳めないので、設定側のルールが要る。
func refRule() Rule {
	return Rule{
		Pattern: `^/docs/((?:pdf|abs|full)/)?XY-\d+/[^/?]+`,
		Replace: `/docs/${1}{ref}`,
	}
}

// harFixture は entry の JSON 断片から最小の HAR を組み立てる。
func harFixture(entries ...string) []byte {
	doc := `{"log":{"entries":[`
	for i, e := range entries {
		if i > 0 {
			doc += ","
		}
		doc += e
	}
	doc += `]}}`
	return []byte(doc)
}

func entry(method, url string, status int, resourceType string) string {
	return `{"_resourceType":"` + resourceType + `",` +
		`"request":{"method":"` + method + `","url":"` + url + `"},` +
		`"response":{"status":` + strconv.Itoa(status) + `}}`
}

// entryNoType は _resourceType フィールドを持たないエントリ。DevTools 以外が
// 書き出した HAR にはこのフィールドが無い。
func entryNoType(method, url string, status int) string {
	return `{"request":{"method":"` + method + `","url":"` + url + `"},` +
		`"response":{"status":` + strconv.Itoa(status) + `}}`
}

func graphQLEntry(url, postData string, status int) string {
	quoted, err := json.Marshal(postData)
	if err != nil {
		panic(err)
	}
	return `{"_resourceType":"fetch",` +
		`"request":{"method":"POST","url":"` + url + `",` +
		`"postData":{"text":` + string(quoted) + `}},` +
		`"response":{"status":` + strconv.Itoa(status) + `}}`
}

func TestExtractEndpoints_DropsNonAPITraffic(t *testing.T) {
	har := harFixture(
		entry("GET", "https://example.com/search?q=x", 200, "document"),
		// 静的アセットは API ではない。
		entry("GET", "https://example.com/static/app.js", 200, "script"),
		entry("GET", "https://example.com/static/style.css", 200, "stylesheet"),
		entry("GET", "https://example.com/static/cover.png", 200, "image"),
		// 別ホストへの計測ビーコン。
		entry("POST", "https://example.org/collect", 204, "xhr"),
		// blob: URL はホストを持たないので落ちる。
		entry("GET", "blob:https://example.com/74a888a0", 200, "other"),
	)

	got, err := extractEndpoints(testConfig(), har)
	if err != nil {
		t.Fatalf("extractEndpoints: %v", err)
	}

	want := []Endpoint{
		{Method: "GET", Path: "/search", Status: 200},
	}
	assertEndpoints(t, got, want)
}

func TestExtractEndpoints_KeepsEntriesWithoutResourceType(t *testing.T) {
	// _resourceType が無い HAR で全滅させると、DevTools 以外の記録が使えなくなる。
	har := harFixture(
		entryNoType("GET", "https://example.com/search?q=x", 200),
		entryNoType("GET", "https://example.org/collect", 204),
	)

	got, err := extractEndpoints(testConfig(), har)
	if err != nil {
		t.Fatalf("extractEndpoints: %v", err)
	}

	want := []Endpoint{
		{Method: "GET", Path: "/search", Status: 200},
	}
	assertEndpoints(t, got, want)
}

func TestExtractEndpoints_APIPrefixesOverrideResourceType(t *testing.T) {
	// api_prefixes を指定したときは、それが唯一の判定基準になる。
	// prefix 配下なら script でも拾い、prefix 外なら xhr でも落とす。
	cfg := testConfig()
	cfg.APIPrefixes = []string{"/api/"}

	har := harFixture(
		entry("GET", "https://example.com/api/items/123", 200, "script"),
		entry("GET", "https://example.com/search?q=x", 200, "xhr"),
	)

	got, err := extractEndpoints(cfg, har)
	if err != nil {
		t.Fatalf("extractEndpoints: %v", err)
	}

	want := []Endpoint{
		{Method: "GET", Path: "/api/items/{id}", Status: 200},
	}
	assertEndpoints(t, got, want)
}

func TestExtractEndpoints_CollapsesRefVariants(t *testing.T) {
	cfg := testConfig()
	cfg.Normalize.Rules = []Rule{refRule()}

	har := harFixture(
		entry("GET", "https://example.com/docs/XY-1234/5678.90", 200, "document"),
		entry("GET", "https://example.com/docs/XY-2468/1357.02", 200, "document"),
		entry("GET", "https://example.com/docs/pdf/XY-1234/5678.90?download=true", 200, "document"),
	)

	got, err := extractEndpoints(cfg, har)
	if err != nil {
		t.Fatalf("extractEndpoints: %v", err)
	}

	want := []Endpoint{
		{Method: "GET", Path: "/docs/pdf/{ref}", Status: 200},
		{Method: "GET", Path: "/docs/{ref}", Status: 200},
	}
	assertEndpoints(t, got, want)
}

func TestExtractEndpoints_DedupesPreferringSuccess(t *testing.T) {
	// 同じエンドポイントは正常系と異常系の両方で叩かれる。失敗の方を採ると
	// 生きているエンドポイントを壊れていると読み違える。
	har := harFixture(
		entry("GET", "https://example.com/search?q=&size=0", 400, "document"),
		entry("GET", "https://example.com/search?q=alpha", 200, "document"),
		entry("GET", "https://example.com/search?q=beta", 200, "document"),
	)

	got, err := extractEndpoints(testConfig(), har)
	if err != nil {
		t.Fatalf("extractEndpoints: %v", err)
	}

	want := []Endpoint{
		{Method: "GET", Path: "/search", Status: 200},
	}
	assertEndpoints(t, got, want)
}

func TestExtractEndpoints_SplitsGraphQLByOperation(t *testing.T) {
	// 1 つの URL に複数の operation が多重化される。まとめて 1 行にすると
	// 画面が提供する掘り下げがすべて消える。
	har := harFixture(
		graphQLEntry(
			"https://example.com/api/graphql",
			`{"query":"\n query SuggestedQuery($limit: Int!) { suggestedResults { id } }\n","variables":{"limit":3},"queryName":"suggested1"}`,
			200),
		graphQLEntry(
			"https://example.com/api/graphql",
			`{"query":"\n query mostRecent($mostRecentLimit: Int!) { mostRecentResults { id } }\n","variables":{"mostRecentLimit":12},"queryName":"mostRecent"}`,
			200),
	)

	got, err := extractEndpoints(testConfig(), har)
	if err != nil {
		t.Fatalf("extractEndpoints: %v", err)
	}

	want := []Endpoint{
		{Method: "POST", Path: "/api/graphql", Operation: "SuggestedQuery", Status: 200},
		{Method: "POST", Path: "/api/graphql", Operation: "mostRecent", Status: 200},
	}
	assertEndpoints(t, got, want)
}

func TestGraphQLOperation_AnonymousQueryUsesFirstField(t *testing.T) {
	// 無名クエリを "" として報告すると、同じ URL の他の無名 operation と混ざる。
	body := `{"query":"\n  query {\n    pastEvents: userEvents(limit: 5) {\n      results { id }\n    }\n  }\n"}`
	got := graphQLOperation("/api/graphql", body)
	if got != "pastEvents" {
		t.Errorf("graphQLOperation = %q, want %q", got, "pastEvents")
	}
}

func TestGraphQLOperation_NamedQueryWinsOverDirective(t *testing.T) {
	body := `{"query":"#graphql\n  query UpcomingByTopic($limit: Int) {\n    events(limit: $limit) { id }\n  }\n"}`
	got := graphQLOperation("/api/graphql/", body)
	if got != "UpcomingByTopic" {
		t.Errorf("graphQLOperation = %q, want %q", got, "UpcomingByTopic")
	}
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		name string
		cfg  func() *Config
		in   string
		want string
	}{
		{
			name: "uuid segment",
			cfg:  testConfig,
			in:   "/api/history/cfd39f04-708a-4c92-aadf-97a8f61ee539/",
			want: "/api/history/{id}/",
		},
		{
			name: "bare numeric segment",
			cfg:  testConfig,
			in:   "/api/items/123/",
			want: "/api/items/{id}/",
		},
		{
			// 隣り合う数値セグメントはスラッシュを共有するので 1 パスでは片方しか置換されない。
			name: "adjacent numeric segments",
			cfg:  testConfig,
			in:   "/api/2024/11/index",
			want: "/api/{id}/{id}/index",
		},
		{
			// ファイル名は識別子ではなく意味を持つ。
			name: "mixed alphanumeric is preserved",
			cfg:  testConfig,
			in:   "/report/ABC-2024.pdf",
			want: "/report/ABC-2024.pdf",
		},
		{
			name: "version segments are not identifiers",
			cfg:  testConfig,
			in:   "/api/v2/search",
			want: "/api/v2/search",
		},
		{
			name: "custom rule collapses site-specific identifiers",
			cfg: func() *Config {
				c := testConfig()
				c.Normalize.Rules = []Rule{refRule()}
				return c
			},
			in:   "/docs/XY-1234/5678.90",
			want: "/docs/{ref}",
		},
		{
			name: "custom rule keeps the sub-path it captures",
			cfg: func() *Config {
				c := testConfig()
				c.Normalize.Rules = []Rule{refRule()}
				return c
			},
			in:   "/docs/pdf/XY-1234/5678.90",
			want: "/docs/pdf/{ref}",
		},
		{
			// 末尾スラッシュの同一視は設定で明示したときだけ働く。
			name: "trailing slash is off by default",
			cfg:  testConfig,
			in:   "/search",
			want: "/search",
		},
		{
			name: "trailing slash is added when enabled",
			cfg: func() *Config {
				c := testConfig()
				c.Normalize.TrailingSlash = true
				return c
			},
			in:   "/api/v2/search",
			want: "/api/v2/search/",
		},
		{
			name: "trailing slash skips file-like last segments",
			cfg: func() *Config {
				c := testConfig()
				c.Normalize.TrailingSlash = true
				return c
			},
			in:   "/catalog/index.json",
			want: "/catalog/index.json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizePath(tc.cfg(), tc.in); got != tc.want {
				t.Errorf("normalizePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSpecPathMatches(t *testing.T) {
	tests := []struct {
		name     string
		template string
		path     string
		want     bool
	}{
		{"exact", "/search", "/search", true},
		{"param segment", "/catalog/{id}", "/catalog/123", true},
		{"param mid-segment", "/docs/pdf/{ref}", "/docs/pdf/XY-1234", true},
		{"param does not span slashes", "/catalog/{id}", "/catalog/123/extra", false},
		{"trailing slash is optional", "/search/", "/search", true},
		{"different path", "/search", "/catalog", false},
		{"prefix is not a match", "/search", "/search/suggest", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := specPathMatches(tc.template, tc.path); got != tc.want {
				t.Errorf("specPathMatches(%q, %q) = %v, want %v",
					tc.template, tc.path, got, tc.want)
			}
		})
	}
}

func TestClassify_ThreeStates(t *testing.T) {
	cov := coverage{
		specPaths: []string{"/search"},
		codePaths: []string{"/docs/pdf/"},
		filters: []filterRule{
			{prefix: "/internal/login", reason: "UI: ログイン画面"},
		},
	}

	tests := []struct {
		name       string
		endpoint   Endpoint
		wantState  string
		wantReason string
	}{
		{
			name:      "in spec is covered",
			endpoint:  Endpoint{Method: "GET", Path: "/search"},
			wantState: stateCovered,
		},
		{
			// spec を持たないプロジェクトでも、コード内の URL リテラルが根拠になる。
			name:      "implemented in code but absent from spec is covered",
			endpoint:  Endpoint{Method: "GET", Path: "/docs/pdf/{ref}"},
			wantState: stateCovered,
		},
		{
			name:       "excluded by filter keeps its reason",
			endpoint:   Endpoint{Method: "GET", Path: "/internal/login"},
			wantState:  stateFiltered,
			wantReason: "UI: ログイン画面",
		},
		{
			name:      "observed but unknown is uncovered",
			endpoint:  Endpoint{Method: "GET", Path: "/docs/{ref}"},
			wantState: stateUncovered,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cov.classify(tc.endpoint)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.State, tc.wantState)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

func TestClassify_SpecCoverageIgnoresOperation(t *testing.T) {
	// spec に載った GraphQL URL は operation 単位の情報を持たない。それでも
	// 全 operation を covered として扱う。黙って落とすよりは報告できる方がよい。
	cov := coverage{specPaths: []string{"/api/graphql"}}
	got := cov.classify(Endpoint{Method: "POST", Path: "/api/graphql", Operation: "mostRecent"})
	if got.State != stateCovered {
		t.Errorf("state = %q, want %q", got.State, stateCovered)
	}
}

func TestParseFilterFile(t *testing.T) {
	src := `# 除外するエンドポイントパターン (1行1パターン、前方一致)
# 理由カテゴリ: UI=UI専用, ANALYTICS=計測系

/internal/login                          # UI: ログイン画面
/internal/track                          # ANALYTICS: クリック計測
/internal/no-comment
`
	got := parseFilterFile([]byte(src))
	want := []filterRule{
		{prefix: "/internal/login", reason: "UI: ログイン画面"},
		{prefix: "/internal/track", reason: "ANALYTICS: クリック計測"},
		{prefix: "/internal/no-comment", reason: ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rules, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rule %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestExtractCodePaths(t *testing.T) {
	// origin 付きの完全 URL、クエリ付き、重複、素の "/" を 1 度に確認する。
	src := "package client\n" + `
const BaseURL = "https://example.com"

func SearchURL(query string) string {
	v := url.Values{}
	v.Set("q", query)
	return BaseURL + "/search?" + v.Encode()
}

func fullURL() string {
	return "https://example.com/search?q=x"
}

func DocURL(ref string) string { return BaseURL + "/docs/" + ref }

func PDFURL(ref string) string { return BaseURL + "/docs/pdf/" + ref + "?download=true" }

func refOf(href string) string { return strings.TrimPrefix(href, "/docs/") }

func root() string { return "/" }
`
	got := extractCodePaths([]byte(src))
	want := []string{"/docs/", "/docs/pdf/", "/search"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func assertEndpoints(t *testing.T, got, want []Endpoint) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d endpoints, want %d:\n got: %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("endpoint %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
