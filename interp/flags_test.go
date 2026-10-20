package interp

import (
	"reflect"
	"testing"
)

func TestPluginFlags(t *testing.T) {
	for _, tc := range []struct {
		goos string
		in   []string
		want []string
	}{
		{"linux", []string{"-race"}, []string{"-race"}},
		{"darwin", nil, []string{"-ldflags=-w"}},
		{"darwin", []string{"-race", "-tags=x"}, []string{"-race", "-tags=x", "-ldflags=-w"}},
		{"darwin", []string{"-ldflags=-X main.v=1"}, []string{"-ldflags=-X main.v=1 -w"}},
		{"darwin", []string{"-ldflags", "-s"}, []string{"-ldflags=-s -w"}},
		{"darwin", []string{"-ldflags=-w -s"}, []string{"-ldflags=-w -s"}},
	} {
		if got := pluginFlags(tc.goos, tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("pluginFlags(%s, %q) = %q, want %q", tc.goos, tc.in, got, tc.want)
		}
	}
}
