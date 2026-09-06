package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"

	"github.com/usadamasa/apigap/internal/chrome"
)

func runCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	configPath := fs.String("c", "apigap.yaml", "設定ファイル")
	output := fs.String("output", "", "HAR の出力先 (既定は設定の output)")
	verbose := fs.Bool("verbose", false, "デバッグログを出力する")
	foreground := fs.Bool("foreground", false, "Chrome の窓を前面に出す (設定の foreground を上書き)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *output != "" {
		cfg.Output = *output
	}
	if *foreground {
		cfg.Foreground = true
	}
	if cfg.Output == "" || cfg.Scenarios == "" || cfg.Cookies == "" {
		return errors.New("設定に cookies / scenarios / output が要ります")
	}

	cookies, ua, err := loadCookies(cfg.Cookies)
	if err != nil {
		return err
	}
	scenarios, err := LoadScenarios(cfg.Scenarios)
	if err != nil {
		return err
	}
	if len(scenarios) == 0 {
		return fmt.Errorf("%s にシナリオがありません", cfg.Scenarios)
	}

	// Ctrl-C で Chrome を残さない。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	har, err := capture(ctx, cfg, cookies, ua, scenarios)
	if har != nil {
		// 途中で失敗しても取れた分は残す。観測が目的なので部分結果にも価値がある。
		if werr := writeHAR(cfg.Output, har); werr != nil {
			return errors.Join(err, werr)
		}
		slog.Info("HAR を書きました", "path", cfg.Output, "entries", len(har.Log.Entries))
	}
	return err
}

// cookieFile はログイン用 CLI が書く cookies.json のうち、注入に要る部分。
type cookieFile struct {
	UserAgent string `json:"user_agent"`
	Cookies   []struct {
		Name     string    `json:"name"`
		Value    string    `json:"value"`
		Domain   string    `json:"domain"`
		Path     string    `json:"path"`
		Expires  time.Time `json:"expires,omitzero"`
		HTTPOnly bool      `json:"httpOnly"`
		Secure   bool      `json:"secure"`
	} `json:"cookies"`
}

// loadCookies は期限切れを除いた Cookie と、あれば保存時の User-Agent を返す。
func loadCookies(path string) ([]*http.Cookie, string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- 設定で指定された Cookie ファイル
	if err != nil {
		return nil, "", fmt.Errorf("Cookie ファイルの読み込みに失敗しました: %w", err)
	}
	var f cookieFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, "", fmt.Errorf("Cookie ファイルの解析に失敗しました (%s): %w", path, err)
	}
	now := time.Now()
	var out []*http.Cookie
	var names []string
	for _, c := range f.Cookies {
		if !c.Expires.IsZero() && c.Expires.Before(now) {
			continue
		}
		out = append(out, &http.Cookie{ // #nosec G124 -- 保存済み Cookie をそのまま再送する
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Expires: c.Expires, HttpOnly: c.HTTPOnly, Secure: c.Secure,
		})
		names = append(names, c.Name)
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("%s に有効な Cookie がありません (ログインし直してください)", path)
	}
	// 名前だけ出す。値は出さない。
	slog.Info("Cookie を読みました", "count", len(out), "names", strings.Join(names, ","))
	return out, f.UserAgent, nil
}

