package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// probe は保存済み Cookie + UA を付けた HTTP クライアントで、ブラウザを起こさずに
// ページが取れるかを測る。capture がブラウザ側の観測なのに対し、こちらは
// 「ブラウザ無しでどこまで届くか」の観測。
//
// TLS の ClientHello・ALPN・リクエストヘッダをフラグで切り替えられるのは、
// 通る / 通らないを分ける軸がどれなのか、事前には決められないため。

const (
	tlsModeGo     = "go"
	tlsModeChrome = "chrome"

	// probeTimeout は 1 URL あたりの上限。黙って止まったままにしない。
	probeTimeout = 60 * time.Second
	// probeMaxRedirects はリダイレクトを追う上限。
	probeMaxRedirects = 10
)

// probeOptions は 1 リクエストの組み立てを決める軸。TLS 側の軸は transport が持つ。
type probeOptions struct {
	// plain は sec-ch-ua / sec-fetch-* / upgrade-insecure-requests を付けない。
	plain bool
	// post はクエリを form body にして POST する。
	post bool
	// noRedirect はリダイレクトを追わず、最初の応答をそのまま返す。
	noRedirect bool
}

// probeResult は 1 URL の観測結果。Set-Cookie は名前だけ持つ。HAR と同じく値は残さない。
type probeResult struct {
	URL         string   `json:"url"`
	Proto       string   `json:"proto,omitempty"`
	Status      int      `json:"status"`
	CFMitigated string   `json:"cf_mitigated,omitempty"`
	ContentType string   `json:"content_type,omitempty"`
	Length      int      `json:"length"`
	DurationMs  int64    `json:"duration_ms"`
	Title       string   `json:"title,omitempty"`
	FinalURL    string   `json:"final_url,omitempty"`
	SetCookies  []string `json:"set_cookies,omitempty"`
	Error       string   `json:"error,omitempty"`
}

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	configPath := fs.String("c", "./apigap.yaml", "設定ファイル")
	tlsMode := fs.String("tls", tlsModeGo, "ClientHello: go (crypto/tls) | chrome (utls)")
	h1 := fs.Bool("h1", false, "ALPN を http/1.1 だけにする")
	plain := fs.Bool("plain", false, "sec-ch-ua / sec-fetch-* を付けない")
	post := fs.Bool("post", false, "クエリを form body にして POST する")
	noRedirect := fs.Bool("no-redirect", false, "リダイレクトを追わない")
	out := fs.String("out", "", "最後のレスポンス本文の書き出し先")
	format := fs.String("format", "markdown", "出力形式: markdown|json")
	verbose := fs.Bool("verbose", false, "デバッグログを出力する")
	if err := fs.Parse(args); err != nil {
		return err
	}
	targets := fs.Args()
	if len(targets) == 0 {
		return errors.New("URL を 1 つ以上指定してください")
	}
	if *tlsMode != tlsModeGo && *tlsMode != tlsModeChrome {
		return fmt.Errorf("不明な --tls です: %s (go|chrome)", *tlsMode)
	}
	if *format != "markdown" && *format != "json" {
		return fmt.Errorf("不明な形式です: %s", *format)
	}
	if *verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if cfg.Cookies == "" {
		return errors.New("設定に cookies が要ります")
	}
	cookies, ua, err := loadCookies(cfg.Cookies, cfg.CookieKeys)
	if err != nil {
		return err
	}
	if ua == "" {
		slog.Warn("Cookie ファイルに User-Agent がありません。UA を付けずに送ります",
			"key", cfg.CookieKeys.UserAgent)
	}

	client, err := probeClient(cfg.BaseURL, cookies, *tlsMode, *h1)
	if err != nil {
		return err
	}

	opt := probeOptions{plain: *plain, post: *post, noRedirect: *noRedirect}
	results := make([]probeResult, 0, len(targets))
	var lastBody []byte
	for _, target := range targets {
		res, body := probeOne(client, target, ua, opt)
		results = append(results, res)
		lastBody = body
	}
	if *out != "" && lastBody != nil {
		if err := os.WriteFile(*out, lastBody, 0o600); err != nil {
			return fmt.Errorf("本文の書き出しに失敗しました: %w", err)
		}
	}
	if *format == "json" {
		return writeProbeJSON(os.Stdout, results)
	}
	return writeProbeMarkdown(os.Stdout, results)
}

