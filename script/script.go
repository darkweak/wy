package script

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/darkweak/wy/internal/gocmd"
)

// Options control how a program is built and run.
type Options struct {
	Stdin          io.Reader // defaults to os.Stdin
	Stdout, Stderr io.Writer // default to os.Stdout / os.Stderr
	Env            []string  // extra environment for the program
	BuildFlags     []string  // extra `go build` flags (e.g. -race)
	Rebuild        bool      // ignore the binary cache
	// Standalone disables module detection: the program is built outside any
	// enclosing go.mod even if the file lives inside a module.
	Standalone bool
}

// Build compiles src (a full program or a snippet) and returns the path of
// the executable. filename is used for error positions and to detect an
// enclosing Go module; it may be empty or a pseudo-name such as "-e".
//
// Standalone programs are cached by content hash: a second Build of the same
// source returns immediately. Programs that import packages of the module
// they live in are built inside that module (through `go build -overlay`, so
// nothing is written there) and rely on the go build cache instead.
func Build(ctx context.Context, src []byte, filename string, opts Options) (string, error) {
	ver, err := gocmd.Version()
	if err != nil {
		return "", err
	}
	abs := filename
	if filename != "" && !strings.HasPrefix(filename, "-") {
		if a, err := filepath.Abs(filename); err == nil {
			abs = a
		}
	}
	// The name as given is part of the key because it is compiled into the
	// binary (through //line directives) for stack traces.
	key := hashKey(ver, abs+"\x00"+filename, src, opts.BuildFlags)
	cacheRoot := filepath.Join(gocmd.CacheDir(), "scripts")

	var modRoot string
	if !opts.Standalone && filepath.IsAbs(abs) {
		modRoot = findModule(filepath.Dir(abs))
	}

	// Fast path: a standalone binary built earlier from identical source.
	// (Whether a build is standalone depends only on the hashed source, and
	// module builds are cached elsewhere, so this is valid in a module too.)
	work := filepath.Join(cacheRoot, key)
	bin := filepath.Join(work, "prog"+exeSuffix())
	if !opts.Rebuild {
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}

	prep, err := Prepare(string(src), filename)
	if err != nil {
		return "", err
	}
	if modRoot != "" && len(prep.NonStd) > 0 {
		return buildInModule(ctx, prep, modRoot, key, opts)
	}
	return buildStandalone(ctx, prep, work, bin, opts)
}

func buildStandalone(ctx context.Context, prep *Prepared, work, bin string, opts Options) (string, error) {
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(work, "main.go"), prep.Code, 0o644); err != nil {
		return "", err
	}
	gomod := filepath.Join(work, "go.mod")
	if _, err := os.Stat(gomod); err != nil {
		mod := "module wyscript\n\ngo " + gocmd.GoDirective() + "\n"
		if err := os.WriteFile(gomod, []byte(mod), 0o644); err != nil {
			return "", err
		}
	}
	env := []string{"GOWORK=off", "GOFLAGS=" + gocmd.WithGOFLAGS("-mod=mod")}
	if len(prep.NonStd) > 0 {
		// Resolve third-party imports to their latest versions (once: the
		// resulting go.mod/go.sum stay in the cache with the binary).
		if _, err := gocmd.Run(ctx, work, env, "mod", "tidy"); err != nil {
			return "", fmt.Errorf("resolving dependencies: %w", err)
		}
	}
	tmp := fmt.Sprintf("%s.%d.tmp", bin, os.Getpid())
	// Naming the file (rather than the package) makes go ignore its build
	// constraints, as `go run file.go` does: scripts often carry
	// "//go:build ignore" so that `go build ./...` skips them.
	args := append([]string{"build", "-o", tmp}, opts.BuildFlags...)
	args = append(args, "main.go")
	if _, err := gocmd.Run(ctx, work, env, args...); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, bin); err != nil {
		return "", err
	}
	return bin, nil
}

func buildInModule(ctx context.Context, prep *Prepared, modRoot, key string, opts Options) (string, error) {
	work := filepath.Join(gocmd.CacheDir(), "modscripts", key)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	gen := filepath.Join(work, "main.go")
	if err := os.WriteFile(gen, prep.Code, 0o644); err != nil {
		return "", err
	}
	virtDir := ".wy-" + key[:16]
	overlay := map[string]map[string]string{"Replace": {filepath.Join(modRoot, virtDir, "main.go"): gen}}
	ov, _ := json.Marshal(overlay)
	ovPath := filepath.Join(work, "overlay.json")
	if err := os.WriteFile(ovPath, ov, 0o644); err != nil {
		return "", err
	}
	bin := filepath.Join(work, "prog"+exeSuffix())
	args := append([]string{"build", "-overlay", ovPath, "-o", bin}, opts.BuildFlags...)
	args = append(args, filepath.Join(modRoot, virtDir, "main.go"))
	if _, err := gocmd.Run(ctx, modRoot, nil, args...); err != nil {
		return "", err
	}
	return bin, nil
}

// Run builds src and runs it with args, returning the program's exit code.
func Run(ctx context.Context, src []byte, filename string, args []string, opts Options) (int, error) {
	bin, err := Build(ctx, src, filename, opts)
	if err != nil {
		return -1, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = or(opts.Stdin, os.Stdin), orW(opts.Stdout, os.Stdout), orW(opts.Stderr, os.Stderr)
	cmd.Env = append(os.Environ(), opts.Env...)
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// CleanCache removes all cached script builds.
func CleanCache() error {
	for _, d := range []string{"scripts", "modscripts", "dirs"} {
		if err := os.RemoveAll(filepath.Join(gocmd.CacheDir(), d)); err != nil {
			return err
		}
	}
	return nil
}

func hashKey(ver, name string, src []byte, flags []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "wy-script-v1\x00%s\x00%s/%s\x00%s\x00%s\x00", ver, runtime.GOOS, runtime.GOARCH, name, strings.Join(flags, " "))
	h.Write(src)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func findModule(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func or(r, def io.Reader) io.Reader {
	if r != nil {
		return r
	}
	return def
}

func orW(w, def io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return def
}

// BuildDir compiles the main package in dir (inside its own module, if any)
// and returns the executable's path. The go build cache keeps rebuilds fast.
func BuildDir(ctx context.Context, dir string, opts Options) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(abs))
	work := filepath.Join(gocmd.CacheDir(), "dirs", hex.EncodeToString(h[:])[:32])
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	bin := filepath.Join(work, filepath.Base(abs)+exeSuffix())
	args := append([]string{"build", "-o", bin}, opts.BuildFlags...)
	args = append(args, ".")
	if _, err := gocmd.Run(ctx, abs, nil, args...); err != nil {
		return "", err
	}
	return bin, nil
}
