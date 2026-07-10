//go:build dockerverify

// verify_docker_test.go — verify のコンテナ検証を実 docker で踏む回帰テスト。
//
// 単体テスト(verify_test.go)は fake の docker runner が非ゼロで返すと verify_failed になる、
// までしか押さえていない。「コンテナの中で本当に install が失敗するか」「その失敗を wharfy が
// 拾えるか」の接続部は誰も確かめていなかった。ここは実際に壊れた deb を作って踏む。
//
// 実 docker とネットワークが要るので既定の go test では走らない:
//
//	go test -tags dockerverify ./cmd/wharfy/ -run TestDockerVerify -timeout 20m
//
// repo は LAN アドレスで配る。ホスト(probe)とコンテナ(install)が同じ URL で届く必要があり、
// 127.0.0.1 はコンテナから、host.docker.internal はホストから解決できないため。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/output"
)

const dockerVerifyVersion = "1.2.0"

// 依存が満たせない deb は install が落ちる。アップロードの 200 では捕まえられない壊れ方。
func TestDockerVerifyAptMissingDependency(t *testing.T) {
	res := runDockerVerifyApt(t, debSpec{depends: []string{"wharfy-verify-no-such-package"}})
	detail := requireVerifyFailed(t, res, "unmet dependency")

	// apt がエラーを吐き、demo は設定まで進んでいない。ここを見ないと「落ちた」だけで
	// 「依存不足で落ちた」ことを示せない(壊し方と落ちる段階が対応していることの確認)。
	if !strings.Contains(detail, "E: ") {
		t.Errorf("apt should report the broken dependency:\n%s", detail)
	}
	if strings.Contains(detail, "Setting up demo") {
		t.Errorf("demo must not be installed when its dependency is missing:\n%s", detail)
	}
}

// バイナリが PATH に無い deb は install が通っても起動確認で落ちる(ファイル配置の誤り)。
func TestDockerVerifyAptBinaryNotOnPath(t *testing.T) {
	res := runDockerVerifyApt(t, debSpec{binaryName: "demo-misplaced"})
	detail := requireVerifyFailed(t, res, "a binary that is not on PATH")

	// install は通り、起動確認だけが落ちる。依存不足とは別の段階で捕まえていることを確かめる。
	if !strings.Contains(detail, "Setting up demo") {
		t.Errorf("the package itself should install fine; only the launch check fails:\n%s", detail)
	}
}

// 対照。壊していない deb は install も起動確認も通り、verify は緑になる。
// これが落ちるなら、上の 2 本は「壊したから落ちた」ことを示さない。
func TestDockerVerifyAptHealthyPackage(t *testing.T) {
	res := runDockerVerifyApt(t, debSpec{})
	if !res.OK {
		t.Fatalf("a healthy package must verify ok: %+v", res)
	}
	if ck := checksOf(t, res); len(ck) != 1 || ck[0].Status != verifyStatusOK {
		t.Fatalf("apt check should be verified: %+v", ck)
	}
}

// debSpec は仕込む壊れ方。ゼロ値は健全な deb。
type debSpec struct {
	depends    []string // 満たせない依存を宣言する
	binaryName string   // /usr/bin へ置く名前。project と違えば PATH から見つからない
}

// runDockerVerifyApt は spec の deb を含む apt repo を立て、その repo を配る wharfy.yaml で verify を走らせる。
// 既定の verify は probe だけなので、コンテナを踏むために --install を立てる。
func runDockerVerifyApt(t *testing.T, spec debSpec) output.Result {
	t.Helper()
	requireDocker(t)
	withInstall(t)

	pkg := "demo"
	binary := spec.binaryName
	if binary == "" {
		binary = pkg
	}
	root := scratchModule(t)
	repoDir := t.TempDir()
	debName := buildDeb(t, root, repoDir, pkg, binary, spec.depends)
	writePackagesIndex(t, repoDir, pkg, debName, spec.depends)
	repoURL := serveOnLAN(t, repoDir)

	writeConfig(t, root, "project: "+pkg+"\nchannels: [apt]\napt:\n  repo: "+repoURL+"\n")
	recordPublishFor(t, root, "apt", dockerVerifyVersion, repoURL)
	chdir(t, root)

	return runVerify(context.Background(), mustLookup(t, "verify"), nil)
}

