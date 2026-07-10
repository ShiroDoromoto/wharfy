package channel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// bareAurRepo は file:// で clone/push できる空の AUR パッケージ repo を作り、その親ディレクトリを返す。
func bareAurRepo(t *testing.T, pkgname string) string {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, pkgname+".git")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(dir, "init", "--bare", "--initial-branch=master", repo)

	// clone 元に最低 1 コミット要る(空 repo は HEAD が無く rev-parse できない)。
	seed := filepath.Join(dir, "seed")
	run(dir, "clone", repo, seed)
	if err := os.WriteFile(filepath.Join(seed, "PKGBUILD"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", "-A")
	run(seed, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-m", "seed")
	run(seed, "push", "origin", "HEAD:master")
	return dir
}

func aurHead(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v: %s", err, out)
	}
	return trimSpace(string(out))
}

// 同じ内容を 2 度 push しても、2 度目は commit も push もせず現在の HEAD を返す。
// 凍結(ship:false)の publish は毎回同じ生成物を書くので、ここが失敗すると publish が落ちる。
func TestGitAurPusherIdempotentWhenUnchanged(t *testing.T) {
	pkg := "demo-bin"
	dir := bareAurRepo(t, pkg)
	remote := filepath.Join(dir, pkg+".git")
	p := &GitAurPusher{SSHKey: "unused", GitName: "wharfy", GitMail: "wharfy@example.com", Host: "file://" + dir}
	files := map[string]string{"PKGBUILD": "pkgname=demo-bin\n", ".SRCINFO": "pkgbase = demo-bin\n"}

	first, err := p.Push(context.Background(), pkg, files)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	if first == "" {
		t.Fatal("first push returned no commit")
	}
	if got := aurHead(t, remote); got != first {
		t.Fatalf("remote HEAD = %s, want %s", got, first)
	}

	second, err := p.Push(context.Background(), pkg, files)
	if err != nil {
		t.Fatalf("second push (unchanged) must not fail: %v", err)
	}
	if second != first {
		t.Fatalf("unchanged push moved HEAD: %s → %s", first, second)
	}
}

// 内容が変われば commit して push する(冪等化で書き込みまで止めていない)。
func TestGitAurPusherPushesWhenChanged(t *testing.T) {
	pkg := "demo-bin"
	dir := bareAurRepo(t, pkg)
	remote := filepath.Join(dir, pkg+".git")
	p := &GitAurPusher{SSHKey: "unused", GitName: "wharfy", GitMail: "wharfy@example.com", Host: "file://" + dir}

	first, err := p.Push(context.Background(), pkg, map[string]string{"PKGBUILD": "pkgver=1.0.0\n"})
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	second, err := p.Push(context.Background(), pkg, map[string]string{"PKGBUILD": "pkgver=1.1.0\n"})
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if second == first {
		t.Fatal("changed content did not create a new commit")
	}
	if got := aurHead(t, remote); got != second {
		t.Fatalf("remote HEAD = %s, want %s", got, second)
	}
}
