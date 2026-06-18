// Command wharfy — 1 つのバイナリをあらゆるチャネルへ配る道具(設計 01)。
// main は薄く、cobra ツリーの組み立てと実行だけを行う。
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "wharfy:", err)
		os.Exit(1)
	}
}