func capture(ctx context.Context, cfg *Config, cookies []*http.Cookie, savedUA string, scenarios []Scenario) (*HAR, error) {
	profile, err := os.MkdirTemp("", "apigap-chrome-")
	if err != nil {
		return nil, fmt.Errorf("Chrome プロファイルの一時ディレクトリを作れません: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(profile); err != nil {
			slog.Warn("Chrome プロファイルの削除に失敗しました", "path", profile, "err", err)
		}
	}()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := launch(ctx, cctx, profile, cfg)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	// cf_clearance は発行時の User-Agent に紐づく。ログインに使った Chrome と別物なら
	// 何を観測しても 403 になるので、先に気づけるようにする。
	if ua, err := b.UserAgent(cctx); err == nil && savedUA != "" && ua != savedUA {
		slog.Warn("Cookie 保存時と User-Agent が違います。Cloudflare に弾かれたらログインし直してください", "saved", savedUA, "chrome", ua)
	}
	page, err := b.NewPage(cctx)
	if err != nil {
		return nil, err
	}
	if err := page.SetCookies(cctx, cookies); err != nil {
		return nil, fmt.Errorf("Cookie の注入に失敗しました: %w", err)
	}
	if err := page.EnableNetwork(cctx); err != nil {
		return nil, err
	}

	rec := newRecorder(cfg.HostSet())
	settle := time.Duration(cfg.SettleTimeoutMs) * time.Millisecond
	for _, sc := range scenarios {
		slog.Info("シナリオ開始", "name", sc.Name, "steps", len(sc.Steps))
		for i, step := range sc.Steps {
			target := resolveURL(cfg.BaseURL, step.Navigate)
			rec.startPage(fmt.Sprintf("%s/%d", sc.Name, i), target)

			navCtx, navCancel := context.WithTimeout(ctx, settle)
			if err := page.Navigate(navCtx, target); err != nil {
				slog.Warn("遷移に失敗しました", "url", target, "err", err)
			}
			status, settled := waitSettled(navCtx, b, page.SessionID(), rec)
			navCancel()
			if !settled {
				slog.Warn("ページが落ち着きませんでした (Cloudflare のチャレンジが解けていない可能性)", "url", target, "last_status", status)
			}
			linger(ctx, b, page.SessionID(), rec, time.Duration(step.EffectiveWaitMs())*time.Millisecond)
			slog.Info("NAV", "url", target, "status", status)

			if err := rec.flush(ctx, page); err != nil {
				return rec.har, err
			}
			if ctx.Err() != nil {
				return rec.har, ctx.Err()
			}
		}
	}
	rec.finish()
	return rec.har, nil
}

// launch は Chrome を起動して CDP に繋ぐ。既定の背面起動 (`open -g`) は、LaunchServices に
// 届かないサンドボックス内などで失敗するか、Chrome を起こせないまま固まる。
// その場合は前面起動でやり直す。観測が目的なので、窓が前に出ることより結果が取れないことの方が困る。
//
// ctx は Chrome プロセスの寿命、connectCtx は接続待ちの期限。分けるのは、
// 接続の短い期限をプロセスに渡すと、その時間で Chrome ごと殺されてしまうため。
func launch(ctx, connectCtx context.Context, profile string, cfg *Config) (*chrome.Browser, error) {
	opts := chrome.Options{UserDataDir: profile, Headless: cfg.Headless, Foreground: cfg.Foreground}
	b, err := connect(ctx, connectCtx, opts)
	if err != nil && !opts.Foreground && !opts.Headless && connectCtx.Err() == nil {
		slog.Warn("背面起動に失敗したので前面で起動し直します", "err", err)
		opts.Foreground = true
		b, err = connect(ctx, connectCtx, opts)
	}
	return b, err
}

func connect(ctx, connectCtx context.Context, opts chrome.Options) (*chrome.Browser, error) {
	b, err := chrome.Launch(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := b.Connect(connectCtx); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

// waitSettled はチャレンジでも 3xx でもない Document 応答が来るまでイベントを処理する。
// Cloudflare Turnstile は 403 のチャレンジページを返し、数秒後に自動で解けて同じ URL を
// 読み直す。固定時間の sleep だとチャレンジページだけ記録して次へ進んでしまう。
func waitSettled(ctx context.Context, b *chrome.Browser, sid target.SessionID, rec *recorder) (int, bool) {
	last := 0
	for {
		select {
		case <-ctx.Done():
			return last, false
		case msg, ok := <-b.Events():
			if !ok {
				return last, false
			}
			if msg.SessionID != sid {
				continue
			}
			doc := rec.handle(msg)
			if doc == nil {
				continue
			}
			last = doc.status
			if doc.settled() {
				return last, true
			}
		}
	}
}

// linger は d の間、ページに留まってイベントを処理し続ける。遅延ロードの XHR を拾うため。
func linger(ctx context.Context, b *chrome.Browser, sid target.SessionID, rec *recorder, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case msg, ok := <-b.Events():
			if !ok {
				return
			}
			if msg.SessionID == sid {
				rec.handle(msg)
			}
		}
	}
}

func resolveURL(base, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(ref, "/")
}

