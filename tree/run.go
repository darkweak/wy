package tree

import (
	"context"
	"os"
	"path/filepath"

	"github.com/darkweak/wy/script"
)

// RunSource interprets a program or snippet (any mix of imports,
// declarations and statements; see github.com/darkweak/wy/script.Prepare), with args as
// os.Args[1:].
func (in *Interp) RunSource(ctx context.Context, src []byte, filename string, args []string) error {
	prep, err := script.PrepareWith(string(src), filename, stdIndex())
	if err != nil {
		return err
	}
	if in.modRoot == "" && filename != "" && !filepath.IsAbs(filename) {
		if abs, err := filepath.Abs(filename); err == nil {
			in.findModule(filepath.Dir(abs))
		}
	}
	os.Args = append([]string{filename}, args...)
	return in.RunFiles(ctx, map[string][]byte{filename: prep.Code})
}
