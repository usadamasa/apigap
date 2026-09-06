package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// `apigap gap` が報告する 3 つの状態。
const (
	// stateCovered は、そのエンドポイントが既にリポジトリへ届いていることを意味する。
	// 根拠は coverage.spec の paths か、coverage.code のソース内 URL リテラル。
	stateCovered = "covered"
	// stateUncovered は、通信では観測できたのにリポジトリの誰も知らないことを意味する。
	// 新しい機能の候補はここに出る。
	stateUncovered = "uncovered"
	// stateFiltered は filter ファイルが前もって除外していることを意味する。
	// 隠さずに報告するのは、filter が「レポートを小さく保つ」ための判断であって
	// 「機能として価値がない」という判断ではないため。両者は別の問いになる。
	stateFiltered = "filtered"
)

// Endpoint は観測された API 面 1 つ。GraphQL は 1 つの URL に複数の operation を
// 多重化するので、Operation も同一性の一部として持つ。
type Endpoint struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Operation string `json:"operation,omitempty"`
	Status    int    `json:"status"`
}

// Finding は観測されたエンドポイントとその判定結果の組。
type Finding struct {
	Endpoint
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type filterRule struct {
	prefix string
	reason string
}

type coverage struct {
	specPaths []string
	codePaths []string
	filters   []filterRule
}

// harDoc は HAR 1.2 のうち gap が読む部分だけ。_resourceType は Chrome DevTools が
// 足すカスタムフィールドで、標準 HAR には無いので欠けることがある。
//
// har.go の HAR 型を使い回さないのは、gap が読むのは自分で書いた HAR とは限らないため。
// あちらは startedDateTime を time.Time で受けるので、RFC3339 に収まらない
// タイムスタンプを書く記録ツールの HAR で解析ごと失敗する。読む側は緩くしておく。
type harDoc struct {
	Log struct {
		Entries []struct {
			ResourceType string `json:"_resourceType"`
			Request      struct {
				Method   string `json:"method"`
				URL      string `json:"url"`
				PostData struct {
					Text string `json:"text"`
				} `json:"postData"`
			} `json:"request"`
			Response struct {
				Status int `json:"status"`
			} `json:"response"`
		} `json:"entries"`
	} `json:"log"`
}

func runGap(args []string) error {
	fs := flag.NewFlagSet("gap", flag.ContinueOnError)
	configPath := fs.String("c", "./apigap.yaml", "設定ファイル")
	harPath := fs.String("har", "", "HAR ファイル (既定: 設定の output)")
	format := fs.String("format", "markdown", "出力形式: markdown|json")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *harPath == "" {
		*harPath = cfg.Output
	}
	if *harPath == "" {
		return fmt.Errorf("--har か設定の output のどちらかが必要です")
	}

	harData, err := os.ReadFile(*harPath) // #nosec G304 -- 利用者が指定した HAR
	if err != nil {
		return fmt.Errorf("HAR の読み込みに失敗しました: %w", err)
	}
	endpoints, err := extractEndpoints(cfg, harData)
	if err != nil {
		return err
	}

	cov, err := loadCoverage(cfg)
	if err != nil {
		return err
	}

	findings := make([]Finding, 0, len(endpoints))
	for _, e := range endpoints {
		findings = append(findings, cov.classify(e))
	}

	switch *format {
	case "json":
		return writeGapJSON(os.Stdout, findings)
	case "markdown":
		return writeGapMarkdown(os.Stdout, findings)
	default:
		return fmt.Errorf("不明な形式です: %s", *format)
	}
}

// apiRequestTypes は api_prefixes 未指定時に API とみなす _resourceType。
//
// 許可リストで持つのは、ノイズ側が無限に増えるため。静的アセットや計測ビーコンは
// サイトごとに名前も経路も違うが、「アプリケーションが読みに行くデータ」は
// document / xhr / fetch / other の 4 種に収まる。取りこぼしは次のキャプチャで
// 気づけるが、ノイズを通すと本物の発見が埋もれて気づけない。
var apiRequestTypes = map[string]bool{
	"document": true,
	"xhr":      true,
	"fetch":    true,
	// _resourceType を持たない HAR (DevTools 以外の記録) も落とさない。
	"": true,
	// EventSource など DevTools が分類しきれなかったものが other に来る。
	"other": true,
}

// apiPath は HAR エントリを API とみなすかを判定し、対象ならそのパスを返す。
// gap と sanitize が同じ判定を共有するための唯一の関数。両者がずれると、
// sanitize が捨てたものを gap が「観測されなかった」と読み違える。
func apiPath(cfg *Config, hosts map[string]bool, rawURL, resourceType string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	// blob: や data: はホストを持たないのでここで落ちる。
	if !hosts[u.Host] {
		return "", false
	}
	if len(cfg.APIPrefixes) > 0 {
		// prefix を明示したときは、それが唯一の基準になる。_resourceType は見ない。
		for _, p := range cfg.APIPrefixes {
			if strings.HasPrefix(u.Path, p) {
				return u.Path, true
			}
		}
		return "", false
	}
	return u.Path, apiRequestTypes[strings.ToLower(resourceType)]
}

// extractEndpoints は生の HAR をソート済み・重複排除済みのエンドポイント一覧に畳む。
func extractEndpoints(cfg *Config, data []byte) ([]Endpoint, error) {
	var doc harDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("HAR の解析に失敗しました: %w", err)
	}

	hosts := cfg.HostSet()
	seen := make(map[Endpoint]int)
	for _, e := range doc.Log.Entries {
		path, ok := apiPath(cfg, hosts, e.Request.URL, e.ResourceType)
		if !ok {
			continue
		}

		ep := Endpoint{
			Method:    e.Request.Method,
			Path:      normalizePath(cfg, path),
			Operation: graphQLOperation(path, e.Request.PostData.Text),
		}
		// 成功ステータスを優先する。同じエンドポイントは正常系と異常系の両方で
		// 叩かれるのが普通で、失敗の方を報告すると生きている面を壊れていると読み違える。
		prev, exists := seen[ep]
		if !exists || (!isSuccess(prev) && isSuccess(e.Response.Status)) {
			seen[ep] = e.Response.Status
		}
	}

	endpoints := make([]Endpoint, 0, len(seen))
	for ep, status := range seen {
		ep.Status = status
		endpoints = append(endpoints, ep)
	}
	sort.Slice(endpoints, func(i, j int) bool {
		a, b := endpoints[i], endpoints[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Operation != b.Operation {
			return a.Operation < b.Operation
		}
		return a.Method < b.Method
	})
	return endpoints, nil
}

func isSuccess(status int) bool {
	return status >= 200 && status < 300
}

var (
	uuidRe       = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	numericSegRe = regexp.MustCompile(`(^|/)[0-9]+(/|$)`)
)

// normalizePath は識別子を {id} に置き換え、多数の識別子で叩かれた 1 つの
// エンドポイントを 1 行に畳む。
//
// 既定で置き換えるのは、識別子だと言い切れる形だけ (UUID と全桁数字のセグメント)。
// ABC-2024.pdf や index.json のような混在した形はそのまま残す。畳みすぎると
// 別々のエンドポイントが 1 行に潰れて実在する面が消えるので、そちらの方が高くつく。
//
// サイト固有の採番体系は normalize.rules で足す。コードに持ち込まないのは、
// 対象サイトごとの違いを設定へ寄せるという方針のため。
func normalizePath(cfg *Config, path string) string {
	path = uuidRe.ReplaceAllString(path, "{id}")
	// 2 度適用する。隣り合う数値セグメントは間のスラッシュを共有するので、
	// 1 度の走査では 1 つおきにしか置換されない。
	for range 2 {
		path = numericSegRe.ReplaceAllString(path, "${1}{id}${2}")
	}
	for _, r := range cfg.Normalize.Rules {
		// パターンは LoadConfig が検証済み。設定を経由せず Config を組み立てた
		// 場合だけ、ここでコンパイルに失敗しうるので黙って読み飛ばす。
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			continue
		}
		path = re.ReplaceAllString(path, r.Replace)
	}
	if cfg.Normalize.TrailingSlash {
		path = withTrailingSlash(path)
	}
	return path
}