// recorder は Network イベントを HAR エントリに組み立てる。
type recorder struct {
	hosts   map[string]bool
	pageref string
	open    map[network.RequestID]*pending
	ready   []*pending
	har     *HAR
}

type pending struct {
	id      network.RequestID
	started time.Time
	entry   Entry
	failed  bool
	size    int
}

// docEvent は対象ホストの Document 応答。ページが落ち着いたかの判定に使う。
type docEvent struct {
	status    int
	challenge bool
}

func (d docEvent) settled() bool {
	return !d.challenge && (d.status < 300 || d.status >= 400)
}

func newRecorder(hosts map[string]bool) *recorder {
	return &recorder{hosts: hosts, open: map[network.RequestID]*pending{}, har: newHAR()}
}

func (r *recorder) startPage(id, title string) {
	r.pageref = id
	r.har.Log.Pages = append(r.har.Log.Pages, Page{StartedDateTime: time.Now(), ID: id, Title: title})
}

// handle はイベント 1 件を取り込む。対象ホストの Document 応答なら docEvent を返す。
// *ExtraInfo 系のイベントは見ない。生の Cookie ヘッダが載っているのはそこで、
// 記録しないのが一番確実な秘匿になる。
func (r *recorder) handle(msg *cdproto.Message) *docEvent {
	switch msg.Method {
	case cdproto.EventNetworkRequestWillBeSent:
		var ev network.EventRequestWillBeSent
		if err := json.Unmarshal(msg.Params, &ev); err != nil {
			slog.Debug("イベントを解釈できませんでした", "method", msg.Method, "err", err)
			return nil
		}
		if ev.RedirectResponse != nil {
			if p, ok := r.open[ev.RequestID]; ok {
				fillResponse(&p.entry, ev.RedirectResponse)
				r.complete(p, ev.WallTime.Time())
			}
		}
		p := &pending{id: ev.RequestID, started: ev.WallTime.Time()}
		p.entry = Entry{
			Pageref:         r.pageref,
			StartedDateTime: p.started,
			Request: Request{
				Method:      ev.Request.Method,
				URL:         ev.Request.URL,
				Cookies:     []NameValue{},
				Headers:     maskHeaders(ev.Request.Headers),
				QueryString: queryString(ev.Request.URL),
				HeadersSize: -1,
				BodySize:    -1,
			},
			Response:     Response{Cookies: []NameValue{}, Headers: []NameValue{}, HeadersSize: -1, BodySize: -1},
			ResourceType: strings.ToLower(string(ev.Type)),
		}
		if body := postData(ev.Request); body != "" {
			p.entry.Request.PostData = &PostData{Text: body}
			p.entry.Request.BodySize = len(body)
			for k, v := range ev.Request.Headers {
				if strings.EqualFold(k, "content-type") {
					p.entry.Request.PostData.MimeType = fmt.Sprint(v)
				}
			}
		}
		r.open[ev.RequestID] = p
	case cdproto.EventNetworkResponseReceived:
		var ev network.EventResponseReceived
		if json.Unmarshal(msg.Params, &ev) != nil {
			return nil
		}
		p, ok := r.open[ev.RequestID]
		if !ok {
			return nil
		}
		fillResponse(&p.entry, ev.Response)
		if ev.Type == network.ResourceTypeDocument && r.hosts[hostOf(ev.Response.URL)] {
			return &docEvent{status: int(ev.Response.Status), challenge: isChallenge(ev.Response.Headers)}
		}
	case cdproto.EventNetworkLoadingFinished:
		var ev network.EventLoadingFinished
		if json.Unmarshal(msg.Params, &ev) != nil {
			return nil
		}
		if p, ok := r.open[ev.RequestID]; ok {
			p.size = int(ev.EncodedDataLength)
			r.complete(p, time.Now())
		}
	case cdproto.EventNetworkLoadingFailed:
		var ev network.EventLoadingFailed
		if json.Unmarshal(msg.Params, &ev) != nil {
			return nil
		}
		if p, ok := r.open[ev.RequestID]; ok {
			p.failed = true
			p.entry.Error = ev.ErrorText
			r.complete(p, time.Now())
		}
	}
	return nil
}

