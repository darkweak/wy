// Package gocmd wraps invocations of the go command and knows how to
// reproduce the build configuration of the running binary, which is what
// makes plugins built at runtime loadable into it.
package gocmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Bin returns the go command to use. WY_GO overrides discovery.
func Bin() string {
	if b := os.Getenv("WY_GO"); b != "" {
		return b
	}
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	// Fall back to the toolchain the binary was built with, if still present.
	p := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return "go"
}

// Error is returned when the go command fails; Output holds its stderr.
type Error struct {
	Args   []string
	Output string
	Err    error
}

func (e *Error) Error() string {
	out := strings.TrimSpace(e.Output)
	if out == "" {
		return fmt.Sprintf("go %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return out
}

// Run executes the go command in dir with extra environment entries.
func Run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, Bin(), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &Error{Args: args, Output: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

var (
	versionOnce sync.Once
	version     string
	versionErr  error
)

// Version reports the version of the go command (e.g. "go1.24.7").
func Version() (string, error) {
	versionOnce.Do(func() {
		out, err := Run(context.Background(), "", nil, "env", "GOVERSION")
		if err != nil {
			versionErr = fmt.Errorf("wy needs a Go toolchain on PATH (or WY_GO): %w", err)
			return
		}
		version = strings.TrimSpace(string(out))
	})
	return version, versionErr
}

// CacheDir is wy's persistent cache root.
func CacheDir() string {
	if d := os.Getenv("WY_CACHE"); d != "" {
		return d
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "wy")
}

// HostBuild describes how the running binary was built, so that plugins
// can be compiled with identical settings.
type HostBuild struct {
	Flags []string // go build flags (-trimpath, -race, -tags, ...)
	Env   []string // environment (GOOS, GOARCH, CGO_ENABLED, GOTOOLCHAIN, ...)
	Info  *debug.BuildInfo
}

// Host inspects the running binary's build information.
func Host() HostBuild {
	hb := HostBuild{Env: []string{
		"CGO_ENABLED=1", // required by -buildmode=plugin
		"GOOS=" + runtime.GOOS,
		"GOARCH=" + runtime.GOARCH,
		"GOFLAGS=" + WithGOFLAGS("-mod=mod"),
		"GOWORK=off",
	}}
	// Build plugins with exactly the toolchain that built the host; the go
	// command fetches it if the local one differs.
	if v := runtime.Version(); strings.HasPrefix(v, "go") {
		hb.Env = append(hb.Env, "GOTOOLCHAIN="+strings.Fields(v)[0])
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return hb
	}
	hb.Info = bi
	for _, s := range bi.Settings {
		switch s.Key {
		case "-race", "-msan", "-asan", "-trimpath":
			if s.Value == "true" {
				hb.Flags = append(hb.Flags, s.Key)
			}
		case "-tags", "-gcflags", "-asmflags":
			if s.Value != "" {
				hb.Flags = append(hb.Flags, s.Key+"="+s.Value)
			}
		case "GOAMD64", "GOARM", "GOARM64", "GO386", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM", "GOEXPERIMENT", "CGO_CFLAGS", "CGO_LDFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS":
			hb.Env = append(hb.Env, s.Key+"="+s.Value)
		}
	}
	return hb
}

// Version of wy itself, shown by `wy version`.
const WyVersion = "0.3.1"

// WithGOFLAGS returns the user's GOFLAGS (from the environment) followed by
// extra, so wy's own settings never discard flags the user set.
func WithGOFLAGS(extra ...string) string {
	return strings.TrimSpace(os.Getenv("GOFLAGS") + " " + strings.Join(extra, " "))
}

// LdflagsFromGOFLAGS returns the -ldflags value set in the GOFLAGS
// environment variable, if any.
func LdflagsFromGOFLAGS() string {
	v := ""
	for _, f := range strings.Fields(os.Getenv("GOFLAGS")) {
		if strings.HasPrefix(f, "-ldflags=") {
			v = strings.TrimPrefix(f, "-ldflags=")
		}
	}
	return v
}

// GoDirective returns "1.N" for the go line of generated go.mod files.
func GoDirective() string {
	v := strings.TrimPrefix(strings.Fields(runtime.Version())[0], "go")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}