// requireVerifyFailed は verify_failed で返ることを確かめ、診断に載ったコンテナの出力を返す。
func requireVerifyFailed(t *testing.T, res output.Result, what string) string {
	t.Helper()
	if res.OK || len(res.Errors) == 0 || res.Errors[0].Code != output.ErrVerifyFailed {
		t.Fatalf("a package with %s must fail verify: %+v", what, res)
	}
	detail := res.Errors[0].Detail
	if strings.TrimSpace(detail) == "" {
		t.Fatalf("the container output should be handed back as detail: %+v", res.Errors[0])
	}
	if !hasNextDo(res, "wharfy publish apt --yes") {
		t.Errorf("verify must guide to re-publish: %+v", res.Next)
	}
	return detail
}

// requireDocker は docker が使えないなら落とす(このテストは明示的に opt-in で呼ばれる)。
func requireDocker(t *testing.T) {
	t.Helper()
	if !dockerAvailable() {
		t.Fatal("docker is required: this test is opt-in via -tags dockerverify")
	}
}

// buildDeb は linux バイナリを作り、nfpm(wharfy 本体と同じ経路)で deb を repoDir へ置く。
func buildDeb(t *testing.T, root, repoDir, pkg, binary string, depends []string) string {
	t.Helper()
	arch := runtime.GOARCH // コンテナはホストと同じ arch で走る
	binPath := filepath.Join(root, "demo-linux")
	crossBuildLinux(t, root, binPath, arch)

	spec := build.PackageSpec{
		Format: "deb", Ext: ".deb",
		Name: pkg, BinaryName: binary, Version: dockerVerifyVersion,
		Maintainer: "wharfy <verify@example.com>", Description: "wharfy verify fixture",
		Depends: depends,
	}
	arts, err := build.PackagePrebuilt(root, "dist", spec, []build.PrebuiltBinary{{OS: "linux", Arch: arch, Path: binPath}})
	if err != nil {
		t.Fatalf("build the deb fixture: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected one deb, got %v", arts)
	}
	name := filepath.Base(arts[0].Path)
	if err := copyFile(filepath.Join(root, arts[0].Path), filepath.Join(repoDir, name)); err != nil {
		t.Fatalf("place the deb in the repo: %v", err)
	}
	return name
}

// crossBuildLinux は起動確認で実行される最小の linux バイナリを作る(cgo 無しなのでクロスできる)。
func crossBuildLinux(t *testing.T, root, out, arch string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "main.go")
	prog := "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"demo " + dockerVerifyVersion + "\") }\n"
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatalf("write fixture source: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", out, src)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross build the fixture binary: %v\n%s", err, b)
	}
}

// writePackagesIndex は flat な apt repo のインデックスを書く(AptProbe と apt の両方がこれを読む)。
// Depends はここに載っていないと apt が依存解決に使わない(deb の control だけでは足りない)。
func writePackagesIndex(t *testing.T, repoDir, pkg, debName string, depends []string) {
	t.Helper()
	debPath := filepath.Join(repoDir, debName)
	info, err := os.Stat(debPath)
	if err != nil {
		t.Fatalf("stat the deb: %v", err)
	}
	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	stanza := fmt.Sprintf(`Package: %s
Version: %s
Architecture: %s
Maintainer: wharfy <verify@example.com>
Filename: %s
Size: %d
SHA256: %s
Description: wharfy verify fixture
`, pkg, dockerVerifyVersion, arch, debName, info.Size(), sha256Of(t, debPath))
	if len(depends) > 0 {
		stanza += "Depends: " + strings.Join(depends, ", ") + "\n"
	}
	if err := os.WriteFile(filepath.Join(repoDir, "Packages"), []byte(stanza+"\n"), 0o644); err != nil {
		t.Fatalf("write the Packages index: %v", err)
	}
}

// serveOnLAN は repoDir を LAN アドレスで配り、その URL を返す。
func serveOnLAN(t *testing.T, repoDir string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(repoDir))}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("http://%s:%d/", lanIP(t), port)
}

// lanIP はコンテナからも届くホストのアドレスを返す。
func lanIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("interface addrs: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		return ipnet.IP.String()
	}
	t.Fatal("no non-loopback IPv4 address: the container cannot reach a repo served here")
	return ""
}

func sha256Of(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
