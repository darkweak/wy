package interp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"

	"wy/bridge"
	"wy/internal/gocmd"
	"wy/internal/source"
)

// bridgePath is the import path of the bridge package, discovered rather
// than hard-coded so the module can be renamed.
var bridgePath = reflect.TypeOf(bridge.Marker{}).PkgPath()

func wyModulePath() string { return strings.TrimSuffix(bridgePath, "/bridge") }

// wyRoot finds the directory holding wy's own module, which interpreted
// code needs in order to link against the very same bridge package.
func wyRoot(opt string) (string, error) {
	if opt != "" {
		return opt, nil
	}
	if d := os.Getenv("WY_ROOT"); d != "" {
		return d, nil
	}
	_, file, _, ok := runtime.Caller(0)
	if ok && filepath.IsAbs(file) {
		root := filepath.Dir(filepath.Dir(file))
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root, nil
		}
	}
	return "", nil // resolved through the module cache using the host's version
}

// workspace is the on-disk Go module in which inputs are compiled.
type workspace struct {
	dir    string
	module string
	host   gocmd.HostBuild
	flags  []string
	log    io.Writer
}

func newWorkspace(opts Options) (*workspace, error) {
	var id [5]byte
	_, _ = rand.Read(id[:])
	ws := &workspace{
		module: "wysess_" + hex.EncodeToString(id[:]),
		host:   gocmd.Host(),
		flags:  opts.BuildFlags,
		log:    opts.Log,
	}
	dir := opts.Dir
	if dir == "" {
		d, err := os.MkdirTemp("", "wy-session-")
		if err != nil {
			return nil, err
		}
		dir = d
	}
	ws.dir = dir
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return nil, err
	}
	gomod, err := ws.goMod(opts)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		return nil, err
	}
	return ws, nil
}

// goMod writes a go.mod that pins wy itself and every dependency of the
// host binary to the exact versions linked into the host, so that packages
// shared between host and interpreted code are identical and the plugin
// loader accepts them.
func (ws *workspace) goMod(opts Options) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "module %s\n\ngo %s\n\n", ws.module, gocmd.GoDirective())
	wyMod := wyModulePath()
	root, err := wyRoot(opts.WyRoot)
	if err != nil {
		return "", err
	}
	bi := ws.host.Info
	required := map[string]bool{wyMod: true}
	switch {
	case root != "":
		fmt.Fprintf(&b, "require %s v0.0.0-00010101000000-000000000000\nreplace %s => %s\n", wyMod, wyMod, root)
	default:
		ver := ""
		if bi != nil {
			for _, d := range bi.Deps {
				if d.Path == wyMod {
					ver = d.Version
				}
			}
		}
		if ver == "" || ver == "(devel)" {
			return "", errors.New("interp: cannot locate wy's source; set Options.WyRoot or WY_ROOT")
		}
		fmt.Fprintf(&b, "require %s %s\n", wyMod, ver)
	}
	if bi != nil {
		for _, d := range bi.Deps {
			if required[d.Path] {
				continue
			}
			required[d.Path] = true
			r := d
			if d.Replace != nil {
				if d.Replace.Version == "" && !filepath.IsAbs(d.Replace.Path) {
					continue // relative local replacement: not reproducible from here
				}
				fmt.Fprintf(&b, "require %s %s\n", d.Path, nonEmpty(d.Version, "v0.0.0-00010101000000-000000000000"))
				fmt.Fprintf(&b, "replace %s => %s %s\n", d.Path, d.Replace.Path, d.Replace.Version)
				continue
			}
			fmt.Fprintf(&b, "require %s %s\n", r.Path, r.Version)
		}
		if opts.HostModuleDir != "" && bi.Main.Path != "" && !required[bi.Main.Path] {
			fmt.Fprintf(&b, "require %s v0.0.0-00010101000000-000000000000\nreplace %s => %s\n", bi.Main.Path, bi.Main.Path, opts.HostModuleDir)
		}
	}
	return b.String(), nil
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (ws *workspace) run(ctx context.Context, args ...string) ([]byte, error) {
	if ws.log != nil {
		fmt.Fprintf(ws.log, "wy: go %s\n", strings.Join(args, " "))
	}
	return gocmd.Run(ctx, ws.dir, ws.host.Env, args...)
}

