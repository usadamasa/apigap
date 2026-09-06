package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
)

// HAR 1.2 のうち、観測とスペック生成に要る部分。`_` 始まりのフィールドは
// HAR 仕様が許すカスタムフィールドで、Chrome DevTools のエクスポートと同じ名前にしてある。
type HAR struct {
	Log Log `json:"log"`
}

type Log struct {
	Version string  `json:"version"`
	Creator Creator `json:"creator"`
	Pages   []Page  `json:"pages"`
	Entries []Entry `json:"entries"`
}

type Creator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Page はシナリオの navigate ステップ 1 つ。
type Page struct {
	StartedDateTime time.Time `json:"startedDateTime"`
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	PageTimings     struct{}  `json:"pageTimings"`
}

type Entry struct {
	Pageref         string    `json:"pageref,omitempty"`
	StartedDateTime time.Time `json:"startedDateTime"`
	Time            float64   `json:"time"`
	Request         Request   `json:"request"`
	Response        Response  `json:"response"`
	Cache           struct{}  `json:"cache"`
	Timings         Timings   `json:"timings"`
	// ResourceType は Chrome の分類 (document / xhr / fetch / script ...) を小文字で持つ。
	ResourceType string `json:"_resourceType,omitempty"`
	// Error は loadingFailed の errorText。応答が無いエントリの理由を残す。
	Error string `json:"_error,omitempty"`
}

type NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Request struct {
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	HTTPVersion string      `json:"httpVersion"`
	Cookies     []NameValue `json:"cookies"`
	Headers     []NameValue `json:"headers"`
	QueryString []NameValue `json:"queryString"`
	PostData    *PostData   `json:"postData,omitempty"`
	HeadersSize int         `json:"headersSize"`
	BodySize    int         `json:"bodySize"`
}

type PostData struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

type Response struct {
	Status      int         `json:"status"`
	StatusText  string      `json:"statusText"`
	HTTPVersion string      `json:"httpVersion"`
	Cookies     []NameValue `json:"cookies"`
	Headers     []NameValue `json:"headers"`
	Content     Content     `json:"content"`
	RedirectURL string      `json:"redirectURL"`
	HeadersSize int         `json:"headersSize"`
	BodySize    int         `json:"bodySize"`
}

type Content struct {
	Size     int    `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	Encoding string `json:"encoding,omitempty"`
}

type Timings struct {
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

// sensitiveHeaders は値を記録しないヘッダ。Cookie 値と Authorization は
// ファイルに一度でも書いたら漏洩事故と同じなので、書き出し時ではなく HAR を組む時点で潰す。
var sensitiveHeaders = map[string]bool{
	"cookie":        true,
	"set-cookie":    true,
	"authorization": true,
}

const masked = "***"

// maskHeaders は CDP のヘッダ map を、秘匿値を潰した名前順のリストにする。
func maskHeaders(h network.Headers) []NameValue {
	out := make([]NameValue, 0, len(h))
	for k, v := range h {
		s := fmt.Sprint(v)
		if sensitiveHeaders[strings.ToLower(k)] {
			s = masked
		}
		out = append(out, NameValue{Name: k, Value: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func queryString(raw string) []NameValue {
	u, err := url.Parse(raw)
	if err != nil {
		return []NameValue{}
	}
	out := []NameValue{}
	for k, vs := range u.Query() {
		for _, v := range vs {
			out = append(out, NameValue{Name: k, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// newHAR は空の HAR を作る。
func newHAR() *HAR {
	h := &HAR{}
	h.Log.Version = "1.2"
	h.Log.Creator = Creator{Name: "apigap", Version: version}
	h.Log.Pages = []Page{}
	h.Log.Entries = []Entry{}
	return h
}

// writeHAR は path に HAR を書く。0600 なのは、秘匿値は潰してあっても
// ログイン済みセッションの観測結果である以上、他人に読ませる理由がないため。
func writeHAR(path string, h *HAR) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

const version = "0.1.0"
