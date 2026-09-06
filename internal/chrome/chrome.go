// Package chrome はシステムの Chrome を起動し、生の CDP (Chrome DevTools Protocol) で操作する。
//
// chromedp の高レベル API (NewContext / Run) や Playwright は使わない。どちらもターゲットに
// 繋いだ瞬間に Runtime.enable を打ち、Cloudflare Turnstile がそれを検知してチャレンジを
// 解かなくなる。ここでは websocket 接続 (chromedp.Conn) と cdproto の型だけを借り、
// Network ドメインのイベントを購読するだけで DOM や Runtime には一切触れない。
package chrome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Options は Chrome の起動オプション。
type Options struct {
	UserDataDir string // 必須。プロファイルの置き場。
	Headless    bool   // --headless=new で起動する。Turnstile 配下では interactive になり通らない
	Foreground  bool   // 窓を前面に出す。既定 (false) では macOS で `open -g` を使い、作業中のアプリからフォーカスを奪わない
}

// Browser は起動した Chrome プロセスと、その CDP 接続。
type Browser struct {
	cmd        *exec.Cmd
	background bool // `open` 経由で起動した。cmd は open であって Chrome ではない
	port       int
	exited     chan error
	conn       *chromedp.Conn
	next       atomic.Int64
	mu         sync.Mutex
	pending    map[int64]chan *cdproto.Message
	in         chan *cdproto.Message
	events     chan *cdproto.Message
}