// probeClient は base_url のホストに属する Cookie だけを載せたクライアントを返す。
// 絞り込みは cookiejar の RFC 6265 判定に任せる。
func probeClient(baseURL string, cookies []*http.Cookie, tlsMode string, h1 bool) (*http.Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("base_url が不正です: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	jar.SetCookies(base, cookies)
	loaded := jar.Cookies(base)
	if len(loaded) == 0 {
		slog.Warn("base_url のホストに載る Cookie がありません", "host", base.Host)
	} else {
		slog.Info("Cookie を jar に入れました", "host", base.Host, "count", len(loaded))
	}

	dialTLS := func(ctx context.Context, _, addr string, _ *tls.Config) (net.Conn, error) {
		return probeDial(ctx, addr, tlsMode, h1)
	}
	var rt http.RoundTripper
	if h1 {
		// DialTLSContext を渡すと http.Transport は h2 へ切り替えないので、これが http/1.1 の軸になる。
		rt = &http.Transport{DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialTLS(ctx, network, addr, nil)
		}}
	} else {
		rt = &http2.Transport{DialTLSContext: dialTLS}
	}
	return &http.Client{Transport: rt, Jar: jar, Timeout: probeTimeout}, nil
}

// probeOne は 1 URL を叩いて結果と本文を返す。通信の失敗は error ではなく
// probeResult.Error に載せる。届かなかったこと自体が観測結果のため。
func probeOne(client *http.Client, target, ua string, opt probeOptions) (probeResult, []byte) {
	res := probeResult{URL: target}

	// 302 で発行される Set-Cookie は最終応答に残らないので、途中の応答から名前を拾う。
	var hopCookies []string
	if opt.noRedirect {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	} else {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if req.Response != nil {
				hopCookies = append(hopCookies, setCookieNames(req.Response.Header)...)
			}
			if len(via) >= probeMaxRedirects {
				return fmt.Errorf("リダイレクトが %d 回を超えました", probeMaxRedirects)
			}
			return nil
		}
	}

	req, err := probeRequest(target, opt.post)
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	setProbeHeaders(req.Header, ua, opt.plain)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		res.DurationMs = time.Since(start).Milliseconds()
		res.Error = err.Error()
		return res, nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, rerr := readProbeBody(resp)
	res.DurationMs = time.Since(start).Milliseconds()
	res.Proto = resp.Proto
	res.Status = resp.StatusCode
	res.CFMitigated = resp.Header.Get("cf-mitigated")
	res.ContentType = resp.Header.Get("Content-Type")
	res.Length = len(body)
	res.Title = htmlTitle(body)
	res.FinalURL = resp.Request.URL.String()
	res.SetCookies = append(hopCookies, setCookieNames(resp.Header)...)
	if rerr != nil {
		res.Error = rerr.Error()
	}
	return res, body
}

