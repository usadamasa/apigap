package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 架空の値。テストが漏れを検出するための目印。
const (
	fakeSessionCookie = "SESSIONID-DO-NOT-LEAK-0001"
	fakeBearerToken   = "Bearer TOKEN-DO-NOT-LEAK-0002"
	fakeCookieValue   = "COOKIEJAR-DO-NOT-LEAK-0003"
	fakeSetCookie     = "sid=SETCOOKIE-DO-NOT-LEAK-0004; Path=/"
)

func writeTempHAR(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capture.har")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("書き込みに失敗: %v", err)
	}
	return path
}

func TestSanitizeFile(t *testing.T) {
	har := `{"log":{"version":"1.2","entries":[
	  {"_resourceType":"document",
	   "request":{"method":"GET","url":"https://example.com/search?q=x",
	     "headers":[{"name":"Cookie","value":"` + fakeSessionCookie + `"},
	                {"name":"Authorization","value":"` + fakeBearerToken + `"},
	                {"name":"Accept","value":"text/html"}],
	     "cookies":[{"name":"sid","value":"` + fakeCookieValue + `"}]},
	   "response":{"status":200,
	     "headers":[{"name":"Set-Cookie","value":"` + fakeSetCookie + `"}],
	     "cookies":[]}},
	  {"_resourceType":"script",
	   "request":{"method":"GET","url":"https://example.com/static/app.js","headers":[],"cookies":[]},
	   "response":{"status":200,"headers":[],"cookies":[]}},
	  {"_resourceType":"xhr",
	   "request":{"method":"POST","url":"https://example.org/collect","headers":[],"cookies":[]},
	   "response":{"status":204,"headers":[],"cookies":[]}}
	]}}`

	path := writeTempHAR(t, har)
	if err := sanitizeFile(testConfig(), path); err != nil {
		t.Fatalf("sanitizeFile: %v", err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("読み込みに失敗: %v", err)
	}
	got := string(out)

	for _, secret := range []string{fakeSessionCookie, fakeBearerToken, fakeCookieValue, fakeSetCookie} {
		if strings.Contains(got, secret) {
			t.Errorf("秘匿値が残っている: %q", secret)
		}
	}
	// 値を伏せるだけで、ヘッダー自体は残す。何が送られていたかは spec の材料になる。
	if !strings.Contains(got, `"Authorization"`) {
		t.Error("Authorization ヘッダーごと消えている")
	}
	if !strings.Contains(got, `"text/html"`) {
		t.Error("秘匿対象でないヘッダーの値まで伏せている")
	}

	var doc struct {
		Log struct {
			Entries []struct {
				Request struct {
					URL string `json:"url"`
				} `json:"request"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("書き戻した HAR が壊れている: %v", err)
	}
	if len(doc.Log.Entries) != 1 {
		t.Fatalf("残ったエントリ数 = %d, want 1: %+v", len(doc.Log.Entries), doc.Log.Entries)
	}
	if !strings.HasPrefix(doc.Log.Entries[0].Request.URL, "https://example.com/search") {
		t.Errorf("残ったエントリ = %q", doc.Log.Entries[0].Request.URL)
	}
}

func TestSanitizeFile_KeepsFilteredPrefixes(t *testing.T) {
	// filter ファイルの prefix は gap が filtered として報告する対象なので、
	// sanitize で消してはいけない。消すと gap から見えなくなる。
	har := `{"log":{"entries":[
	  {"_resourceType":"document",
	   "request":{"method":"GET","url":"https://example.com/internal/login","headers":[],"cookies":[]},
	   "response":{"status":200,"headers":[],"cookies":[]}}
	]}}`

	cfg := testConfig()
	cfg.Filter = "/dev/null/never-read"

	path := writeTempHAR(t, har)
	if err := sanitizeFile(cfg, path); err != nil {
		t.Fatalf("sanitizeFile: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("読み込みに失敗: %v", err)
	}
	if !strings.Contains(string(out), "/internal/login") {
		t.Error("filter 対象の prefix が sanitize で落とされている")
	}
}
