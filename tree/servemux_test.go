package tree

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A restricted interpreter registers handlers on its own default mux, so
// that a package registering the same pattern as the host does not panic.
func TestRestrictedServeMux(t *testing.T) {
	const pattern = "/wy-test-debug"
	http.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	in := New(Options{Restricted: true})
	vals, err := in.Eval(context.Background(), `http.HandleFunc("`+pattern+`", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("interp")) })
http.DefaultServeMux`)
	if err != nil {
		t.Fatal(err)
	}
	mux := vals[0].(*http.ServeMux)
	if mux == http.DefaultServeMux {
		t.Fatal("restricted interpreter uses the host's DefaultServeMux")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", pattern, nil))
	if rec.Body.String() != "interp" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