// withTrailingSlash は末尾スラッシュの有無を同一視する。Django 系のサイトは
// 同じリソースを両方の形で返すため、揃えないと 1 つの面が 2 行に割れる。
// 最後のセグメントがファイル名に見えるもの (index.json) はそのまま残す。
func withTrailingSlash(path string) string {
	if strings.HasSuffix(path, "/") {
		return path
	}
	last := path[strings.LastIndex(path, "/")+1:]
	if strings.Contains(last, ".") {
		return path
	}
	return path + "/"
}

var (
	graphQLOperationRe = regexp.MustCompile(`\b(?:query|mutation)\s+(\w+)`)
	// 無名の operation も、最初に選択するフィールドで自分を名乗ることが多い。
	// そのフィールドには意味のある alias が付いているのが普通。
	graphQLAnonFieldRe = regexp.MustCompile(`\b(?:query|mutation)\s*\{\s*(\w+)`)
)

// graphQLOperation はリクエストボディが運ぶ GraphQL operation 名を返す。
// GraphQL でないエンドポイントと解析できないボディは "" になる。
func graphQLOperation(path, postData string) string {
	if !strings.Contains(path, "graphql") || postData == "" {
		return ""
	}
	var body struct {
		Query     string `json:"query"`
		QueryName string `json:"queryName"`
	}
	if err := json.Unmarshal([]byte(postData), &body); err != nil {
		return ""
	}
	if m := graphQLOperationRe.FindStringSubmatch(body.Query); m != nil {
		return m[1]
	}
	if m := graphQLAnonFieldRe.FindStringSubmatch(body.Query); m != nil {
		return m[1]
	}
	// 最後はクライアント自身が付けたラベルに頼る。
	return body.QueryName
}