// buildPlugin compiles ./<pkgDir> (a main package) as a plugin.
func (ws *workspace) buildPlugin(ctx context.Context, pkgDir, out string) error {
	args := []string{"build", "-buildmode=plugin", "-o", out}
	var flags []string
	// -ldflags on the command line would replace one given in GOFLAGS, so
	// the GOFLAGS value is carried over and merged.
	if ld := gocmd.LdflagsFromGOFLAGS(); ld != "" {
		flags = append(flags, "-ldflags="+ld)
	}
	flags = append(flags, ws.host.Flags...)
	flags = append(flags, ws.flags...)
	args = append(args, pluginFlags(runtime.GOOS, flags)...)
	args = append(args, "./"+pkgDir)
	_, err := ws.run(ctx, args...)
	return err
}

// pluginFlags adjusts go build flags for -buildmode=plugin on goos.
//
// On macOS, the Go linker appends a __DWARF segment to plugins without
// updating the segment count recorded in LC_DYLD_CHAINED_FIXUPS, and recent
// versions of dyld refuse to load the result ("chained fixups, seg_count
// does not match number of segments", golang.org/issue/81793). Plugins are
// therefore linked without DWARF (-w), which only drops debug information;
// it does not affect the package hashes the plugin loader checks. A -w is
// merged into any -ldflags the user gave.
func pluginFlags(goos string, flags []string) []string {
	if goos != "darwin" {
		return flags
	}
	out := make([]string, 0, len(flags)+1)
	merged := false
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		switch {
		case strings.HasPrefix(f, "-ldflags="):
			out = append(out, "-ldflags="+withW(strings.TrimPrefix(f, "-ldflags=")))
			merged = true
		case f == "-ldflags" && i+1 < len(flags):
			out = append(out, "-ldflags="+withW(flags[i+1]))
			i++
			merged = true
		default:
			out = append(out, f)
		}
	}
	if !merged {
		out = append(out, "-ldflags=-w")
	}
	return out
}

// withW adds -w to a linker flag string unless it is already there.
func withW(ldflags string) string {
	for _, f := range strings.Fields(ldflags) {
		if f == "-w" || f == "-w=true" || f == "-w=1" {
			return ldflags
		}
	}
	return strings.TrimSpace(ldflags + " -w")
}

// importer type-checks against compiler export data obtained through
// `go list -export`, which works for the standard library, the host's
// dependencies and any module the session pulls in.
type pkgImporter struct {
	ws      *workspace
	mu      sync.Mutex
	exports map[string]string
	gc      types.ImporterFrom
}

func newImporter(ws *workspace, fset *token.FileSet) *pkgImporter {
	im := &pkgImporter{ws: ws, exports: map[string]string{}}
	im.gc = importer.ForCompiler(fset, "gc", im.lookup).(types.ImporterFrom)
	return im
}

func (im *pkgImporter) Import(path string) (*types.Package, error) {
	return im.ImportFrom(path, "", 0)
}

func (im *pkgImporter) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if path == "C" {
		return nil, errors.New(`cgo (import "C") is not supported in interactive inputs; use script mode`)
	}
	if err := im.ensure(path); err != nil {
		return nil, err
	}
	return im.gc.ImportFrom(path, dir, mode)
}

func (im *pkgImporter) lookup(path string) (io.ReadCloser, error) {
	if err := im.ensure(path); err != nil {
		return nil, err
	}
	im.mu.Lock()
	file := im.exports[path]
	im.mu.Unlock()
	if file == "" {
		return nil, fmt.Errorf("no export data for %s", path)
	}
	return os.Open(file)
}

func (im *pkgImporter) ensure(path string) error {
	im.mu.Lock()
	_, ok := im.exports[path]
	im.mu.Unlock()
	if ok {
		return nil
	}
	err := im.load(path)
	if err != nil && !source.IsStd(path) {
		// A module nobody required yet: fetch it and try again.
		if _, gerr := im.ws.run(context.Background(), "get", path); gerr != nil {
			return fmt.Errorf("could not import %s: %w", path, gerr)
		}
		err = im.load(path)
	}
	return err
}

func (im *pkgImporter) load(paths ...string) error {
	args := append([]string{"list", "-e", "-export", "-deps", "-json=ImportPath,Export,Error"}, paths...)
	out, err := im.ws.run(context.Background(), args...)
	if err != nil {
		return err
	}
	type pkg struct {
		ImportPath string
		Export     string
		Error      *struct{ Err string }
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var firstErr error
	im.mu.Lock()
	defer im.mu.Unlock()
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			return err
		}
		if p.Error != nil {
			for _, want := range paths {
				if p.ImportPath == want && firstErr == nil {
					firstErr = errors.New(strings.TrimSpace(p.Error.Err))
				}
			}
			continue
		}
		if p.Export != "" {
			im.exports[p.ImportPath] = p.Export
		}
	}
	return firstErr
}
