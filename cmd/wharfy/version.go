// version.go — tag を単一ソースにする version 注入(samples/version.go を移植)。
//
// 値は手書きしない。リリース時に ldflags で注入する:
//
//	-X main.version={{.Version}} -X main.commit={{.Commit}} -X main.date={{.Date}}
//
// これで「コードに書いたバージョン」と「実際に配ったタグ」がズレない。
package main

import (
	"fmt"
	"runtime/debug"
)

// ldflags で上書きされる。未注入(go run / go install 直)のときの既定値。
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// resolveVersion は ldflags 注入が無いとき(go install など)に
// ビルド情報へフォールバックする。エージェントが版を確実に読めるようにする。
func resolveVersion() (v, c, d string) {
	v, c, d = version, commit, date
	if v != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				c = s.Value
			case "vcs.time":
				d = s.Value
			}
		}
	}
	return
}

func versionLine() string {
	v, c, d := resolveVersion()
	if c == "" {
		return v
	}
	short := c
	if len(short) > 7 {
		short = short[:7]
	}
	if d == "" {
		return fmt.Sprintf("%s (%s)", v, short)
	}
	return fmt.Sprintf("%s (%s, %s)", v, short, d)
}
