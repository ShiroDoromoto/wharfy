// Command wharfy — 1 つのバイナリをあらゆるチャネルへ配る道具(設計 01)。
// main は薄く、cobra ツリーの組み立てと実行だけを行う。
package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// errNotOK は「envelope を Emit 済みで ok=false」の合図。中身は既に出しているので
		// 追加メッセージは出さず、終了コードだけ非ゼロにする。それ以外は真の異常なので表示する。
		if !errors.Is(err, errNotOK) {
			fmt.Fprintln(os.Stderr, "wharfy:", err)
		}
		os.Exit(1)
	}
}
