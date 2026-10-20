package gocmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestBin(t *testing.T) {
	t.Setenv("WY_GO", "/custom/go")
	if got := Bin(); got != "/custom/go" {
		t.Fatalf("Bin() with WY_GO = %q", got)
	}
	t.Setenv("WY_GO", "")
	want, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	if got := Bin(); got != want {
		t.Fatalf("Bin() = %q, want %q", got, want)
	}
	// Without go on PATH, Bin falls back to the toolchain in GOROOT.
	t.Setenv("PATH", t.TempDir())
	got := Bin()
	goroot := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goroot); err == nil {
		if got != goroot {
			t.Fatalf("Bin() without PATH = %q, want %q", got, goroot)
		}
	} else if got != "go" {
		t.Fatalf("Bin() without PATH or GOROOT = %q, want go", got)
	}
}

func TestError(t *testing.T) {
	base := errors.New("exit status 1")
	e := &Error{Args: []string{"build", "x"}, Output: "  compile failed\n", Err: base}
	if got := e.Error(); got != "compile failed" {
		t.Fatalf("Error() = %q", got)
	}
	e.Output = " \n"
	if got := e.Error(); got != "go build x: exit status 1" {
		t.Fatalf("Error() without output = %q", got)
	}
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	out, err := Run(ctx, "", nil, "env", "GOROOT")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatal("go env GOROOT printed nothing")
	}

	_, err = Run(ctx, t.TempDir(), nil, "no-such-subcommand")
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	if !slices.Equal(ge.Args, []string{"no-such-subcommand"}) || ge.Output == "" || ge.Err == nil {
		t.Fatalf("Error = %+v", ge)
	}
}

func TestRunEnvAndDir(t *testing.T) {
	dir := t.TempDir()
	out, err := Run(context.Background(), dir, []string{"GOOS=plan9"}, "env", "GOOS")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "plan9" {
		t.Fatalf("GOOS = %q, want env override plan9", got)
	}
}

func TestVersion(t *testing.T) {
	v, err := Version()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v, "go1.") {
		t.Fatalf("Version() = %q", v)
	}
	// Cached: a second call returns the same answer.
	if v2, _ := Version(); v2 != v {
		t.Fatalf("Version() changed: %q then %q", v, v2)
	}
}

func TestCacheDir(t *testing.T) {
	t.Setenv("WY_CACHE", "/tmp/wy-cache-test")
	if got := CacheDir(); got != "/tmp/wy-cache-test" {
		t.Fatalf("CacheDir() = %q", got)
	}
	t.Setenv("WY_CACHE", "")
	if got := CacheDir(); filepath.Base(got) != "wy" {
		t.Fatalf("CacheDir() = %q, want .../wy", got)
	}
}

func TestWithGOFLAGS(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	if got := WithGOFLAGS("-mod=mod"); got != "-mod=mod" {
		t.Fatalf("got %q", got)
	}
	if got := WithGOFLAGS(); got != "" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("GOFLAGS", "-trimpath")
	if got := WithGOFLAGS("-mod=mod", "-v"); got != "-trimpath -mod=mod -v" {
		t.Fatalf("got %q", got)
	}
}

func TestLdflagsFromGOFLAGS(t *testing.T) {
	for goflags, want := range map[string]string{
		"":                                 "",
		"-trimpath":                        "",
		"-ldflags=-s":                      "-s",
		"-trimpath -ldflags=-w -mod=mod":   "-w",
		"-ldflags=-s -ldflags=-X=main.v=1": "-X=main.v=1", // last one wins
	} {
		t.Setenv("GOFLAGS", goflags)
		if got := LdflagsFromGOFLAGS(); got != want {
			t.Errorf("GOFLAGS=%q: got %q, want %q", goflags, got, want)
		}
	}
}

func TestGoDirective(t *testing.T) {
	got := GoDirective()
	v := strings.TrimPrefix(strings.Fields(runtime.Version())[0], "go")
	if !strings.HasPrefix(v, got) || strings.Count(got, ".") > 1 {
		t.Fatalf("GoDirective() = %q for runtime %q", got, runtime.Version())
	}
	if strings.Count(v, ".") >= 1 && strings.Count(got, ".") != 1 {
		t.Fatalf("GoDirective() = %q, want major.minor", got)
	}
}

func TestHost(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	hb := Host()
	for _, want := range []string{
		"CGO_ENABLED=1",
		"GOOS=" + runtime.GOOS,
		"GOARCH=" + runtime.GOARCH,
		"GOFLAGS=-mod=mod",
		"GOWORK=off",
	} {
		if !slices.Contains(hb.Env, want) {
			t.Errorf("Host().Env = %v, missing %q", hb.Env, want)
		}
	}
	if v := runtime.Version(); strings.HasPrefix(v, "go") {
		if !slices.Contains(hb.Env, "GOTOOLCHAIN="+strings.Fields(v)[0]) {
			t.Errorf("Host().Env = %v, missing GOTOOLCHAIN", hb.Env)
		}
	}
	if hb.Info == nil {
		t.Fatal("test binaries carry build info")
	}
	for _, f := range hb.Flags {
		if !strings.HasPrefix(f, "-") {
			t.Errorf("bad flag %q", f)
		}
	}
}
