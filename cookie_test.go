package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCookieFile(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.json")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 既定のキーは従来の形 ({"user_agent":..., "cookies":[{name,value,...}]}) を読む。
func TestLoadCookies_DefaultKeys(t *testing.T) {
	path := writeCookieFile(t, `{"user_agent":"Foo/1","cookies":[
	  {"name":"sid","value":"abc","domain":"app.example.com","path":"/","expires":"2099-01-01T00:00:00Z","httpOnly":true,"secure":true},
	  {"name":"cleared","value":""},
	  {"name":"old","value":"x","expires":"2000-01-01T00:00:00Z"}
	]}`)
	cookies, ua, err := loadCookies(path, defaultCookieKeys())
	if err != nil {
		t.Fatalf("loadCookies: %v", err)
	}
	if ua != "Foo/1" {
		t.Errorf("user_agent = %q", ua)
	}
	if len(cookies) != 2 || cookies[0].Name != "sid" || cookies[1].Name != "cleared" {
		t.Fatalf("期限切れだけ落ち、値が空のものは残るはず: %+v", cookies)
	}
	c := cookies[0]
	if c.Value != "abc" || c.Domain != "app.example.com" || c.Path != "/" || !c.HttpOnly || !c.Secure {
		t.Errorf("cookie = %+v", c)
	}
}

// キー名を設定で差し替えれば、入れ子・別名・秒数の expires でも読める。
func TestLoadCookies_MappedKeys(t *testing.T) {
	path := writeCookieFile(t, `{"session":{"jar":[
	  {"n":"sid","v":"abc","host":"app.example.com","p":"/","exp":4102444800,"sec":true},
	  {"n":"old","v":"x","exp":1000000000},
	  {"n":"forever","v":"y","exp":0}
	]},"ua":"Foo/1"}`)
	keys := CookieKeys{
		Cookies: "session.jar", UserAgent: "ua",
		Name: "n", Value: "v", Domain: "host", Path: "p", Expires: "exp", HTTPOnly: "http", Secure: "sec",
	}
	cookies, ua, err := loadCookies(path, keys)
	if err != nil {
		t.Fatalf("loadCookies: %v", err)
	}
	if ua != "Foo/1" {
		t.Errorf("user_agent = %q", ua)
	}
	if len(cookies) != 2 {
		t.Fatalf("期限切れの 1 件だけ落ちるはず: %+v", cookies)
	}
	if got := cookies[0]; got.Name != "sid" || got.Value != "abc" || got.Domain != "app.example.com" || !got.Secure {
		t.Errorf("cookie[0] = %+v", got)
	}
	if want := time.Unix(4102444800, 0); !cookies[0].Expires.Equal(want) {
		t.Errorf("expires = %v, want %v", cookies[0].Expires, want)
	}
	if !cookies[1].Expires.IsZero() {
		t.Errorf("exp <= 0 はセッション Cookie 扱い: %v", cookies[1].Expires)
	}
}

// キーの対応づけが違うときは、どの設定を直せばいいか分かるエラーにする。
func TestLoadCookies_ReportsWrongKeys(t *testing.T) {
	cases := []struct {
		name string
		src  string
		keys CookieKeys
		want string
	}{
		{
			name: "配列が見つからない",
			src:  `{"jar":[{"name":"sid","value":"abc"}]}`,
			keys: defaultCookieKeys(),
			want: "cookie_keys.cookies",
		},
		{
			name: "name / value が取れない",
			src:  `{"cookies":[{"n":"sid","v":"abc"}]}`,
			keys: defaultCookieKeys(),
			want: "cookie_keys.name",
		},
	}
	for _, c := range cases {
		_, _, err := loadCookies(writeCookieFile(t, c.src), c.keys)
		if err == nil {
			t.Fatalf("%s: エラーになるはず", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v に %q が要る", c.name, err, c.want)
		}
	}
}