// probeRequest は GET か、クエリを form body に移した POST を組む。
func probeRequest(target string, post bool) (*http.Request, error) {
	if !post {
		return http.NewRequest(http.MethodGet, target, nil)
	}
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	body := u.RawQuery
	u.RawQuery = ""
	req, err := http.NewRequest(http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

const probeAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"

// setProbeHeaders はナビゲーション相当のヘッダを組む。plain なら Chrome 固有のぶんを外す。
func setProbeHeaders(h http.Header, ua string, plain bool) {
	if ua != "" {
		h.Set("User-Agent", ua)
	}
	h.Set("Accept", probeAccept)
	h.Set("Accept-Language", "en-US,en;q=0.9")
	// gzip を自分で付けると Go は自動展開しないので、readProbeBody で展開する。
	h.Set("Accept-Encoding", "gzip")
	if plain {
		return
	}
	// sec-ch-ua は Chromium 系だけのヒント。UA から版が読めないなら付けない。
	if v := chromeMajorVersion(ua); v != "" {
		h.Set("sec-ch-ua", fmt.Sprintf(`"Chromium";v=%q, "Google Chrome";v=%q, "Not-A.Brand";v="99"`, v, v))
		h.Set("sec-ch-ua-mobile", "?0")
		h.Set("sec-ch-ua-platform", fmt.Sprintf("%q", platformHint(ua)))
	}
	h.Set("sec-fetch-dest", "document")
	h.Set("sec-fetch-mode", "navigate")
	h.Set("sec-fetch-site", "none")
	h.Set("sec-fetch-user", "?1")
	h.Set("upgrade-insecure-requests", "1")
}

var chromeVersionRe = regexp.MustCompile(`Chrome/(\d+)`)

func chromeMajorVersion(ua string) string {
	if m := chromeVersionRe.FindStringSubmatch(ua); m != nil {
		return m[1]
	}
	return ""
}

// platformHint は UA から sec-ch-ua-platform の値を決める。UA と食い違わせないため、
// 固定値は持たない。
func platformHint(ua string) string {
	switch {
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "Macintosh"):
		return "macOS"
	case strings.Contains(ua, "Windows"):
		return "Windows"
	default:
		return "Linux"
	}
}

func readProbeBody(resp *http.Response) ([]byte, error) {
	var r io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip を展開できません: %w", err)
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	return io.ReadAll(r)
}

var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func htmlTitle(body []byte) string {
	if m := titleRe.FindSubmatch(body); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// setCookieNames は Set-Cookie の名前だけを返す。値は持ち出さない。
func setCookieNames(h http.Header) []string {
	var names []string
	for _, sc := range h.Values("Set-Cookie") {
		name, _, _ := strings.Cut(sc, "=")
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func writeProbeJSON(w io.Writer, results []probeResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"results": results})
}

func writeProbeMarkdown(w io.Writer, results []probeResult) error {
	if _, err := fmt.Fprint(w, "# probe\n\n"); err != nil {
		return err
	}
	cols := []string{"url", "proto", "status", "cf-mitigated", "content-type", "bytes", "ms", "title", "final", "set-cookie", "error"}
	if _, err := fmt.Fprintf(w, "| %s |\n|%s\n",
		strings.Join(cols, " | "), strings.Repeat(" --- |", len(cols))); err != nil {
		return err
	}
	for _, r := range results {
		status := ""
		if r.Status != 0 {
			status = fmt.Sprint(r.Status)
		}
		cells := []string{
			r.URL, r.Proto, status, r.CFMitigated, r.ContentType,
			fmt.Sprint(r.Length), fmt.Sprint(r.DurationMs), r.Title, r.FinalURL,
			strings.Join(r.SetCookies, " "), r.Error,
		}
		for i, c := range cells {
			cells[i] = escapeCell(c)
		}
		if _, err := fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | ")); err != nil {
			return err
		}
	}
	return nil
}

var cellBreakRe = regexp.MustCompile(`\s+`)

func escapeCell(s string) string {
	return strings.ReplaceAll(cellBreakRe.ReplaceAllString(strings.TrimSpace(s), " "), "|", `\|`)
}

// probeDial は (必要なら CONNECT プロキシ経由で) addr へ繋ぎ、TLS ハンドシェイクまで済ませる。
func probeDial(ctx context.Context, addr, tlsMode string, h1 bool) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	conn, err := dialMaybeProxy(ctx, addr)
	if err != nil {
		return nil, err
	}
	alpn := []string{"h2", "http/1.1"}
	if h1 {
		alpn = []string{"http/1.1"}
	}
	if tlsMode == tlsModeGo {
		tc := tls.Client(conn, &tls.Config{ServerName: host, NextProtos: alpn, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return tc, nil
	}

	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Chrome の ClientHello は ALPN に h2 を含む。--h1 では spec 側の拡張を差し替える。
	for _, e := range spec.Extensions {
		if a, ok := e.(*utls.ALPNExtension); ok {
			a.AlpnProtocols = alpn
		}
	}
	uc := utls.UClient(conn, &utls.Config{ServerName: host, MinVersion: tls.VersionTLS12}, utls.HelloCustom)
	if err := uc.ApplyPreset(&spec); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := uc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	slog.Debug("TLS ハンドシェイク", "hello", tlsMode, "alpn", uc.ConnectionState().NegotiatedProtocol)
	return uc, nil
}

// dialMaybeProxy は HTTPS_PROXY があれば CONNECT を通してから素の TCP を返す。
// http.Transport の Proxy は DialTLSContext と併用できないので、自前で張る。
func dialMaybeProxy(ctx context.Context, addr string) (net.Conn, error) {
	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		proxy = os.Getenv("https_proxy")
	}
	var d net.Dialer
	if proxy == "" {
		return d.DialContext(ctx, "tcp", addr)
	}
	pu, err := url.Parse(proxy)
	if err != nil {
		// url.Error は元の URL をそのまま出す。Basic 認証のパスワードごと載るので包まない。
		return nil, errors.New("HTTPS_PROXY を URL として読めません")
	}
	conn, err := d.DialContext(ctx, "tcp", pu.Host)
	if err != nil {
		return nil, err
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if pu.User != nil {
		pw, _ := pu.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(pu.User.Username() + ":" + pw))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		_ = conn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT: %s", resp.Status)
	}
	return conn, nil
}
