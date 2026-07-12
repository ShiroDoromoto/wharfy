package build

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPushMultiArchStagesAndInvokesBuildx: linux バイナリを arch 別に整え、TARGETARCH の
// Dockerfile を書き、buildx build --platform …,--push … を組み立てて呼ぶ。
func TestPushMultiArchStagesAndInvokesBuildx(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/app-linux-amd64"), "amd64-bin")
	writeFile(t, filepath.Join(root, "dist/app-linux-arm64"), "arm64-bin")

	var gotBin string
	var gotArgs []string
	b := &PrebuiltContainerizer{
		Bin:      "docker",
		LookPath: func(string) (string, error) { return "/usr/bin/docker", nil },
		Run: func(_ context.Context, dir, name string, args ...string) ([]byte, error) {
			gotBin = name
			gotArgs = args
			return nil, nil
		},
	}
	bins := []PrebuiltBinary{
		{OS: "linux", Arch: "amd64", Path: "dist/app-linux-amd64"},
		{OS: "linux", Arch: "arm64", Path: "dist/app-linux-arm64"},
		{OS: "darwin", Arch: "arm64", Path: "dist/ignored"},
	}
	if err := b.PushMultiArch(context.Background(), root, ".wharfy/dist", "ghcr.io/acme/app", "0.1.0", "app", bins); err != nil {
		t.Fatalf("PushMultiArch: %v", err)
	}

	if gotBin != "docker" {
		t.Errorf("bin = %q, want docker", gotBin)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{
		"buildx build",
		"--platform linux/amd64,linux/arm64",
		"-t ghcr.io/acme/app:0.1.0",
		"-t ghcr.io/acme/app:latest",
		"--push",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q\n got: %s", want, joined)
		}
	}

	// staging: 各 arch のバイナリが配置される
	for _, arch := range []string{"amd64", "arm64"} {
		p := filepath.Join(root, ".wharfy/dist/oci", arch, "app")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("staged binary missing for %s: %v", arch, err)
		}
	}
	// Dockerfile が TARGETARCH で arch 別バイナリを COPY する
	df, err := os.ReadFile(filepath.Join(root, ".wharfy/dist/oci.Dockerfile"))
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}
	if !strings.Contains(string(df), "${TARGETARCH}") || !strings.Contains(string(df), "/usr/bin/app") {
		t.Errorf("Dockerfile wrong:\n%s", df)
	}
}

// TestPushMultiArchNoLinux: linux バイナリが無ければ失敗(darwin/windows だけでは作れない)。
func TestPushMultiArchNoLinux(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/app-darwin-arm64"), "x")
	b := &PrebuiltContainerizer{
		LookPath: func(string) (string, error) { return "docker", nil },
		Run:      func(context.Context, string, string, ...string) ([]byte, error) { return nil, nil },
	}
	err := b.PushMultiArch(context.Background(), root, ".wharfy/dist", "img", "0.1.0", "app",
		[]PrebuiltBinary{{OS: "darwin", Arch: "arm64", Path: "dist/app-darwin-arm64"}})
	if err == nil {
		t.Fatal("expected error with no linux binaries")
	}
}

// TestPushMultiArchDockerMissing: docker が無ければ UnavailableError。
func TestPushMultiArchDockerMissing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dist/app-linux-amd64"), "x")
	b := &PrebuiltContainerizer{
		LookPath: func(string) (string, error) { return "", os.ErrNotExist },
		Run:      func(context.Context, string, string, ...string) ([]byte, error) { return nil, nil },
	}
	err := b.PushMultiArch(context.Background(), root, ".wharfy/dist", "img", "0.1.0", "app",
		[]PrebuiltBinary{{OS: "linux", Arch: "amd64", Path: "dist/app-linux-amd64"}})
	var un *UnavailableError
	if err == nil || !asUnavailable(err, &un) {
		t.Fatalf("expected UnavailableError, got %v", err)
	}
}

func asUnavailable(err error, target **UnavailableError) bool {
	for err != nil {
		if u, ok := err.(*UnavailableError); ok {
			*target = u
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
		} else {
			return false
		}
	}
	return false
}

// RegistryHost: 最初のセグメントがホストらしければそれ、でなければ Docker Hub の暗黙参照。
func TestRegistryHost(t *testing.T) {
	for image, want := range map[string]string{
		"ghcr.io/acme/demo":        "ghcr.io",
		"localhost:5000/acme/demo": "localhost:5000",
		"acme/demo":                "docker.io",
		"demo":                     "docker.io",
	} {
		if got := RegistryHost(image); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

// Login はトークンを stdin に流す(argv に載せない)。
func TestRegistryLoginPassesTokenOnStdin(t *testing.T) {
	var gotStdin, gotArgs string
	l := &RegistryLogin{
		Bin:      "docker",
		LookPath: func(string) (string, error) { return "/usr/bin/docker", nil },
		Run: func(_ context.Context, stdin, _ string, args ...string) ([]byte, error) {
			gotStdin, gotArgs = stdin, strings.Join(args, " ")
			return nil, nil
		},
	}
	if err := l.Login(context.Background(), "ghcr.io", "acme", "secret"); err != nil {
		t.Fatal(err)
	}
	if gotStdin != "secret" {
		t.Errorf("token should go to stdin: %q", gotStdin)
	}
	if gotArgs != "login ghcr.io -u acme --password-stdin" {
		t.Errorf("unexpected argv: %q", gotArgs)
	}
}

// トークンが無ければ何もしない(既存の資格情報に任せる経路を壊さない)。
func TestRegistryLoginSkipsWithoutToken(t *testing.T) {
	called := false
	l := &RegistryLogin{
		Bin:      "docker",
		LookPath: func(string) (string, error) { return "/usr/bin/docker", nil },
		Run:      func(context.Context, string, string, ...string) ([]byte, error) { called = true; return nil, nil },
	}
	if err := l.Login(context.Background(), "ghcr.io", "acme", ""); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("no token → no docker login")
	}
}
