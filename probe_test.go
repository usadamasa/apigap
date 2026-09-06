package main

import (
	"compress/gzip"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// newTestClient は cookiejar 付きの素のクライアントを返す。probe の transport は
// TLS の指紋を切り替えるためのもので、ヘッダやリダイレクトの挙動とは独立している。
func newTestClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func TestProbeChromeHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer srv.Close()

	if res, _ := probeOne(newTestClient(t), srv.URL, testUA, probeOptions{}); res.Error != "" {
		t.Fatalf("probeOne: %s", res.Error)
	}
	want := map[string]string{
		"User-Agent":                testUA,
		"Accept-Encoding":           "gzip",
		"sec-ch-ua":                 `"Chromium";v="152", "Google Chrome";v="152", "Not-A.Brand";v="99"`,
		"sec-ch-ua-mobile":          "?0",
		"sec-ch-ua-platform":        `"macOS"`,
		"sec-fetch-dest":            "document",
		"sec-fetch-mode":            "navigate",
		"upgrade-insecure-requests": "1",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
}

func TestProbePlainDropsClientHints(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer srv.Close()

	if res, _ := probeOne(newTestClient(t), srv.URL, testUA, probeOptions{plain: true}); res.Error != "" {
		t.Fatalf("probeOne: %s", res.Error)
	}
	for _, k := range []string{"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "sec-fetch-user", "upgrade-insecure-requests"} {
		if v := got.Get(k); v != "" {
			t.Errorf("--plain なのに %s=%q が付いています", k, v)
		}
	}
	// UA と Accept は --plain でも落とさない。Chrome 固有のヒントではないため。
	if got.Get("User-Agent") != testUA {
		t.Errorf("User-Agent = %q", got.Get("User-Agent"))
	}
	if got.Get("Accept") == "" {
		t.Error("Accept が落ちています")
	}
}

// UA から Chrome のバージョンを読めないときは、空のヒントを送るより付けないほうがよい。
func TestProbeNonChromeUADropsClientHints(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer srv.Close()

	const ua = "Mozilla/5.0 (X11; Linux x86_64) Gecko/20100101 Firefox/140.0"
	if res, _ := probeOne(newTestClient(t), srv.URL, ua, probeOptions{}); res.Error != "" {
		t.Fatalf("probeOne: %s", res.Error)
	}
	if v := got.Get("sec-ch-ua"); v != "" {
		t.Errorf("sec-ch-ua = %q, want empty", v)
	}
	if got.Get("sec-fetch-mode") != "navigate" {
		t.Error("sec-fetch-* は Chrome 固有ではないので残す")
	}
}

func TestProbePost(t *testing.T) {
	var (
		method, ctype, body string
		query               string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ctype, query = r.Method, r.Header.Get("Content-Type"), r.URL.RawQuery
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		body = string(b)
	}))
	defer srv.Close()

	res, _ := probeOne(newTestClient(t), srv.URL+"/search?q=go&n=3", testUA, probeOptions{post: true})
	if res.Error != "" {
		t.Fatalf("probeOne: %s", res.Error)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s", method)
	}
	if ctype != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ctype)
	}
	if body != "q=go&n=3" {
		t.Errorf("body = %q, want %q", body, "q=go&n=3")
	}
	if query != "" {
		t.Errorf("クエリが URL に残っています: %q", query)
	}
}