// postData は Network.enable の maxPostDataSize 以内で同梱された POST 本文を復元する。
// Chrome は base64 のチャンク列で渡してくる。
func postData(req *network.Request) string {
	if !req.HasPostData || len(req.PostDataEntries) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, e := range req.PostDataEntries {
		raw, err := base64.StdEncoding.DecodeString(e.Bytes)
		if err != nil {
			return ""
		}
		sb.Write(raw)
	}
	return sb.String()
}

func (r *recorder) complete(p *pending, at time.Time) {
	delete(r.open, p.id)
	p.entry.Time = float64(at.Sub(p.started).Microseconds()) / 1000
	p.entry.Timings = Timings{Wait: p.entry.Time}
	r.ready = append(r.ready, p)
}

func fillResponse(e *Entry, res *network.Response) {
	e.Response.Status = int(res.Status)
	e.Response.StatusText = res.StatusText
	e.Response.HTTPVersion = res.Protocol
	e.Response.Headers = maskHeaders(res.Headers)
	e.Response.Content.MimeType = res.MimeType
	for k, v := range res.Headers {
		if strings.EqualFold(k, "location") {
			e.Response.RedirectURL = fmt.Sprint(v)
		}
	}
}

// isChallenge は Cloudflare のチャレンジ応答かを判定する。チャレンジページは
// cf-mitigated: challenge ヘッダを持つ。
func isChallenge(h network.Headers) bool {
	for k, v := range h {
		if strings.EqualFold(k, "cf-mitigated") && strings.EqualFold(fmt.Sprint(v), "challenge") {
			return true
		}
	}
	return false
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// maxBodyBytes を超える本文はサイズだけ記録する。JS バンドルや PDF を丸ごと抱えると
// HAR が数十 MB になり、gap にも spec 生成にも役に立たない。
const maxBodyBytes = 4 << 20

// wantBody は本文を取りに行く価値があるエントリかを判定する。
func wantBody(p *pending) bool {
	if p.failed || p.size > maxBodyBytes {
		return false
	}
	switch p.entry.ResourceType {
	case "document", "xhr", "fetch", "other":
	default:
		return false
	}
	st := p.entry.Response.Status
	if st == 204 || st == 304 || (st >= 300 && st < 400) {
		return false
	}
	mime := strings.ToLower(p.entry.Response.Content.MimeType)
	for _, skip := range []string{"image/", "font/", "video/", "audio/", "application/pdf", "application/octet-stream"} {
		if strings.HasPrefix(mime, skip) {
			return false
		}
	}
	return true
}

// flush は完了したエントリの本文を取り、HAR に移す。次のページへ遷移すると
// Chrome 側の本文は消えるので、ステップごとに呼ぶ。
func (r *recorder) flush(ctx context.Context, page *chrome.Page) error {
	for _, p := range r.ready {
		if wantBody(p) {
			var body network.GetResponseBodyReturns
			bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := page.Call(bctx, network.CommandGetResponseBody, &network.GetResponseBodyParams{RequestID: p.id}, &body)
			cancel()
			if err != nil {
				slog.Debug("本文を取れませんでした", "url", p.entry.Request.URL, "err", err)
			} else {
				p.entry.Response.Content.Text = body.Body
				if body.Base64encoded {
					p.entry.Response.Content.Encoding = "base64"
				}
			}
		}
		p.entry.Response.Content.Size = p.size
		p.entry.Response.BodySize = p.size
		r.har.Log.Entries = append(r.har.Log.Entries, p.entry)
	}
	r.ready = nil
	return ctx.Err()
}

// finish は応答が来ないまま残ったエントリも HAR に載せる (打ち切られたことが分かるように)。
func (r *recorder) finish() {
	for _, p := range r.ready {
		r.har.Log.Entries = append(r.har.Log.Entries, p.entry)
	}
	r.ready = nil
	// open は map なので、開始時刻で並べてから載せる。
	left := make([]*pending, 0, len(r.open))
	for _, p := range r.open {
		left = append(left, p)
	}
	sort.Slice(left, func(i, j int) bool { return left[i].started.Before(left[j].started) })
	for _, p := range left {
		r.har.Log.Entries = append(r.har.Log.Entries, p.entry)
	}
	r.open = map[network.RequestID]*pending{}
}
