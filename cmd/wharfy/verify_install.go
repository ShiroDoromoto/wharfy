package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/ShiroDoromoto/wharfy/internal/config"
)

// verify_install.go — `--install` を付けたときだけ走る実インストールの末端。
//
// 既定の verify は probe(HTTP 照合)だけで終わる(D-4)。ここへ来るのは人間が --install を明示した
// ときに限る。apt/rpm は使い捨てコンテナで踏むが、script / goinstall は「利用者がその OS で踏む
// 経路」そのものなのでホストで走らせる — コンテナでは macOS/Windows の経路を踏めない。
//
// ホストに書くのは一時ディレクトリだけ: install.sh には PREFIX を、go install には GOBIN を渡す。
// PATH 上のものは何も置き換えない。

// errToolMissing は実インストールに要る道具(sh / go)がホストに無いこと。verify の失敗ではないので
// partial に落とし、何を入れれば最後まで踏めるかを言う。
var errToolMissing = errors.New("tool not available")

var (
	// scriptInstall / goinstallInstall は実インストールの末端(テストで差し替え)。dockerRun と同じ
	// 思想で、ネットワークとホストに触る一段だけを差し替え可能にする。
	// 出力は成功しても捨てない(失敗時の診断に返す)。
	scriptInstall    = runScriptInstall
	goinstallInstall = runGoinstallInstall
)

// runScriptInstall は公開 install.sh を取得し、一時 PREFIX へ実インストールして、入ったバイナリを
// 起動する。利用者が `curl … | sh` で踏む経路と同じものを、書き込み先だけ一時ディレクトリに向けて走らせる。
func runScriptInstall(ctx context.Context, url, binary string, run []string) ([]byte, error) {
	if _, err := exec.LookPath("sh"); err != nil {
		return nil, errToolMissing
	}
	dir, err := os.MkdirTemp("", "wharfy-verify-script-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	body, err := fetchInstallScript(ctx, url)
	if err != nil {
		return nil, err
	}
	script := filepath.Join(dir, config.InstallScriptName)
	if err := os.WriteFile(script, body, 0o600); err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	prefix := filepath.Join(dir, "prefix")
	cmd := exec.CommandContext(cctx, "sh", script)
	cmd.Env = append(os.Environ(), "PREFIX="+prefix)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, err
	}
	runOut, err := runInstalledBinary(cctx, filepath.Join(prefix, "bin", binary), run)
	return append(out, runOut...), err
}

// runGoinstallInstall は一時 GOBIN へ `go install <path>@<tag>` し、入ったバイナリを起動する。
//
// go.mod を持たない一時ディレクトリで走らせる: `go install path@version` はカレントの module に
// 縛られない形なので、wharfy を動かしているプロジェクトの依存を巻き込まない。
func runGoinstallInstall(ctx context.Context, installPath, tag string, run []string) ([]byte, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return nil, errToolMissing
	}
	dir, err := os.MkdirTemp("", "wharfy-verify-goinstall-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	cctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	gobin := filepath.Join(dir, "bin")
	cmd := exec.CommandContext(cctx, "go", "install", installPath+"@"+tag)
	cmd.Env = append(os.Environ(), "GOBIN="+gobin)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, err
	}
	installed, err := soleBinary(gobin)
	if err != nil {
		return out, err
	}
	runOut, err := runInstalledBinary(cctx, installed, run)
	return append(out, runOut...), err
}

// soleBinary は go install が GOBIN に置いた唯一の実行ファイルを返す。go install が付ける名前は
// install path の最終要素であって、wharfy.yaml の binary: とは別でありうる。だから名前では引かない。
func soleBinary(gobin string) (string, error) {
	ents, err := os.ReadDir(gobin)
	if err != nil {
		return "", fmt.Errorf("go install reported success but wrote nothing to GOBIN: %w", err)
	}
	var files []string
	for _, e := range ents {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	if len(files) != 1 {
		return "", fmt.Errorf("expected exactly one binary in GOBIN, found %d", len(files))
	}
	return filepath.Join(gobin, files[0]), nil
}

// runInstalledBinary は入ったバイナリが起動するかを見る。run(wharfy.yaml の verify.run)が在ればそれを、
// 無ければ --version → version → --help の順に試し、どれか 1 つが通れば「動いた」とみなす(サブコマンドの
// 名前はプロジェクトによる。依存不足やパス誤りならどれも起動しない)。コンテナ検証と同じ作法・同じ設定を使う
// ——ホストで踏むかコンテナで踏むかで「動いた」の意味が変わってはいけない。
//
// 版の一致は見ない。`go install` は wharfy の ldflags を通さないので、版を注入している CLI は dev と
// 名乗る。一致で判定すると偽陰性になる(依頼者の指摘)。判定は「入って、起動する」まで。
func runInstalledBinary(ctx context.Context, path string, run []string) ([]byte, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("the installer reported success but %s is not there: %w", path, err)
	}
	attempts := [][]string{{"--version"}, {"version"}, {"--help"}}
	if len(run) > 0 {
		attempts = [][]string{run}
	}
	var last []byte
	var lastErr error
	for _, args := range attempts {
		out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
		if err == nil {
			return out, nil
		}
		last, lastErr = out, err
	}
	return last, fmt.Errorf("the installed binary did not run: %w", lastErr)
}

// fetchInstallScript は公開 install.sh の本文を取る(probe と同じ URL)。
func fetchInstallScript(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
