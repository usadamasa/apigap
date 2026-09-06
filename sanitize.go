package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// 伏せるヘッダーの一覧 sensitiveHeaders と伏せ字 masked は har.go と共有する。
// capture は記録する時点で潰し、sanitize は他所で記録された HAR を後から潰すが、
// 何を秘匿とみなすかの定義が 2 つあると片方だけ漏れる。

func runSanitize(args []string) error {
	fs := flag.NewFlagSet("sanitize", flag.ContinueOnError)
	configPath := fs.String("c", "./apigap.yaml", "設定ファイル")
	harPath := fs.String("har", "", "HAR ファイル (既定: 設定の output)")
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
	return sanitizeFile(cfg, *harPath)
}

// sanitizeFile は HAR から秘匿値を落とし、API でないエントリを捨てて同じパスへ書き戻す。
//
// 対象の判定は gap と同じ apiPath に任せる。ここで独自の条件を持つと、
// sanitize が捨てたものを gap が「観測されなかった」と読み違える。
//
// filter ファイルの prefix はここでは落とさない。それらは gap が filtered として
// 報告する対象であり、HAR から消すと報告そのものができなくなる。
func sanitizeFile(cfg *Config, harPath string) error {
	data, err := os.ReadFile(harPath) // #nosec G304 -- 利用者が指定した HAR
	if err != nil {
		return fmt.Errorf("HAR の読み込みに失敗しました: %w", err)
	}

	// 既知のフィールドだけの構造体ではなく map で読むのは、HAR の残りを
	// そのまま書き戻すため。型を付けた分だけ情報が落ちる。
	var har map[string]any
	if err := json.Unmarshal(data, &har); err != nil {
		return fmt.Errorf("HAR の解析に失敗しました: %w", err)
	}

	log, _ := har["log"].(map[string]any)
	if log == nil {
		return fmt.Errorf("HAR に log がありません: %s", harPath)
	}
	entries, _ := log["entries"].([]any)

	hosts := cfg.HostSet()
	kept := make([]any, 0, len(entries))
	excluded := 0
	for _, entry := range entries {
		e, _ := entry.(map[string]any)
		req, _ := e["request"].(map[string]any)
		rawURL, _ := req["url"].(string)
		resourceType, _ := e["_resourceType"].(string)

		if _, ok := apiPath(cfg, hosts, rawURL, resourceType); !ok {
			excluded++
			continue
		}
		kept = append(kept, entry)
	}

	sanitized := 0
	for _, entry := range kept {
		e, _ := entry.(map[string]any)
		req, _ := e["request"].(map[string]any)
		resp, _ := e["response"].(map[string]any)

		sanitized += sanitizeHeaders(req)
		sanitized += sanitizeHeaders(resp)
		sanitized += sanitizeCookies(req)
		sanitized += sanitizeCookies(resp)
	}

	log["entries"] = kept
	out, err := json.MarshalIndent(har, "", "  ")
	if err != nil {
		return fmt.Errorf("HAR の書き出しに失敗しました: %w", err)
	}
	if err := os.WriteFile(harPath, out, 0o600); err != nil {
		return fmt.Errorf("HAR の書き込みに失敗しました: %w", err)
	}

	fmt.Fprintf(os.Stderr, "対象外 %d 件を除外し、残る %d 件のうち %d 個の秘匿値を伏せました\n",
		excluded, len(kept), sanitized)
	return nil
}

func sanitizeHeaders(obj map[string]any) int {
	headers, _ := obj["headers"].([]any)
	count := 0
	for _, h := range headers {
		hdr, _ := h.(map[string]any)
		name, _ := hdr["name"].(string)
		if sensitiveHeaders[strings.ToLower(name)] {
			hdr["value"] = masked
			count++
		}
	}
	return count
}

// sanitizeCookies は cookies 配列の値をすべて伏せる。名前で選別しないのは、
// どの Cookie がセッションを持つかがサイトごとに違い、判別を誤ると漏れるため。
func sanitizeCookies(obj map[string]any) int {
	cookies, _ := obj["cookies"].([]any)
	count := 0
	for _, c := range cookies {
		cookie, _ := c.(map[string]any)
		cookie["value"] = masked
		count++
	}
	return count
}
