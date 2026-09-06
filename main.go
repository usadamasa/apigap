// Command apigap はログイン済み Cookie でサイトのページを開いて通信を HAR に記録し、
// リポジトリがまだ知らないエンドポイントを報告する。
//
// 対象サイトごとの違いはすべて apigap.yaml とシナリオ YAML に置き、コードは持ち込まない。
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "capture":
		err = runCapture(os.Args[2:])
	case "gap":
		err = runGap(os.Args[2:])
	case "sanitize":
		err = runSanitize(os.Args[2:])
	case "probe":
		err = runProbe(os.Args[2:])
	case "-h", "--help", "help":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `Usage: apigap <command> [options]

Commands:
  capture   Open every scenario page with the saved cookies and write a HAR
  gap       Report observed endpoints the repository does not know yet
  sanitize  Mask credentials in a HAR recorded elsewhere (e.g. DevTools export)
  probe     Fetch URLs with the saved cookies from a plain HTTP client, no browser

Every command takes -c <apigap.yaml> (default: ./apigap.yaml).
`)
}
