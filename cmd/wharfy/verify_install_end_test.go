package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
)

// verify_install_end_test.go — `--install` の末端(sh / go を実際に起こす層)を実物で踏む。
//
// verify_install_test.go は scriptInstall / goinstallInstall を差し替えて、その手前(判定と
// partial/failed への振り分け)だけを見る。ここはその内側 —— 本物の install.sh を sh で走らせ、
// 本物の `go install` を踏み、入ったバイナリを起動する。PREFIX の渡し方も GOBIN の探索も
// --version → version → --help の連鎖も、差し替えの向こう側にあって一度も走っていなかった。
//
// ネットワークには出ない。install.sh の curl は PATH 先頭に置いた偽物が受け、`go install` は
// file:// の module proxy から取る。書き込みはすべて一時ディレクトリ。

// skipWithoutTools は要る道具が無いホスト(Windows など)でその検証を飛ばす。
func skipWithoutTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on this host", tool)
		}
	}
}

// writeExec は実行できるスクリプトを置く。
func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// hostAsset は install.sh がこのホストで組み立てるアセット名(uname の分岐と同じ結論)。
func hostAsset(project, version string) string {
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", project, version, runtime.GOOS, runtime.GOARCH)
}

// writeTarGz は install.sh が展開するのと同じ形のアーカイブを作る(バイナリは実行ビット付き)。
func writeTarGz(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

// fakeCurl は PATH の先頭に curl を置き換えて置く。install.sh の URL は GitHub に焼き込まれて
// いる(利用者が踏むのはその URL であって、書き換えられては検証にならない)ので、取得の一段だけを
// 差し替え、要求された URL を記録する。assets に無いアセットは curl 22(HTTP エラー)で返す。
func fakeCurl(t *testing.T, assets string) (requested func() string) {
	t.Helper()
	bin := t.TempDir()
	rec := filepath.Join(t.TempDir(), "urls")
	writeExec(t, filepath.Join(bin, "curl"), `#!/bin/sh
out=""
url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s\n' "$url" >> "`+rec+`"
asset="${url##*/}"
[ -f "`+assets+`/$asset" ] || exit 22
[ -n "$out" ] || exit 2
cp "`+assets+`/$asset" "$out"
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() string {
		b, err := os.ReadFile(rec)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
}

// realInstallerServer は config が生成する本物の install.sh を配る(probe と同じ URL の役)。
func realInstallerServer(t *testing.T, version string) *httptest.Server {
	t.Helper()
	script := config.GenerateInstallScript(config.Config{Project: "demo", Github: "acme/demo"}, version)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(script))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 本物の install.sh を sh で走らせ、一時 PREFIX に入ったバイナリが起動するところまで踏む。
func TestRunScriptInstallRunsTheRealInstaller(t *testing.T) {
	skipWithoutTools(t, "sh", "tar", "install", "uname", "mktemp")
	assets := t.TempDir()
	writeTarGz(t, filepath.Join(assets, hostAsset("demo", "1.2.0")), map[string]string{
		"demo": "#!/bin/sh\necho \"demo launched: $*\"\n",
	})
	requested := fakeCurl(t, assets)
	srv := realInstallerServer(t, "1.2.0")

	out, err := runScriptInstall(context.Background(), srv.URL, "demo", nil)
	if err != nil {
		t.Fatalf("the generated install.sh should install and run demo: %v\n%s", err, out)
	}
	if want := "https://github.com/acme/demo/releases/download/v1.2.0/" + hostAsset("demo", "1.2.0"); requested() != want {
		t.Errorf("install.sh should fetch the versioned asset: got %q want %q", requested(), want)
	}
	// PREFIX を渡さなければ $HOME/.local に入る。一時ディレクトリの下に入ったことを本文で確かめる。
	if !strings.Contains(string(out), "wharfy-verify-script-") {
		t.Errorf("the installer must be pointed at a throwaway PREFIX, not the host: %s", out)
	}
	if !strings.Contains(string(out), "demo launched: --version") {
		t.Errorf("the installed binary should be launched with the first attempt: %s", out)
	}
}

// アセットが無い(release から消えた・まだ上がっていない)→ curl が落ち、install.sh が説明して落ちる。
// probe だけでは見えない失敗であり、その本文がそのまま verify の detail になる。
func TestRunScriptInstallReportsADownloadFailure(t *testing.T) {
	skipWithoutTools(t, "sh", "tar", "install", "uname", "mktemp")
	fakeCurl(t, t.TempDir()) // アセットを 1 つも置かない
	srv := realInstallerServer(t, "1.2.0")

	out, err := runScriptInstall(context.Background(), srv.URL, "demo", nil)
	if err == nil {
		t.Fatalf("a missing asset must fail the install: %s", out)
	}
	if !strings.Contains(string(out), "download failed") || !strings.Contains(string(out), "curl exit 22") {
		t.Errorf("the installer should say what failed and how: %s", out)
	}
}

// verify.run はホストで入れたバイナリにも渡る。サブコマンド必須の CLI は --version では起動しない。
func TestRunScriptInstallHonoursVerifyRunOnTheHost(t *testing.T) {
	skipWithoutTools(t, "sh", "tar", "install", "uname", "mktemp")
	assets := t.TempDir()
	writeTarGz(t, filepath.Join(assets, hostAsset("demo", "1.2.0")), map[string]string{
		"demo": "#!/bin/sh\nif [ \"$1\" = status ] && [ \"$2\" = --quiet ]; then echo ok; exit 0; fi\necho usage >&2\nexit 2\n",
	})
	fakeCurl(t, assets)
	srv := realInstallerServer(t, "1.2.0")

	out, err := runScriptInstall(context.Background(), srv.URL, "demo", []string{"status", "--quiet"})
	if err != nil {
		t.Fatalf("verify.run should be used to launch the installed binary: %v\n%s", err, out)
	}
	out, err = runScriptInstall(context.Background(), srv.URL, "demo", nil)
	if err == nil {
		t.Fatalf("without verify.run none of --version / version / --help start this binary: %s", out)
	}
	if !strings.Contains(err.Error(), "did not run") {
		t.Errorf("a binary that never starts is reported as such: %v", err)
	}
}

// fileModuleProxy は module proxy を一時ディレクトリに組む(GOPROXY=file://)。versions は
// 版 → main.go の中身。ネットワークにも本物の proxy にも出ずに `go install` を踏むため。
func fileModuleProxy(t *testing.T, modulePath string, versions map[string]string) string {
	t.Helper()
	proxy := t.TempDir()
	dir := filepath.Join(proxy, filepath.FromSlash(modulePath), "@v")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module " + modulePath + "\n\ngo 1.22.0\n"
	var list strings.Builder
	for version, main := range versions {
		write := func(name, body string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write(version+".info", `{"Version":"`+version+`","Time":"2020-01-01T00:00:00Z"}`)
		write(version+".mod", gomod)
		writeModuleZip(t, filepath.Join(dir, version+".zip"), modulePath+"@"+version, map[string]string{
			"go.mod":           gomod,
			"cmd/demo/main.go": main,
		})
		list.WriteString(version + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "list"), []byte(list.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return proxy
}

// writeModuleZip は module proxy が返す zip(すべての要素が <module>@<version>/ の下)を書く。
func writeModuleZip(t *testing.T, path, prefix string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(prefix + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// withFileProxy は go の環境を一時ディレクトリへ閉じ込める(ネットワーク・sum db・toolchain の取得なし)。
// -modcacherw が無いと module cache が読み取り専用で残り、t.TempDir の後片付けが落ちる。
func withFileProxy(t *testing.T, proxy string) {
	t.Helper()
	t.Setenv("GOPROXY", "file://"+filepath.ToSlash(proxy))
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOFLAGS", "-modcacherw")
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOWORK", "off")
}

// 本物の `go install path@tag` を一時 GOBIN へ踏み、入ったバイナリを起動する。
// 版の一致は見ない —— go install は ldflags を通さないので、この main は dev と名乗る。
func TestRunGoinstallInstallRunsTheRealToolchain(t *testing.T) {
	skipWithoutTools(t, "go")
	proxy := fileModuleProxy(t, "example.com/demo", map[string]string{
		"v1.2.0": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"demo dev\") }\n",
	})
	withFileProxy(t, proxy)

	out, err := runGoinstallInstall(context.Background(), "example.com/demo/cmd/demo", "v1.2.0", nil)
	if err != nil {
		t.Fatalf("`go install` of a resolvable tag should install and run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "demo dev") {
		t.Errorf("the installed binary should have been launched: %s", out)
	}
}

// そのタグでビルドが通らない → `go install` が落ち、その出力が診断として返る。
func TestRunGoinstallInstallReportsABuildFailure(t *testing.T) {
	skipWithoutTools(t, "go")
	proxy := fileModuleProxy(t, "example.com/demo", map[string]string{
		"v1.3.0": "package main\n\nfunc main() { undefinedFn() }\n",
	})
	withFileProxy(t, proxy)

	out, err := runGoinstallInstall(context.Background(), "example.com/demo/cmd/demo", "v1.3.0", nil)
	if err == nil {
		t.Fatalf("a module that does not build must fail the install: %s", out)
	}
	if !strings.Contains(string(out), "undefinedFn") {
		t.Errorf("the compiler's complaint should be handed back: %s", out)
	}
}

// verify.run はホストの `go install` 経路にも渡る(script と同じ作法)。
func TestRunGoinstallInstallHonoursVerifyRun(t *testing.T) {
	skipWithoutTools(t, "go")
	proxy := fileModuleProxy(t, "example.com/demo", map[string]string{
		"v1.2.0": "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\n" +
			"func main() {\n\tif len(os.Args) != 2 || os.Args[1] != \"status\" {\n\t\tos.Exit(2)\n\t}\n\tfmt.Println(\"ok\")\n}\n",
	})
	withFileProxy(t, proxy)

	out, err := runGoinstallInstall(context.Background(), "example.com/demo/cmd/demo", "v1.2.0", []string{"status"})
	if err != nil {
		t.Fatalf("verify.run should launch the go-installed binary: %v\n%s", err, out)
	}
	if out, err := runGoinstallInstall(context.Background(), "example.com/demo/cmd/demo", "v1.2.0", nil); err == nil {
		t.Errorf("without verify.run this binary starts on none of the attempts: %s", out)
	}
}

// soleBinary: go install が置いた実行ファイルは名前で引かない(install path の最終要素であって
// binary: とは別でありうる)。だから「ちょうど 1 つ」だけを受ける。
func TestSoleBinaryRequiresExactlyOne(t *testing.T) {
	t.Run("one", func(t *testing.T) {
		gobin := t.TempDir()
		writeExec(t, filepath.Join(gobin, "renamed"), "#!/bin/sh\n")
		got, err := soleBinary(gobin)
		if err != nil {
			t.Fatalf("a single binary is the one we installed: %v", err)
		}
		if got != filepath.Join(gobin, "renamed") {
			t.Errorf("soleBinary should return the file it found: %q", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		if _, err := soleBinary(t.TempDir()); err == nil {
			t.Error("an empty GOBIN means go install wrote nothing")
		}
	})
	t.Run("two", func(t *testing.T) {
		gobin := t.TempDir()
		writeExec(t, filepath.Join(gobin, "demo"), "#!/bin/sh\n")
		writeExec(t, filepath.Join(gobin, "other"), "#!/bin/sh\n")
		if _, err := soleBinary(gobin); err == nil {
			t.Error("two binaries mean we cannot tell which one to launch")
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, err := soleBinary(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Error("a GOBIN that was never created is a failed install")
		}
	})
}

// インストーラが成功を名乗ったのに、そこにバイナリが無い ——「入った」の嘘を verify が拾う。
func TestRunInstalledBinaryReportsAMissingBinary(t *testing.T) {
	_, err := runInstalledBinary(context.Background(), filepath.Join(t.TempDir(), "demo"), nil)
	if err == nil || !strings.Contains(err.Error(), "is not there") {
		t.Fatalf("a binary that was never written must be reported: %v", err)
	}
}

// --version が無くても version / --help のどれかで起動すれば「動いた」(サブコマンドの名前はプロジェクトによる)。
func TestRunInstalledBinaryFallsBackThroughTheVersionChain(t *testing.T) {
	skipWithoutTools(t, "sh")
	bin := filepath.Join(t.TempDir(), "demo")
	writeExec(t, bin, "#!/bin/sh\nif [ \"$1\" = --help ]; then echo helped; exit 0; fi\nexit 1\n")

	out, err := runInstalledBinary(context.Background(), bin, nil)
	if err != nil {
		t.Fatalf("a binary that only answers --help still runs: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "helped") {
		t.Errorf("the output of the attempt that worked is returned: %s", out)
	}
}

// realPS1Server は config が生成する本物の install.ps1 を配る(Windows の利用者が irm で取るもの)。
func realPS1Server(t *testing.T, version string) *httptest.Server {
	t.Helper()
	script := config.GenerateInstallPS1(config.Config{Project: "demo", Github: "acme/demo"}, version)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(script))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 本物の install.ps1 を PowerShell で走らせる。ダウンロードまでは行かせない ——このテストは
// ネットワークに出ないので、arch を潰して installer 自身の分類済み失敗(exit 2)で折り返す。
//
// 踏みたいのは「PowerShell が本文を解釈して走る」ところ: v0.16.0 が配った install.ps1 は
// `$Project:` を PowerShell が ${drive:name} と読む穴で、まさにこの Fail の一行で壊れていた。
// CI では ubuntu(PowerShell 7)と windows(Windows PowerShell 5.1)の両方で走る。skip されるのは
// どちらも無い手元だけ。arch より後ろ(取得・展開・配置・PATH 追記)はどの OS でも踏めていない。
func TestRunPowerShellInstallRunsTheRealPS1(t *testing.T) {
	if _, err := powerShellPath(); err != nil {
		t.Skip("neither powershell nor pwsh is on this host")
	}
	t.Setenv("PROCESSOR_ARCHITECTURE", "SPARC")
	srv := realPS1Server(t, "1.2.0")

	out, err := runPowerShellInstall(context.Background(), srv.URL, "demo", nil)
	if err == nil {
		t.Fatalf("an unsupported arch must fail the install: %s", out)
	}
	if !strings.Contains(string(out), "demo: unsupported arch") {
		t.Errorf("the installer should name the project and what failed: %s", out)
	}
}

// 生成器と読み手の約束: install.ps1 に版を書くのは config、それを読むのは channel。
// 片方だけ書式を変えれば verify は版を読めなくなり、正しい release を赤にする。ここで結んでおく。
func TestPS1VersionReadsWhatTheGeneratorWrites(t *testing.T) {
	ps1 := config.GenerateInstallPS1(config.Config{Project: "demo", Github: "acme/demo"}, "1.2.0")
	if v := channel.PS1Version(ps1); v != "1.2.0" {
		t.Errorf("the generated install.ps1 should carry its version where the probe looks: %q", v)
	}
	sh := config.GenerateInstallScript(config.Config{Project: "demo", Github: "acme/demo"}, "1.2.0")
	if v := channel.ScriptVersion(sh); v != "1.2.0" {
		t.Errorf("the generated install.sh should carry its version where the probe looks: %q", v)
	}
}