// loadCoverage は「リポジトリが既に知っているエンドポイント」を集める。
// spec と filter はどちらも省略可能で、未設定なら単にその根拠が無いものとして扱う。
func loadCoverage(cfg *Config) (coverage, error) {
	var cov coverage

	if cfg.Coverage.Spec != "" {
		specData, err := os.ReadFile(cfg.Coverage.Spec) // #nosec G304 -- 設定で指定された spec
		if err != nil {
			return cov, fmt.Errorf("spec の読み込みに失敗しました: %w", err)
		}
		cov.specPaths, err = parseSpecPaths(specData)
		if err != nil {
			return cov, fmt.Errorf("spec の解析に失敗しました: %w", err)
		}
	}

	var codePaths []string
	for _, dir := range cfg.Coverage.Code {
		found, err := scanCodePaths(dir)
		if err != nil {
			return cov, fmt.Errorf("コードの走査に失敗しました (%s): %w", dir, err)
		}
		codePaths = append(codePaths, found...)
	}
	cov.codePaths = dedupeSorted(codePaths)

	if cfg.Filter != "" {
		filterData, err := os.ReadFile(cfg.Filter) // #nosec G304 -- 設定で指定された filter
		if err != nil {
			return cov, fmt.Errorf("filter の読み込みに失敗しました: %w", err)
		}
		cov.filters = parseFilterFile(filterData)
	}

	return cov, nil
}

func parseSpecPaths(data []byte) ([]string, error) {
	var spec struct {
		Paths map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(spec.Paths))
	for p := range spec.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

// scanCodePaths は非テストの Go ソースから URL リテラルを集める。spec だけが
// 根拠ではない。spec を持たないプロジェクトも多いし、spec に載せ忘れたまま
// コードから直接叩いているエンドポイントもある。
func scanCodePaths(dir string) ([]string, error) {
	var all []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path) // #nosec G304 -- 走査対象のソース
		if readErr != nil {
			return readErr
		}
		all = append(all, extractCodePaths(data)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dedupeSorted(all), nil
}

// codePathRe は Go の文字列リテラルのうち、パスか完全 URL に見えるものを拾う。
// origin の部分は任意にしてあり、"https://example.com/a/b" と "/a/b" の
// どちらの書き方でも同じパスとして扱える。空白を含むリテラルは散文なので除く。
var codePathRe = regexp.MustCompile(`"((?:https?://[^"/\s]+)?/[^"\s]*)"`)

func extractCodePaths(src []byte) []string {
	matches := codePathRe.FindAllSubmatch(src, -1)
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		p := string(m[1])
		// origin を剥がしてパスだけにする。
		if i := strings.Index(p, "://"); i >= 0 {
			p = p[strings.Index(p[i+3:], "/")+i+3:]
		}
		// クエリはエンドポイントの同一性に関わらない。
		if i := strings.Index(p, "?"); i >= 0 {
			p = p[:i]
		}
		// 素の "/" は前方一致であらゆるパスに当たり、すべてを covered にしてしまう。
		if p == "/" {
			continue
		}
		paths = append(paths, p)
	}
	return dedupeSorted(paths)
}

func dedupeSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// parseFilterFile は 1 行 1 prefix の除外リストを読む。# 以降は除外の理由として
// 保存し、レポートにそのまま出す。理由の無い除外は後から誰も判断できない。
func parseFilterFile(data []byte) []filterRule {
	var rules []filterRule
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		prefix, reason := line, ""
		if before, after, found := strings.Cut(line, "#"); found {
			prefix = strings.TrimSpace(before)
			reason = strings.TrimSpace(after)
		}
		rules = append(rules, filterRule{prefix: prefix, reason: reason})
	}
	return rules
}

// specPathMatches は観測パスが spec のテンプレートを満たすかを返す。
// テンプレートのパラメータ ({bookId}) は 1 セグメント分に相当するので、
// スラッシュをまたがせない。
func specPathMatches(template, path string) bool {
	var pattern strings.Builder
	pattern.WriteString("^")
	// 末尾スラッシュは両側で任意にする。同じリソースがどちらでも返るうえ、
	// spec のテンプレート自体が付けたり付けなかったりで一貫しない。
	rest := strings.TrimSuffix(template, "/")
	for {
		open := strings.Index(rest, "{")
		if open < 0 {
			pattern.WriteString(regexp.QuoteMeta(rest))
			break
		}
		closeIdx := strings.Index(rest[open:], "}")
		if closeIdx < 0 {
			pattern.WriteString(regexp.QuoteMeta(rest))
			break
		}
		pattern.WriteString(regexp.QuoteMeta(rest[:open]))
		pattern.WriteString(`[^/]+`)
		rest = rest[open+closeIdx+1:]
	}
	pattern.WriteString("/?$")

	re, err := regexp.Compile(pattern.String())
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

func (c coverage) classify(e Endpoint) Finding {
	// covered が filtered に優先する。既にクライアントへ届いているなら、
	// filter が決めるのは「レポートに出すか」だけで、実装済みという事実は動かない。
	for _, tmpl := range c.specPaths {
		if specPathMatches(tmpl, e.Path) {
			return Finding{Endpoint: e, State: stateCovered}
		}
	}
	for _, p := range c.codePaths {
		// ponytail: 前方一致なので、リテラル /docs/ は /docs/abs/... まで covered にする。
		// 粗いが実装済みの周辺を見落とすより安全。厳密にするならセグメント単位の一致へ。
		if strings.HasPrefix(e.Path, p) {
			return Finding{Endpoint: e, State: stateCovered}
		}
	}
	for _, r := range c.filters {
		if strings.HasPrefix(e.Path, r.prefix) {
			return Finding{Endpoint: e, State: stateFiltered, Reason: r.reason}
		}
	}
	return Finding{Endpoint: e, State: stateUncovered}
}

func writeGapJSON(w io.Writer, findings []Finding) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"findings": findings,
		"summary":  summarize(findings),
	})
}

func summarize(findings []Finding) map[string]int {
	counts := map[string]int{stateUncovered: 0, stateFiltered: 0, stateCovered: 0}
	for _, f := range findings {
		counts[f.State]++
	}
	return counts
}

func writeGapMarkdown(w io.Writer, findings []Finding) error {
	counts := summarize(findings)
	fmt.Fprintf(w, "# API surface gap\n\n")
	fmt.Fprintf(w, "observed=%d uncovered=%d filtered=%d covered=%d\n",
		len(findings), counts[stateUncovered], counts[stateFiltered], counts[stateCovered])

	sections := []struct {
		state string
		title string
		note  string
	}{
		{stateUncovered, "Uncovered", "Observed in captured traffic, unknown to this repository."},
		{stateFiltered, "Filtered", "Dropped by the filter file. The reason is a reporting judgement, not a verdict on capability value."},
		{stateCovered, "Covered", "Already reachable through the configured spec or a URL literal in the scanned sources."},
	}

	for _, s := range sections {
		rows := filterByState(findings, s.state)
		fmt.Fprintf(w, "\n## %s (%d)\n\n%s\n\n", s.title, len(rows), s.note)
		if len(rows) == 0 {
			fmt.Fprintf(w, "_none_\n")
			continue
		}
		fmt.Fprintf(w, "| method | path | operation | status | reason |\n")
		fmt.Fprintf(w, "|---|---|---|---|---|\n")
		for _, f := range rows {
			fmt.Fprintf(w, "| %s | `%s` | %s | %d | %s |\n",
				f.Method, f.Path, orDash(f.Operation), f.Status, orDash(f.Reason))
		}
	}
	return nil
}

func filterByState(findings []Finding, state string) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.State == state {
			out = append(out, f)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
