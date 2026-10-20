package source

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"wy/internal/gocmd"
)

// preferred resolves package names that several standard packages share.
var preferred = map[string]string{
	"rand":     "math/rand",
	"template": "text/template",
	"scanner":  "text/scanner",
	"pprof":    "runtime/pprof",
	"trace":    "runtime/trace",
	"metrics":  "runtime/metrics",
	"debug":    "runtime/debug",
	"constant": "go/constant",
	"doc":      "go/doc",
	"build":    "go/build",
}

var (
	stdOnce sync.Once
	stdIdx  map[string]string
	stdAll  map[string]bool
	stdErr  error
)

// Std returns a map from package name to import path for the standard
// library, honouring the preferences above for ambiguous names.
func Std() (byName map[string]string, isStd map[string]bool, err error) {
	stdOnce.Do(func() { stdIdx, stdAll, stdErr = loadStd() })
	return stdIdx, stdAll, stdErr
}

func loadStd() (map[string]string, map[string]bool, error) {
	ver, err := gocmd.Version()
	if err != nil {
		return nil, nil, err
	}
	cache := filepath.Join(gocmd.CacheDir(), "std-"+ver+".txt")
	data, err := os.ReadFile(cache)
	if err != nil {
		data, err = gocmd.Run(context.Background(), "", []string{"GOFLAGS=", "GOWORK=off"},
			"list", "-f", "{{.ImportPath}} {{.Name}}", "std")
		if err != nil {
			return nil, nil, err
		}
		_ = os.MkdirAll(filepath.Dir(cache), 0o755)
		_ = os.WriteFile(cache, data, 0o644)
	}
	byName := map[string]string{}
	all := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		path, name := f[0], f[1]
		all[path] = true
		if strings.Contains(path, "internal") || strings.HasPrefix(path, "vendor/") || name == "main" {
			continue
		}
		if old, ok := byName[name]; !ok || len(path) < len(old) {
			byName[name] = path
		}
	}
	for name, path := range preferred {
		if all[path] {
			byName[name] = path
		}
	}
	return byName, all, nil
}

// IsStd reports whether path is a standard-library import path.
func IsStd(path string) bool {
	_, all, err := Std()
	if err == nil && all != nil {
		return all[path] || path == "C"
	}
	first := strings.SplitN(path, "/", 2)[0]
	return !strings.Contains(first, ".")
}
