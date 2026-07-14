package build

// bundle.go — BYO-bundle(GUI・依頼①)のネイティブ経路。prebuilt(単一バイナリ持ち込み)の対。
// バンドル(.dmg/.zip 等)は既にビルド済み・署名済みの最終成果物なので、wharfy は再 archive せず、
// 存在と sha256 を検証してそのまま Release アセットにする(relay に徹する)。生成も再署名もしない。

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Bundle は解決済みの持ち込みバンドル 1 つ。config.Bundle の写し
// (build は下位ドメインで config を import しないため値だけ受け取る)。
type Bundle struct {
	OS     string
	Arch   string
	Kind   string // dmg/zip/exe/msi/appimage/deb/rpm
	Path   string // 利用者 root からの相対パス
	SHA256 string // 宣言 sha(任意)。空でなければ実 sha と照合する
}

// ValidateBundles は持ち込みバンドルの存在と(宣言があれば)sha256 を検証し、Artifact にする。
// Path はそのまま残す(再 archive しない)。Kind を Artifact.Kind に載せて後段(cask/release)へ運ぶ。
// prebuilt の PrebuiltBuilder.Build と対称だが、archive 化はしない点だけが違う。
func ValidateBundles(root string, bundles []Bundle) ([]Artifact, error) {
	if len(bundles) == 0 {
		return nil, &FailedError{Err: fmt.Errorf("bundle mode: no bundles declared")}
	}
	out := make([]Artifact, 0, len(bundles))
	for _, bd := range bundles {
		full := bd.Path
		if !filepath.IsAbs(full) {
			full = filepath.Join(root, bd.Path)
		}
		sum, err := SHA256File(full)
		if err != nil {
			return nil, &FailedError{Err: fmt.Errorf("bundle %s: %w", bd.Path, err)}
		}
		if bd.SHA256 != "" && !strings.EqualFold(bd.SHA256, sum) {
			return nil, &FailedError{Err: fmt.Errorf("bundle %s sha256 mismatch: declared %s but file is %s", bd.Path, bd.SHA256, sum)}
		}
		out = append(out, Artifact{OS: bd.OS, Arch: bd.Arch, Kind: bd.Kind, Path: bd.Path, SHA256: sum})
	}
	return out, nil
}