// Launch は Chrome を起動する。CDP にはまだ繋がない (Connect を呼ぶ)。
// 自動操作系のフラグ (--enable-automation 等) は付けない。Turnstile に弾かれる。
func Launch(ctx context.Context, opts Options) (*Browser, error) {
	path, err := findChrome()
	if err != nil {
		return nil, err
	}
	if opts.UserDataDir == "" {
		return nil, errors.New("UserDataDir が空です")
	}
	if err := os.MkdirAll(opts.UserDataDir, 0o700); err != nil {
		return nil, fmt.Errorf("Chrome プロファイルディレクトリの作成に失敗しました: %w", err)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := []string{
		"--user-data-dir=" + opts.UserDataDir,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--no-first-run",
		"--no-default-browser-check",
	}
	if opts.Headless {
		args = append(args, "--headless=new")
	}
	args = append(args, "about:blank")

	var cmd *exec.Cmd
	background := false
	if bundle := macOSAppBundle(path); bundle != "" && !opts.Foreground && !opts.Headless {
		// `open -g` は LaunchServices に「前面に出さず起動」を頼める唯一の手段。stdio は
		// 引き継がれないが、こちらは DevTools ポートを自分で決めて /json/version を
		// ポーリングするので困らない。`-n` で新規インスタンス (既に開いている Chrome に
		// 引数を渡さない)、`-W` で Chrome の終了まで open が生き、exited がそのまま使える。
		openArgs := append([]string{"-g", "-n", "-W", "-a", bundle, "--args"}, args...)
		cmd = exec.CommandContext(ctx, "open", openArgs...) // #nosec G204 -- 引数は固定のフラグとパス
		background = true
	} else {
		cmd = exec.CommandContext(ctx, path, args...) // #nosec G204 -- path は固定候補から選んだ実行ファイル
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("Chrome の起動に失敗しました: %w", err)
	}
	b := &Browser{
		cmd:        cmd,
		background: background,
		port:       port,
		exited:     make(chan error, 1),
		pending:    map[int64]chan *cdproto.Message{},
		in:         make(chan *cdproto.Message),
		events:     make(chan *cdproto.Message),
	}
	go func() { b.exited <- cmd.Wait() }()
	slog.Debug("Chrome を起動しました", "pid", cmd.Process.Pid, "port", port, "headless", opts.Headless, "background", background)
	return b, nil
}

// macOSAppBundle は Chrome バイナリのパスから .app バンドルのパスを返す。
// macOS 以外、またはバンドル構成でない場合は空文字。
func macOSAppBundle(chromeBinary string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	bundle := filepath.Dir(filepath.Dir(filepath.Dir(chromeBinary)))
	if filepath.Ext(bundle) != ".app" {
		return ""
	}
	return bundle
}

// Connect は remote-debugging ポートの起動を待ち、ブラウザレベルの websocket に繋ぐ。
func (b *Browser) Connect(ctx context.Context) error {
	ver, err := b.version(ctx)
	if err != nil {
		return err
	}
	conn, err := chromedp.DialContext(ctx, ver.WebSocketDebuggerURL)
	if err != nil {
		return fmt.Errorf("CDP への接続に失敗しました: %w", err)
	}
	b.conn = conn
	go b.pump()
	go b.readLoop()
	slog.Debug("CDP に接続しました", "url", ver.WebSocketDebuggerURL)
	return nil
}

// Events は CDP イベントのストリーム。接続が閉じると close される。
func (b *Browser) Events() <-chan *cdproto.Message { return b.events }

type versionInfo struct {
	UserAgent            string `json:"User-Agent"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// version は /json/version をポーリングして取得する。Chrome の起動直後は listen していない。
func (b *Browser) version(ctx context.Context) (*versionInfo, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", b.port)
	var lastErr error
	for range 50 {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			var v versionInfo
			err = json.NewDecoder(resp.Body).Decode(&v)
			_ = resp.Body.Close()
			if err == nil && v.WebSocketDebuggerURL != "" {
				return &v, nil
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-b.exited:
			return nil, fmt.Errorf("Chrome が終了しました: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("Chrome の DevTools ポートに繋がりませんでした: %v", lastErr)
}

// UserAgent は Chrome が送る User-Agent を返す (Runtime を使わず /json/version から取る)。
func (b *Browser) UserAgent(ctx context.Context) (string, error) {
	v, err := b.version(ctx)
	if err != nil {
		return "", err
	}
	return v.UserAgent, nil
}

func (b *Browser) readLoop() {
	for {
		msg := new(cdproto.Message)
		if err := b.conn.Read(context.Background(), msg); err != nil {
			b.mu.Lock()
			for id, ch := range b.pending {
				close(ch)
				delete(b.pending, id)
			}
			b.mu.Unlock()
			close(b.in)
			return
		}
		if msg.ID != 0 {
			b.mu.Lock()
			ch, ok := b.pending[msg.ID]
			delete(b.pending, msg.ID)
			b.mu.Unlock()
			if ok {
				ch <- msg
			}
			continue
		}
		b.in <- msg
	}
}

// pump は readLoop と消費側の間で無制限のキューになる。消費側はイベント処理の途中で
// Network.getResponseBody のような往復コマンドを送るため、有限バッファだと
// 1 ページ分のイベントであっさり溢れて loadingFinished を取りこぼす。
func (b *Browser) pump() {
	var queue []*cdproto.Message
	for {
		var out chan *cdproto.Message
		var head *cdproto.Message
		if len(queue) > 0 {
			out, head = b.events, queue[0]
		}
		select {
		case m, ok := <-b.in:
			if !ok {
				for _, m := range queue {
					b.events <- m
				}
				close(b.events)
				return
			}
			queue = append(queue, m)
		case out <- head:
			queue = queue[1:]
		}
	}
}

// Call は CDP コマンドを送って結果を待つ。sessionID が空ならブラウザレベル。
func (b *Browser) Call(ctx context.Context, sessionID target.SessionID, method string, params, result any) error {
	if b.conn == nil {
		return errors.New("CDP に接続していません (Connect を先に呼ぶ)")
	}
	id := b.next.Add(1)
	msg := &cdproto.Message{ID: id, SessionID: sessionID, Method: cdproto.MethodType(method)}
	if params != nil {
		buf, err := json.Marshal(params)
		if err != nil {
			return err
		}
		msg.Params = buf
	}
	ch := make(chan *cdproto.Message, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	if err := b.conn.Write(ctx, msg); err != nil {
		return fmt.Errorf("%s の送信に失敗しました: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return fmt.Errorf("%s: CDP 接続が閉じられました", method)
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

// Close は Chrome を終了し、接続を閉じる。
// まず Browser.close で穏当に閉じる。Kill だけだとレンダラ等の子プロセスが
// プロファイルに書き終える前に親だけ死に、直後の RemoveAll が directory not empty で失敗する。
func (b *Browser) Close() {
	if b.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = b.Call(ctx, "", "Browser.close", nil, nil)
		select {
		case <-b.exited:
			cancel()
			_ = b.conn.Close()
			return
		case <-ctx.Done():
		}
		cancel()
		_ = b.conn.Close()
	}
	if b.background {
		// open 経由だと握っているのは open のプロセスで、Kill しても Chrome には届かない。
		// DevTools ポートはこの起動だけのものなので、それを手掛かりに Chrome 本体を落とす。
		_ = exec.Command("pkill", "-f", fmt.Sprintf("--remote-debugging-port=%d", b.port)).Run() // #nosec G204 -- port は自分で採番した整数
	}
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
	<-b.exited
}

// Page はタブ 1 枚 (flatten セッション)。
type Page struct {
	b         *Browser
	sessionID target.SessionID
}

// NewPage は about:blank のタブを開いて繋ぐ。Runtime は enable しない。
func (b *Browser) NewPage(ctx context.Context) (*Page, error) {
	var created target.CreateTargetReturns
	if err := b.Call(ctx, "", target.CommandCreateTarget, &target.CreateTargetParams{URL: "about:blank"}, &created); err != nil {
		return nil, err
	}
	var attached target.AttachToTargetReturns
	if err := b.Call(ctx, "", target.CommandAttachToTarget,
		&target.AttachToTargetParams{TargetID: created.TargetID, Flatten: true}, &attached); err != nil {
		return nil, err
	}
	return &Page{b: b, sessionID: attached.SessionID}, nil
}

// SessionID はこのタブのセッション ID。イベントの振り分けに使う。
func (p *Page) SessionID() target.SessionID { return p.sessionID }

// Call はこのタブのセッションで CDP コマンドを送る。
func (p *Page) Call(ctx context.Context, method string, params, result any) error {
	return p.b.Call(ctx, p.sessionID, method, params, result)
}

// SetCookies は Cookie をブラウザに注入する。
func (p *Page) SetCookies(ctx context.Context, cookies []*http.Cookie) error {
	params := make([]*network.CookieParam, 0, len(cookies))
	for _, c := range cookies {
		cp := &network.CookieParam{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HttpOnly,
		}
		if !c.Expires.IsZero() {
			t := cdp.TimeSinceEpoch(c.Expires)
			cp.Expires = &t
		}
		params = append(params, cp)
	}
	return p.Call(ctx, network.CommandSetCookies, &network.SetCookiesParams{Cookies: params}, nil)
}

// EnableNetwork は Network イベントの購読を始める。POST 本文はイベントに同梱させる。
func (p *Page) EnableNetwork(ctx context.Context) error {
	return p.Call(ctx, network.CommandEnable, &network.EnableParams{MaxPostDataSize: 1 << 20}, nil)
}

// Navigate は url へ遷移する。文書の応答が届くまで返らない。
func (p *Page) Navigate(ctx context.Context, url string) error {
	var nav page.NavigateReturns
	if err := p.Call(ctx, page.CommandNavigate, &page.NavigateParams{URL: url}, &nav); err != nil {
		return err
	}
	if nav.ErrorText != "" {
		return fmt.Errorf("%s への遷移に失敗しました: %s", url, nav.ErrorText)
	}
	return nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("空きポートの取得に失敗しました: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// findChrome はシステムの Chrome を探す (macOS / Linux)。
func findChrome() (string, error) {
	const macPath = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	if _, err := os.Stat(macPath); err == nil {
		return macPath, nil
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium-browser", "chromium"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("Google Chrome が見つかりませんでした")
}
