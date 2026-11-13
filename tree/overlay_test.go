package tree

import (
	"crypto/hkdf"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The generic functions of standard packages that wy registers natively,
// provided by the sources of tree/stdsrc/overlay.
func TestGenericStdlibOverlays(t *testing.T) {
	secret, salt := []byte("0123456789abcdef"), []byte("salt")
	okm, _ := hkdf.Key(sha256.New, secret, salt, "info", 42)
	prk, _ := hkdf.Extract(sha256.New, secret, salt)
	dk, _ := pbkdf2.Key(sha256.New, "password", salt, 4096, 32)
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: `import (
	"crypto/hkdf"
	"crypto/pbkdf2"
	"database/sql"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"hash/maphash"
	rand "math/rand/v2"
)`},
		{src: `secret, salt := []byte("0123456789abcdef"), []byte("salt")`},
		{src: `k, err := hkdf.Key(sha256.New, secret, salt, "info", 42); hex.EncodeToString(k), err`, want: hex.EncodeToString(okm) + " <nil>"},
		{src: `prk, err := hkdf.Extract(sha256.New, secret, salt); hex.EncodeToString(prk), err`, want: hex.EncodeToString(prk) + " <nil>"},
		{src: `k, err := hkdf.Expand(sha256.New, prk, "info", 42); hex.EncodeToString(k), err`, want: hex.EncodeToString(okm) + " <nil>"},
		{src: `_, err := hkdf.Expand(sha256.New, prk, "info", 255*32+1); err`, want: "hkdf: requested key length too large"},
		{src: `k, err := pbkdf2.Key(sha256.New, "password", salt, 4096, 32); hex.EncodeToString(k), err`, want: hex.EncodeToString(dk) + " <nil>"},
		{src: `_, err := pbkdf2.Key(sha256.New, "password", salt, 1, 0); err`, want: "pbkdf2: keyLength must be larger than 0"},

		{src: `var n sql.Null[int64]; err := n.Scan("42"); n.V, n.Valid, err`, want: "42 true <nil>"},
		{src: `v, err := n.Value(); v, err`, want: "42 <nil>"},
		{src: `err := n.Scan(nil); n.V, n.Valid, err`, want: "0 false <nil>"},
		{src: `var s sql.Null[string]; err := s.Scan(3.5); s.V, s.Valid, err`, want: "3.5 true <nil>"},

		{src: `x := rand.N(10); x >= 0 && x < 10`, want: "true"},
		{src: `d := rand.N(time.Duration(5)); d >= 0 && d < 5`, want: "true"},
		{src: `rand.N(0)`, wantErr: "invalid argument to N"},

		{src: `type pt struct{ X, Y int }`},
		{src: `seed := maphash.MakeSeed()`},
		{src: `maphash.Comparable(seed, "a") == maphash.Comparable(seed, "a"), maphash.Comparable(seed, pt{1, 2}) == maphash.Comparable(seed, pt{1, 2})`, want: "true true"},
		{src: `var h1, h2 maphash.Hash; h1.SetSeed(seed); h2.SetSeed(seed); maphash.WriteComparable(&h1, pt{3, 4}); maphash.WriteComparable(&h2, pt{3, 4}); h1.Sum64() == h2.Sum64()`, want: "true"},
		{src: `var hs maphash.Hasher[string] = maphash.ComparableHasher[string]{}; hs.Equal("a", "a"), hs.Equal("a", "b")`, want: "true false"},

		{src: `p := new([64]byte); c := runtime.AddCleanup(p, func(int) {}, 1); c.Stop(); p != nil`, want: "true"},
		{src: `runtime.AddCleanup((*int)(nil), func(int) {}, 1)`, wantErr: "ptr is nil"},
		{src: `q := new(int); runtime.AddCleanup(q, func(*int) {}, q)`, wantErr: "ptr is equal to arg"},

		{src: `b, err := jsontext.AppendQuote([]byte("x="), "a\"b"); string(b), err`, want: `x="a\"b" <nil>`},
		{src: `b, err := jsontext.AppendUnquote(nil, []byte("\"a\\nb\"")); string(b), err`, want: "a\nb <nil>"},
		{src: `b, err := jsontext.AppendFormat(nil, "{ \"a\" : 1 }"); string(b), err`, want: `{"a":1} <nil>`},

		{src: `m := json.MarshalFunc(func(p pt) ([]byte, error) { return []byte(fmt.Sprint(p.X + p.Y)), nil })`},
		{src: `b, err := json.Marshal([]any{pt{1, 2}, "s"}, json.WithMarshalers(m)); string(b), err`, want: `[3,"s"] <nil>`},
		{src: `b, err := json.Marshal(pt{1, 2}, json.WithMarshalers(m)); string(b), err`, want: `3 <nil>`},
		// Types with the same structure are told apart in interfaces.
		{src: `type pt2 struct{ X, Y int }`},
		{src: `b, err := json.Marshal([]any{pt{1, 2}, pt2{1, 2}}, json.WithMarshalers(m)); string(b), err`, want: `[3,{"X":1,"Y":2}] <nil>`},
		{src: `mt := json.MarshalToFunc(func(enc *jsontext.Encoder, s fmt.Stringer) error { return enc.WriteToken(jsontext.String("S:" + s.String())) })`},
		{src: `b, err := json.Marshal(map[string]any{"d": time.Second, "n": 1}, json.WithMarshalers(mt), json.Deterministic(true)); string(b), err`, want: `{"d":"S:1s","n":1} <nil>`},
		{src: `u := json.UnmarshalFunc(func(b []byte, p *pt) error { p.X = len(b); return nil })`},
		{src: `var v pt; err := json.Unmarshal([]byte("[1, 2]"), &v, json.WithUnmarshalers(u)); v.X, err`, want: "6 <nil>"},
		{src: `uf := json.UnmarshalFromFunc(func(dec *jsontext.Decoder, p *pt) error { _, err := dec.ReadValue(); p.Y = 9; return err })`},
		{src: `var vs []pt; err := json.Unmarshal([]byte("[{}, 2]"), &vs, json.WithUnmarshalers(uf)); vs, err`, want: "[{0 9} {0 9}] <nil>"},
		{src: `on, ok := json.GetOption(json.StringifyNumbers(true), json.StringifyNumbers); on, ok`, want: "true true"},
		{src: `_, ok := json.GetOption(json.DefaultOptionsV2(), json.WithMarshalers); ok`, want: "false"},
	})
}
