package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
)

// event は CDP イベントを組み立てる。cdproto の型を素直に Marshal すると、未指定の
// 列挙が "" で、timestamp が null で出てしまい Unmarshal が拒む。実機の Chrome は
// どちらも必ず埋めて送るので、ここでは空の値を落として同じ形にする。
func event(t *testing.T, method cdproto.MethodType, params any) *cdproto.Message {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	dropEmpty(m)
	if m["timestamp"] == nil {
		m["timestamp"] = 1.5
	}
	raw, _ = json.Marshal(m)
	return &cdproto.Message{Method: method, Params: raw}
}

func dropEmpty(m map[string]any) {
	for k, v := range m {
		switch x := v.(type) {
		case nil:
			delete(m, k)
		case string:
			if x == "" {
				delete(m, k)
			}
		case map[string]any:
			dropEmpty(x)
		}
	}
}

func wall(t time.Time) *cdp.TimeSinceEpoch {
	w := cdp.TimeSinceEpoch(t)
	return &w
}

func TestRecorder_MasksCredentialsAndSplitsRedirects(t *testing.T) {
	rec := newRecorder(map[string]bool{"app.example.com": true})
	rec.startPage("search/0", "https://app.example.com/search?q=x")
	now := time.Now()

	// 1. 検索ページ: 302 → 200 のリダイレクト連鎖 (requestId は同じ)
	rec.handle(event(t, cdproto.EventNetworkRequestWillBeSent, network.EventRequestWillBeSent{
		RequestID: "1", Type: network.ResourceTypeDocument, WallTime: wall(now),
		Request: &network.Request{Method: "GET", URL: "https://app.example.com/search?q=x",
			Headers: network.Headers{"Cookie": "cf_clearance=SECRET-COOKIE", "Accept": "text/html"}},
	}))
	rec.handle(event(t, cdproto.EventNetworkRequestWillBeSent, network.EventRequestWillBeSent{
		RequestID: "1", Type: network.ResourceTypeDocument, WallTime: wall(now.Add(50 * time.Millisecond)),
		Request:          &network.Request{Method: "GET", URL: "https://app.example.com/search?q=x&page=1", Headers: network.Headers{}},
		RedirectResponse: &network.Response{URL: "https://app.example.com/search?q=x", Status: 302, Headers: network.Headers{"Location": "/search?q=x&page=1", "Set-Cookie": "JSESSIONID=SECRET-SESSION"}},
	}))
	doc := rec.handle(event(t, cdproto.EventNetworkResponseReceived, network.EventResponseReceived{
		RequestID: "1", Type: network.ResourceTypeDocument,
		Response: &network.Response{URL: "https://app.example.com/search?q=x&page=1", Status: 200, MimeType: "text/html",
			Headers: network.Headers{"Content-Type": "text/html", "Set-Cookie": "AWSALB=SECRET-ALB"}},
	}))
	if doc == nil || !doc.settled() {
		t.Fatalf("200 の Document は settled のはず: %+v", doc)
	}
	rec.handle(event(t, cdproto.EventNetworkLoadingFinished, network.EventLoadingFinished{RequestID: "1", EncodedDataLength: 1234}))

	// 2. 認可ヘッダ付き XHR (POST 本文あり)
	rec.handle(event(t, cdproto.EventNetworkRequestWillBeSent, network.EventRequestWillBeSent{
		RequestID: "2", Type: network.ResourceTypeXHR, WallTime: wall(now),
		Request: &network.Request{Method: "POST", URL: "https://app.example.com/api/track", HasPostData: true,
			PostDataEntries: []*network.PostDataEntry{{Bytes: base64.StdEncoding.EncodeToString([]byte(`{"q":1}`))}},
			Headers:         network.Headers{"Authorization": "Bearer SECRET-TOKEN", "Content-Type": "application/json"}},
	}))
	rec.handle(event(t, cdproto.EventNetworkLoadingFailed, network.EventLoadingFailed{RequestID: "2", ErrorText: "net::ERR_ABORTED"}))

	// 3. チャレンジ応答は settled にならない
	rec.handle(event(t, cdproto.EventNetworkRequestWillBeSent, network.EventRequestWillBeSent{
		RequestID: "3", Type: network.ResourceTypeDocument, WallTime: wall(now),
		Request: &network.Request{Method: "GET", URL: "https://app.example.com/items/42", Headers: network.Headers{}},
	}))
	doc = rec.handle(event(t, cdproto.EventNetworkResponseReceived, network.EventResponseReceived{
		RequestID: "3", Type: network.ResourceTypeDocument,
		Response: &network.Response{URL: "https://app.example.com/items/42", Status: 403, Headers: network.Headers{"cf-mitigated": "challenge"}},
	}))
	if doc == nil || doc.settled() {
		t.Fatalf("cf-mitigated: challenge の 403 は settled ではない: %+v", doc)
	}

	rec.finish()
	har := rec.har
	if len(har.Log.Entries) != 4 {
		t.Fatalf("entries = %d, want 4 (302, 200, failed XHR, open challenge)", len(har.Log.Entries))
	}
	e0, e1, e2 := har.Log.Entries[0], har.Log.Entries[1], har.Log.Entries[2]
	if e0.Response.Status != 302 || e0.Response.RedirectURL == "" {
		t.Errorf("リダイレクト元が独立したエントリになっていない: %+v", e0.Response)
	}
	if e1.Response.Status != 200 || e1.ResourceType != "document" || e1.Pageref != "search/0" {
		t.Errorf("最終応答: %+v", e1)
	}
	if e2.Error != "net::ERR_ABORTED" || e2.Request.PostData == nil || e2.Request.PostData.Text != `{"q":1}` || e2.Request.PostData.MimeType != "application/json" {
		t.Errorf("失敗した XHR: %+v", e2)
	}

	out, err := json.Marshal(har)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET-COOKIE", "SECRET-SESSION", "SECRET-ALB", "SECRET-TOKEN"} {
		if strings.Contains(string(out), secret) {
			t.Errorf("HAR に秘匿値 %s が残っている", secret)
		}
	}
	if !strings.Contains(string(out), `"name":"Cookie","value":"***"`) {
		t.Errorf("Cookie ヘッダは名前を残して値だけ潰す: %s", out)
	}
}

func TestWantBody(t *testing.T) {
	mk := func(rt, mime string, status, size int) *pending {
		p := &pending{size: size}
		p.entry.ResourceType = rt
		p.entry.Response.Content.MimeType = mime
		p.entry.Response.Status = status
		return p
	}
	cases := []struct {
		name string
		p    *pending
		want bool
	}{
		{"html document", mk("document", "text/html", 200, 500_000), true},
		{"json xhr", mk("xhr", "application/json", 200, 100), true},
		{"script bundle", mk("script", "application/javascript", 200, 100), false},
		{"pdf", mk("document", "application/pdf", 200, 100), false},
		{"redirect", mk("document", "text/html", 302, 0), false},
		{"too large", mk("document", "text/html", 200, maxBodyBytes+1), false},
	}
	for _, c := range cases {
		if got := wantBody(c.p); got != c.want {
			t.Errorf("%s: wantBody = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestResolveURL(t *testing.T) {
	if got := resolveURL("https://app.example.com", "/items/42"); got != "https://app.example.com/items/42" {
		t.Errorf("got %q", got)
	}
	if got := resolveURL("https://app.example.com/", "https://cdn.example.org/x"); got != "https://cdn.example.org/x" {
		t.Errorf("絶対 URL はそのまま: %q", got)
	}
}
