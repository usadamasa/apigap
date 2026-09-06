# apigap

ログイン済み Cookie でサイトのページを開き、そのページが自分で投げる通信を HAR に記録して、
リポジトリがまだ知らないエンドポイントを報告する CLI です。

対象サイトごとの違い (origin、Cookie の置き場、開くページ、既知の URL の在り処、識別子の畳み方)
はすべて設定ファイルとシナリオ YAML に置き、利用側のリポジトリにはコードを持ち込みません。

## 何を解くか

未公開 API を持つサイトを CLI から使うとき、「その画面はどのエンドポイントを叩いているか」を
DevTools で覗いて手で写すのは再現性がありません。apigap は次を自動化します。

1. **capture** は、Cookie を注入した Chrome でシナリオのページを順に開き、Network イベントを HAR 1.2 に落とします。
2. **gap** は、HAR に現れたエンドポイントをリポジトリの OpenAPI spec と Go ソースの URL リテラルに
   突き合わせ、`covered` / `uncovered` / `filtered` の 3 状態で報告します。

Chrome の操作には Playwright や chromedp の高レベル API を使わず、生の CDP を使います。
Cloudflare Turnstile 配下のサイトでは、`Runtime.enable` を打った時点でチャレンジが解けなくなるためです。
同じ理由で、シナリオにはページを開く `navigate` ステップしかありません。

## インストール

```bash
go install github.com/usadamasa/apigap@latest
```

システムの Google Chrome (macOS / Linux) が要ります。headful で窓が開きますが、macOS では
`open -g` で背面に起動するので作業中のアプリからフォーカスを奪いません。

## 使い方

利用側リポジトリに設定ファイルとシナリオを置きます。

```
capture/
  apigap.yaml
  endpoint-filter.txt
  scenarios/
    10-search.yaml
    20-detail.yaml
```

```bash
apigap capture -c capture/apigap.yaml          # → HAR を書く
apigap gap -c capture/apigap.yaml              # → Markdown の表
apigap gap -c capture/apigap.yaml --format json
apigap sanitize -c capture/apigap.yaml --har exported.har   # DevTools で取った HAR の秘匿値を潰す
```

### apigap.yaml

相対パスは設定ファイルのあるディレクトリ基準。`~` と `${VAR:-default}` を展開します。
JSON Schema を `schema/` に置いてあるので、先頭に `$schema` コメントを書くとエディタで補完と検証が効きます。

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/usadamasa/apigap/main/schema/apigap.schema.json
base_url: https://app.example.com
hosts: []                                    # base_url 以外に gap の対象にするホスト
cookies: ${XDG_CACHE_HOME:-~/.cache}/myapp/cookies.json
scenarios: scenarios
output: ../tmp/capture.har
headless: false                              # Turnstile 配下では true にしても通らない
foreground: false                            # 既定は macOS で `open -g` の背面起動。前面に出すなら true
settle_timeout_ms: 30000                     # チャレンジでない Document 応答を待つ上限

api_prefixes: []                             # 空: 同一ホストの document/xhr/fetch/other すべて
                                             # 指定: この prefix に前方一致するパスだけ
coverage:
  spec: internal/client/openapi.yaml         # 省略可。paths のテンプレートが covered の根拠
  code: [internal/client, cmd/myapp]         # Go ソースの URL リテラルを走査する

filter: endpoint-filter.txt                  # 1 行 1 prefix、# 以降が理由

cookie_keys:                                 # Cookie ファイルのキー名。省略した項目は既定値
  cookies: session.jar                       # Cookie 配列の位置。"." 区切りで入れ子をたどる
  user_agent: ua
  name: n
  value: v
  domain: host
  path: p
  expires: exp
  http_only: httpOnly
  secure: secure

normalize:
  trailing_slash: false                      # 末尾スラッシュ有無を同一視する (Django 系向け)
  rules:                                     # UUID・数値セグメントの既定に加える置換
    - pattern: '/docs/[A-Z]{2}-\d+'
      replace: '/docs/{docId}'
```

### Cookie ファイル

Cookie を書いた JSON。既定では
`{"cookies": [{"name", "value", "domain", "path", "expires"?, "httpOnly", "secure"}], "user_agent"?}`
の形を読みますが、ログイン用の CLI が書く形はツールごとに違うので、キー名は `cookie_keys` で
対応づけます。apigap 側は特定の構造を前提にしません。

`expires` は RFC3339 の文字列と epoch 秒の数値のどちらでも構いません。0 以下と欠落は
セッション Cookie とみなし、期限切れは読み飛ばします。`user_agent` があれば起動した Chrome の
UA と比べ、違えば警告します (Cloudflare の `cf_clearance` は発行時の UA に紐づくため)。

配列がルート直下にある形 (`[{...}]`) は読めません。必要になったら対応を足します。

### シナリオ

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/usadamasa/apigap/main/schema/scenario.schema.json
name: detail
steps:
  - navigate: /items/42                     # / 始まりは base_url に対する相対パス
    wait_ms: 10000                          # 落ち着いてから留まる時間 (既定 3000)
```

ファイル名順に実行します。`navigate` 後は、チャレンジでも 3xx でもない Document 応答が
来るまで待ち (`settle_timeout_ms` が上限)、その後 `wait_ms` だけ留まって遅延ロードの通信を拾います。

## HAR について

- HAR 1.2。各エントリに Chrome DevTools 互換の `_resourceType` (小文字) を付けます。
- `Cookie` / `Set-Cookie` / `Authorization` ヘッダの値は組み立て時に `***` にし、
  `cookies` 配列は空にします。`*ExtraInfo` 系イベントは記録しません。
- 本文は document / xhr / fetch / other のうちテキスト系 (4 MB 以下) だけ持ちます。
  script / stylesheet / 画像 / PDF はサイズだけです。
- ファイルは 0600 で書きます。

## gap の 3 状態

| 状態 | 意味 |
| --- | --- |
| `covered` | spec の paths か、ソースの URL リテラルに一致する |
| `uncovered` | 観測されたがリポジトリの誰も知らない |
| `filtered` | filter ファイルが除外している。理由付きで表示する |

filter は「レポートを読みやすく保つ」ための判断で、「機能として価値がない」の判断ではありません。
だから隠さず出します。