// リダイレクト先で発行される Set-Cookie は最終応答に残らないので、途中の応答から名前を拾う。
// 値は HAR と同じく伏せる。
func TestProbeFollowsRedirectAndReportsCookieNames(t *testing.T) {
	var hop2 *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/entry":
			http.SetCookie(w, &http.Cookie{Name: "SESSIONID", Value: "s3cret", Path: "/"})
			http.Redirect(w, r, "/landing", http.StatusFound)
		default:
			hop2 = r.Clone(r.Context())
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><head><title>Landing</title></head></html>"))
		}
	}))
	defer srv.Close()

	client := newTestClient(t)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.Jar.SetCookies(base, []*http.Cookie{{Name: "seeded", Value: "v", Path: "/"}})

	res, _ := probeOne(client, srv.URL+"/entry", testUA, probeOptions{})
	if res.Error != "" {
		t.Fatalf("probeOne: %s", res.Error)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", res.Status)
	}
	if !strings.HasSuffix(res.FinalURL, "/landing") {
		t.Errorf("final = %q, want .../landing", res.FinalURL)
	}
	if res.Title != "Landing" {
		t.Errorf("title = %q", res.Title)
	}
	if len(res.SetCookies) != 1 || res.SetCookies[0] != "SESSIONID" {
		t.Errorf("set-cookie = %v, want [SESSIONID]", res.SetCookies)
	}
	for _, name := range res.SetCookies {
		if strings.Contains(name, "s3cret") || strings.Contains(name, "=") {
			t.Errorf("Set-Cookie の値が漏れています: %q", name)
		}
	}
	if hop2 == nil {
		t.Fatal("リダイレクト先に届いていません")
	}
	if c, err := hop2.Cookie("seeded"); err != nil || c.Value != "v" {
		t.Errorf("seed した Cookie が 2 ホップ目に載っていません: %v", err)
	}
	if _, err := hop2.Cookie("SESSIONID"); err != nil {
		t.Errorf("302 で発行された Cookie が 2 ホップ目に載っていません: %v", err)
	}
}

func TestProbeNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/landing", http.StatusFound)
	}))
	defer srv.Close()

	res, _ := probeOne(newTestClient(t), srv.URL+"/entry", testUA, probeOptions{noRedirect: true})
	if res.Error != "" {
		t.Fatalf("probeOne: %s", res.Error)
	}
	if res.Status != http.StatusFound {
		t.Errorf("status = %d, want 302", res.Status)
	}
	if !strings.HasSuffix(res.FinalURL, "/entry") {
		t.Errorf("final = %q, want .../entry", res.FinalURL)
	}
}

// Accept-Encoding を自分で付けると Go は自動展開しないので、probe 側で展開する。
func TestProbeDecompressesGzip(t *testing.T) {
	const html = "<html><head><title>Gzipped</title></head><body>ok</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/html")
		gz := gzip.NewWriter(w)
		defer func() { _ = gz.Close() }()
		_, _ = gz.Write([]byte(html))
	}))
	defer srv.Close()

	res, body := probeOne(newTestClient(t), srv.URL, testUA, probeOptions{})
	if res.Error != "" {
		t.Fatalf("probeOne: %s", res.Error)
	}
	if string(body) != html {
		t.Errorf("body = %q", body)
	}
	if res.Length != len(html) {
		t.Errorf("length = %d, want %d (展開後の長さ)", res.Length, len(html))
	}
	if res.Title != "Gzipped" {
		t.Errorf("title = %q", res.Title)
	}
}

func TestProbeUnreachableHostIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	res, _ := probeOne(newTestClient(t), addr, testUA, probeOptions{})
	if res.Error == "" {
		t.Fatal("届かない相手なのにエラーが記録されていません")
	}
	if res.Status != 0 {
		t.Errorf("status = %d, want 0", res.Status)
	}
}

func TestWriteProbeMarkdownEscapesPipes(t *testing.T) {
	var sb strings.Builder
	err := writeProbeMarkdown(&sb, []probeResult{{
		URL: "https://example.com/a", Proto: "HTTP/2.0", Status: 200,
		ContentType: "text/html", Length: 42, Title: "a | b", FinalURL: "https://example.com/a",
		SetCookies: []string{"SESSIONID"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"https://example.com/a", "HTTP/2.0", "200", "SESSIONID", `a \| b`} {
		if !strings.Contains(out, want) {
			t.Errorf("出力に %q がありません:\n%s", want, out)
		}
	}
	// 表が壊れていないこと (行ごとのセル数が揃う)。
	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "|") {
			rows = append(rows, line)
		}
	}
	if len(rows) < 3 {
		t.Fatalf("表の行が足りません:\n%s", out)
	}
	// エスケープ済みの \| はセルの区切りではないので数から外す。
	cells := func(row string) int { return strings.Count(row, "|") - strings.Count(row, `\|`) }
	n := cells(rows[0])
	for i, r := range rows {
		if cells(r) != n {
			t.Errorf("行 %d のセル数が揃っていません: %q", i, r)
		}
	}
}
