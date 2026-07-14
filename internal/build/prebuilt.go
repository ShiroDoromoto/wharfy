package build

// prebuilt.go — BYO-binary(依頼①)のネイティブ経路。GoReleaser を使わず、利用者が
// 持ち込んだビルド済みバイナリを検証・archive 化する(prebuilt builder は GoReleaser Pro
// 専用のため、OSS の wharfy は自前で行う)。archive 命名は GoReleaser 既定と一致させ、
// publish 側(formula/scoop/install.sh)を無改修で通す。

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// PrebuiltBinary は解決済みのビルド済みバイナリ 1 つ。config.PrebuiltBinary の写し
// (build は下位ドメインで config を import しないため、値だけ受け取る)。
type PrebuiltBinary struct {
	OS     string
	Arch   string
	Path   string // 利用者 root からの相対パス
	SHA256 string // 宣言 sha(任意)。空でなければ実 sha と照合する
}

// PrebuiltBuilder は BYO-binary の Builder 実装(依頼①)。configPath は使わない
// (GoReleaser 設定は不要)。渡されたバイナリの存在と sha256 を検証し、Artifact にする。
type PrebuiltBuilder struct {
	Binaries []PrebuiltBinary
}

// Build はビルドせず、持ち込みバイナリを検証して Binary 成果物にする(存在＋sha256)。
func (b *PrebuiltBuilder) Build(_ context.Context, root, _ string) ([]Artifact, error) {
	if len(b.Binaries) == 0 {
		return nil, &FailedError{Err: fmt.Errorf("prebuilt mode: no binaries declared")}
	}
	out := make([]Artifact, 0, len(b.Binaries))
	for _, bin := range b.Binaries {
		full := bin.Path
		if !filepath.IsAbs(full) {
			full = filepath.Join(root, bin.Path)
		}
		sum, err := SHA256File(full)
		if err != nil {
			return nil, &FailedError{Err: fmt.Errorf("prebuilt binary %s: %w", bin.Path, err)}
		}
		if bin.SHA256 != "" && !strings.EqualFold(bin.SHA256, sum) {
			return nil, &FailedError{Err: fmt.Errorf("prebuilt %s sha256 mismatch: declared %s but file is %s", bin.Path, bin.SHA256, sum)}
		}
		out = append(out, Artifact{OS: bin.OS, Arch: bin.Arch, Path: bin.Path, SHA256: sum})
	}
	return out, nil
}

// ArchivePrebuilt は各ビルド済みバイナリを配布アーカイブにまとめ、distDir(root からの相対)に置く。
//   - 命名: <project>_<version>_<os>_<arch>.tar.gz(windows は .zip)。GoReleaser 既定と一致させる。
//     publish 側(formulaArchives/scoopArchives/install.sh)がこの名前で URL を組むため、一致が契約。
//   - アーカイブ内の実行ファイル名: binaryName(windows は .exe を付す)。formula の `bin.install` と
//     install.sh の展開先がこの名前を前提にする。
//
// 実アーカイブの sha256 を付けた Archive 成果物を返す(publish が formula/manifest の sha に使う)。
func ArchivePrebuilt(root, distDir, project, version, binaryName string, bins []PrebuiltBinary) ([]Artifact, error) {
	outDir := filepath.Join(root, distDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, &FailedError{Err: fmt.Errorf("mkdir %s: %w", distDir, err)}
	}
	out := make([]Artifact, 0, len(bins))
	for _, bin := range bins {
		src := bin.Path
		if !filepath.IsAbs(src) {
			src = filepath.Join(root, bin.Path)
		}
		windows := bin.OS == "windows"
		ext := "tar.gz"
		if windows {
			ext = "zip"
		}
		archiveName := fmt.Sprintf("%s_%s_%s_%s.%s", project, version, bin.OS, bin.Arch, ext)
		archivePath := filepath.Join(outDir, archiveName)

		inner := binaryName
		if windows && !strings.HasSuffix(inner, ".exe") {
			inner += ".exe"
		}

		var err error
		if windows {
			err = writeZip(archivePath, inner, src)
		} else {
			err = writeTarGz(archivePath, inner, src)
		}
		if err != nil {
			return nil, &FailedError{Err: fmt.Errorf("archive %s: %w", archiveName, err)}
		}
		sum, err := SHA256File(archivePath)
		if err != nil {
			return nil, &FailedError{Err: fmt.Errorf("checksum %s: %w", archiveName, err)}
		}
		// Path は distDir 相対(利用者 root 基準)。publish は OS/Arch から名前を再構築するので
		// Path 自体は表示・アップロード用。
		out = append(out, Artifact{OS: bin.OS, Arch: bin.Arch, Path: filepath.Join(distDir, archiveName), SHA256: sum})
	}
	return out, nil
}

// writeTarGz は src バイナリを内部名 inner・mode 0755 で 1 エントリの tar.gz にする。
func writeTarGz(archivePath, inner, src string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	out, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	hdr := &tar.Header{
		Name: inner,
		Mode: 0o755,
		Size: info.Size(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.Copy(tw, f); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return out.Close()
}

// writeZip は src バイナリを内部名 inner・mode 0755 で 1 エントリの zip にする(windows 用)。
func writeZip(archivePath, inner, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	out, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	hdr := &zip.FileHeader{Name: inner, Method: zip.Deflate}
	hdr.SetMode(0o755)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Close()
}
