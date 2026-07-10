//go:build dockerverify

// verify_docker_rpm_test.go — verify の rpm 側を実 docker で踏む回帰テスト(deb 側は verify_docker_test.go)。
//
// rpm を deb と別ファイルにしたのは足場が違うから: deb の flat repo は Packages を手で書けるが、
// rpm は repodata(repomd.xml → primary.xml)が要り、それを作る createrepo_c はホスト(macOS)に無い。
// 検証に使うのと同じ fedora コンテナの中で作る ——実際に配布者が持つのと同じ、本物のメタデータになる。
//
//	go test -tags dockerverify ./cmd/wharfy/ -run TestDockerVerifyRpm -timeout 20m
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// 満たせない Requires を持つ rpm は install が落ちる。repo に置けている(probe は通る)ので、
// メタデータの照合だけでは見えない壊れ方。
func TestDockerVerifyRpmMissingDependency(t *testing.T) {
	res := runDockerVerifyRpm(t, rpmSpec{depends: []string{"wharfy-verify-no-such-package"}})
	detail := requireVerifyFailed(t, res, "rpm", "unmet dependency")

	if !strings.Contains(detail, "wharfy-verify-no-such-package") {
		t.Errorf("dnf should name the dependency it cannot resolve:\n%s", detail)
	}
	if strings.Contains(detail, installedMarker) {
		t.Errorf("demo must not be installed when its dependency is missing:\n%s", detail)
	}
}

// バイナリが PATH に無い rpm は install が通っても起動確認で落ちる(ファイル配置の誤り)。
// 依存不足とは別の段階で捕まえていることを、install が済んだ印で確かめる。
func TestDockerVerifyRpmBinaryNotOnPath(t *testing.T) {
	res := runDockerVerifyRpm(t, rpmSpec{binaryName: "demo-misplaced"})
	detail := requireVerifyFailed(t, res, "rpm", "a binary that is not on PATH")

	if !strings.Contains(detail, installedMarker) {
		t.Errorf("the package itself should install fine; only the launch check fails:\n%s", detail)
	}
}

// 対照。壊していない rpm は install も起動確認も通り、verify は緑になる。
// これが落ちるなら、上の 2 本は「壊したから落ちた」ことを示さない。
func TestDockerVerifyRpmHealthyPackage(t *testing.T) {
	res := runDockerVerifyRpm(t, rpmSpec{})
	if !res.OK {
		t.Fatalf("a healthy package must verify ok: %+v", res)
	}
	if ck := checksOf(t, res); len(ck) != 1 || ck[0].Status != verifyStatusOK {
		t.Fatalf("rpm check should be verified: %+v", ck)
	}
}

// installedMarker は dnf が install を終えた印(-q でも要約は出る)。これで「入ったが起動しなかった」と
// 「入りもしなかった」を見分ける ——落ちた段階が壊し方と対応していることを示せなければ、
// 落ちたこと自体は何も証明しない。
const installedMarker = "Installed:"

// rpmSpec は仕込む壊れ方。ゼロ値は健全な rpm。
type rpmSpec struct {
	depends    []string // 満たせない依存を宣言する
	binaryName string   // /usr/bin へ置く名前。project と違えば PATH から見つからない
}

// runDockerVerifyRpm は spec の rpm を含む yum repo を立て、その repo を配る wharfy.yaml で verify を走らせる。
func runDockerVerifyRpm(t *testing.T, spec rpmSpec) output.Result {
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
	buildRpm(t, root, repoDir, pkg, binary, spec.depends)
	createRepodata(t, repoDir)
	repoURL := serveOnLAN(t, repoDir)

	writeConfig(t, root, "project: "+pkg+"\nchannels: [rpm]\nrpm:\n  repo: "+repoURL+"\n")
	recordPublishFor(t, root, "rpm", dockerVerifyVersion, repoURL)
	chdir(t, root)

	return runVerify(context.Background(), mustLookup(t, "verify"), nil)
}

// buildRpm は linux バイナリを作り、nfpm(wharfy 本体と同じ経路)で rpm を repoDir へ置く。
func buildRpm(t *testing.T, root, repoDir, pkg, binary string, depends []string) {
	t.Helper()
	// rpm はコンテナの中の dnf が入れる。ホストの arch ではなく、そのイメージが実際に走る arch に
	// 合わせないと「アーキテクチャが違う」だけで入らず、壊し方と無関係に落ちる。
	arch := containerArch(t, defaultVerifyImages["rpm"])
	binPath := filepath.Join(root, "demo-linux")
	crossBuildLinux(t, root, binPath, arch)

	spec := build.PackageSpec{
		Format: "rpm", Ext: ".rpm",
		Name: pkg, BinaryName: binary, Version: dockerVerifyVersion,
		Maintainer: "wharfy <verify@example.com>", Description: "wharfy verify fixture",
		Depends: depends,
	}
	arts, err := build.PackagePrebuilt(root, "dist", spec, []build.PrebuiltBinary{{OS: "linux", Arch: arch, Path: binPath}})
	if err != nil {
		t.Fatalf("build the rpm fixture: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected one rpm, got %v", arts)
	}
	name := filepath.Base(arts[0].Path)
	if err := copyFile(filepath.Join(root, arts[0].Path), filepath.Join(repoDir, name)); err != nil {
		t.Fatalf("place the rpm in the repo: %v", err)
	}
}

// createRepodata は repoDir に repodata/ を作る(RpmProbe と dnf の両方がこれを読む)。
// createrepo_c はホストに無いので、検証に使うのと同じ fedora イメージの中で走らせる。
// 生成物は root 所有で残るため、最後にホストの uid へ返す(t.TempDir の後片付けが落ちないように)。
func createRepodata(t *testing.T, repoDir string) {
	t.Helper()
	// 圧縮は既定(zstd)のまま。createrepo_c が実際に吐く primary.xml.zst を RpmProbe が読めることを
	// ここで踏む。fury など hosted repo が配る gz/非圧縮は internal/channel の単体テストが見る。
	script := fmt.Sprintf(`set -eu
dnf install -y -q createrepo_c
createrepo_c /repo
chown -R %d:%d /repo/repodata`, os.Getuid(), os.Getgid())
	cmd := exec.CommandContext(context.Background(), "docker", "run", "--rm",
		"-v", repoDir+":/repo", defaultVerifyImages["rpm"], "bash", "-lc", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the repodata with createrepo_c: %v\n%s", err, out)
	}
}

// containerArch は image が実際に走る arch を、そのイメージ自身に聞く。手元に引いてある変種が
// ホストと違う(arm64 のマシンに amd64 の fedora)ことは普通にある。
func containerArch(t *testing.T, image string) string {
	t.Helper()
	// stdout だけを読む。docker はイメージの変種がホストと違うとき stderr に警告を出す。
	out, err := exec.CommandContext(context.Background(), "docker", "run", "--rm", image, "uname", "-m").Output()
	if err != nil {
		t.Fatalf("ask %s which arch it runs: %v", image, err)
	}
	switch machine := strings.TrimSpace(string(out)); machine {
	case "aarch64":
		return "arm64"
	case "x86_64":
		return "amd64"
	default:
		t.Fatalf("%s runs on %q, which wharfy does not package for", image, machine)
		return ""
	}
}
