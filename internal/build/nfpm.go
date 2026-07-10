package build

// nfpm.go — BYO-binary(依頼① #3)の deb/rpm ネイティブ生成。持ち込み linux バイナリから
// nfpm をライブラリとして呼んで deb/rpm を作る(D-2: 利用者に nfpm CLI を要求しない)。
// パッケージングは決定的なファイル生成でありコンパイルではないので、ADR-5(compiler は
// subprocess pin)の射程外としてライブラリ import を選ぶ。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goreleaser/nfpm/v2"
	"github.com/goreleaser/nfpm/v2/files"

	_ "github.com/goreleaser/nfpm/v2/deb" // deb packager を登録
	_ "github.com/goreleaser/nfpm/v2/rpm" // rpm packager を登録
)

// PackageSpec は 1 チャネル(apt→deb / rpm→rpm)のパッケージ生成指定。依存 3 区分は
// フォーマット固有(ディストロで名前が異なるため呼び出し側で解決して渡す)。
type PackageSpec struct {
	Format      string // "deb" | "rpm"
	Ext         string // ".deb" | ".rpm"(生成物名に使う)
	Name        string // パッケージ名(= project)
	BinaryName  string // /usr/bin 配下の実行ファイル名
	Version     string
	Maintainer  string
	Description string
	Homepage    string
	License     string
	Depends     []string
	Recommends  []string
	Suggests    []string
	// Notice は配布者が書いた告知(D-3)。deb/rpm には注記専用の欄が無いので description に続ける。
	// post-install scriptlet は失敗するとインストールを壊すので使わない。
	Notice string
}

// describe は description に告知を続ける。告知が無ければ description のまま。
func (s PackageSpec) describe() string {
	notice := strings.TrimSpace(s.Notice)
	if notice == "" {
		return s.Description
	}
	if s.Description == "" {
		return notice
	}
	return s.Description + "\n\n" + notice
}

// PackagePrebuilt は各 linux バイナリ(arch ごと)から deb/rpm を作り distDir に置く。
// 生成物名は <project>_<version>_linux_<arch><ext>(expectedPackages と一致・publish が上げる)。
// 実ファイルの sha256 を付けた成果物を返す。windows/darwin バイナリは対象外(linux のみ)。
func PackagePrebuilt(root, distDir string, spec PackageSpec, bins []PrebuiltBinary) ([]Artifact, error) {
	pkgr, err := nfpm.Get(spec.Format)
	if err != nil {
		return nil, &FailedError{Err: fmt.Errorf("nfpm %s: %w", spec.Format, err)}
	}
	outDir := filepath.Join(root, distDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, &FailedError{Err: fmt.Errorf("mkdir %s: %w", distDir, err)}
	}

	var out []Artifact
	for _, bin := range bins {
		if bin.OS != "linux" {
			continue // deb/rpm は linux のみ
		}
		src := bin.Path
		if !filepath.IsAbs(src) {
			src = filepath.Join(root, bin.Path)
		}
		if _, err := os.Stat(src); err != nil {
			return nil, &FailedError{Err: fmt.Errorf("prebuilt binary %s: %w", bin.Path, err)}
		}

		info := nfpm.WithDefaults(&nfpm.Info{
			Name:        spec.Name,
			Arch:        bin.Arch, // nfpm が deb=amd64/arm64・rpm=x86_64/aarch64 に翻訳
			Platform:    "linux",
			Version:     spec.Version,
			Maintainer:  spec.Maintainer,
			Description: spec.describe(),
			Homepage:    spec.Homepage,
			License:     spec.License,
			Overridables: nfpm.Overridables{
				Depends:    spec.Depends,
				Recommends: spec.Recommends,
				Suggests:   spec.Suggests,
				Contents: files.Contents{
					{
						Source:      src,
						Destination: "/usr/bin/" + spec.BinaryName,
						FileInfo:    &files.ContentFileInfo{Mode: 0o755},
					},
				},
			},
		})

		name := fmt.Sprintf("%s_%s_linux_%s%s", spec.Name, spec.Version, bin.Arch, spec.Ext)
		target := filepath.Join(outDir, name)
		f, err := os.Create(target)
		if err != nil {
			return nil, &FailedError{Err: fmt.Errorf("create %s: %w", name, err)}
		}
		if err := pkgr.Package(info, f); err != nil {
			f.Close()
			return nil, &FailedError{Err: fmt.Errorf("nfpm package %s: %w", name, err)}
		}
		if err := f.Close(); err != nil {
			return nil, &FailedError{Err: fmt.Errorf("close %s: %w", name, err)}
		}
		sum, err := sha256File(target)
		if err != nil {
			return nil, &FailedError{Err: fmt.Errorf("checksum %s: %w", name, err)}
		}
		out = append(out, Artifact{OS: "linux", Arch: bin.Arch, Path: filepath.Join(distDir, name), SHA256: sum})
	}
	return out, nil
}
