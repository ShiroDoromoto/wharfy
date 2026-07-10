package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ShiroDoromoto/wharfy/internal/config"
)

// verify_install.go — `--install` を付けたときだけ走る実インストールの末端。
//
// 既定の verify は probe(HTTP 照合)だけで終わる(D-4)。ここへ来るのは人間が --install を明示した
// ときに限る。apt/rpm は使い捨てコンテナで踏むが、script / goinstall は「利用者がその OS で踏む
// 経路」そのものなのでホストで走らせる — コンテナでは macOS/Windows の経路を踏めない。
//
// ホストに書くのは一時ディレクトリだけ: install.sh には PREFIX を、install.ps1 には WHARFY_PREFIX を、
// go install には GOBIN を渡す。PATH 上のものは何も置き換えない。

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

// scriptInstaller は「この OS の利用者が実際に踏むインストーラ」——取得先 URL・ファイル名・走らせる道具。
type scriptInstaller struct {
	URL  string // 公開インストーラの URL
	Name string // install.sh / install.ps1
	Tool string // それを走らせる道具(sh / powershell)
}

// hostScriptInstaller は install.sh の公開 URL から、goos に対応するインストーラを選ぶ。
//
// script チャネルは install.sh(mac/Linux)と install.ps1(Windows)を同じ release に並べて置く。
// Windows のホストで install.sh を踏んでも「darwin と linux 向けだ」と断られるだけで、配っている
// install.ps1 は一度も走らない ——だから踏む先そのものを OS で選ぶ。
func hostScriptInstaller(goos, shURL string) scriptInstaller {
	if goos == "windows" {
		return scriptInstaller{URL: siblingURL(shURL, config.InstallPS1Name), Name: config.InstallPS1Name, Tool: "powershell"}
	}
	return scriptInstaller{URL: shURL, Name: config.InstallScriptName, Tool: "sh"}
}

// siblingURL は url と同じ場所に置かれた別ファイルの URL を返す(release も script.base_url も、
// install.sh と install.ps1 を同じ場所に並べる)。
func siblingURL(raw, name string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return raw
	}
	dir := path.Dir(u.Path)
	if !strings.HasPrefix(dir, "/") {
		dir = "/"
	}
	u.Path = path.Join(dir, name)
	return u.String()
}

// runScriptInstall は公開インストーラを取得し、一時の置き場所へ実インストールして、入ったバイナリを
// 起動する。利用者がその OS で踏む経路と同じものを、書き込み先だけ一時ディレクトリに向けて走らせる。
func runScriptInstall(ctx context.Context, url, binary string, run []string) ([]byte, error) {
	if runtime.GOOS == "windows" {
		return runPowerShellInstall(ctx, url, binary, run)
	}
	return runShInstall(ctx, url, binary, run)
}

// runShInstall は install.sh を sh で走らせる(利用者の `curl … | sh` と同じ)。入り先は一時 PREFIX。
func runShInstall(ctx context.Context, url, binary string, run []string) ([]byte, error) {
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

// runPowerShellInstall は install.ps1 を PowerShell で走らせる(利用者の `irm … | iex` と同じ)。
//
// 入り先は WHARFY_PREFIX で移す ——install.sh の PREFIX に当たるが、install.sh が PREFIX/bin へ
// 置くのに対し、install.ps1 は与えられた prefix の直下に <binary>.exe を置く。
//
// 走らせるのは OS 標準の Windows PowerShell を優先する(install.ps1 が想定する実行環境であり、
// 利用者の大半がそれで踏む)。無ければ PowerShell 7(pwsh)。-ExecutionPolicy Bypass は、利用者が
// `irm | iex` で踏むときと同じく、ポリシーに阻まれずに本文を走らせるため。
func runPowerShellInstall(ctx context.Context, url, binary string, run []string) ([]byte, error) {
	shell, err := powerShellPath()
	if err != nil {
		return nil, err
	}
	return runPowerShellInstallWith(ctx, shell, url, binary, run)
}

// runPowerShellInstallWith は走らせる PowerShell を呼び手が指す。verify は powerShellPath が選んだ
// 一つを渡すが、テストはホストに在る PowerShell を一つずつ渡して、どれで踏んだかを明示できる。
func runPowerShellInstallWith(ctx context.Context, shell, url, binary string, run []string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "wharfy-verify-script-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	body, err := fetchInstallScript(ctx, url)
	if err != nil {
		return nil, err
	}
	script := filepath.Join(dir, config.InstallPS1Name)
	if err := os.WriteFile(script, body, 0o600); err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	prefix := filepath.Join(dir, "prefix")
	cmd := exec.CommandContext(cctx, shell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script)
	cmd.Env = append(os.Environ(), "WHARFY_PREFIX="+prefix)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, err
	}
	runOut, err := runInstalledBinary(cctx, filepath.Join(prefix, binary+".exe"), run)
	return append(out, runOut...), err
}

// powerShellNames は install.ps1 を走らせる PowerShell の候補で、並びがそのまま優先順位。
// Windows PowerShell(powershell)を先に見るのは、それが install.ps1 の本番環境だから
// ——Windows に最初から入っているのは 5.1 で、利用者の大半はそれで踏む。pwsh(7)は次点。
var powerShellNames = []string{"powershell", "pwsh"}

// powerShellPath は install.ps1 を走らせる PowerShell を探す。どちらも無ければ errToolMissing。
func powerShellPath() (string, error) {
	for _, name := range powerShellNames {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errToolMissing
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

// fetchInstallScript は公開インストーラ(install.sh / install.ps1)の本文を取る。
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
