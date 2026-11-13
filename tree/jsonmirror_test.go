package tree

import "testing"

// Compiled encoders see the JSON and text methods of interpreted types
// nested in their arguments (nats.go's StreamConfig{Retention RetentionPolicy}).
func TestJSONMirrorNestedMethods(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: "type pol int\n" +
			"func (p pol) MarshalJSON() ([]byte, error) { return json.Marshal([]string{\"limits\", \"new\"}[p]) }\n" +
			"func (p *pol) UnmarshalJSON(b []byte) error { var s string; if err := json.Unmarshal(b, &s); err != nil { return err }; if s == \"new\" { *p = 1 } else { *p = 0 }; return nil }\n" +
			"type level uint8\n" +
			"func (l level) MarshalText() ([]byte, error) { return []byte(fmt.Sprint(\"L\", int(l))), nil }\n" +
			"func (l *level) UnmarshalText(b []byte) error { n, err := strconv.Atoi(strings.TrimPrefix(string(b), \"L\")); *l = level(n); return err }\n" +
			"type sub struct{ P pol `json:\"p\"`; L *level `json:\"l,omitempty\"` }\n" +
			"type cfg struct{ P pol `json:\"p\"`; O pol `json:\"o,omitempty\"`; N int `json:\"n\"`; Subs []sub `json:\"subs\"`; M map[string]pol `json:\"m,omitempty\"`; S *sub `json:\"s,omitempty\"`; Any any `json:\"any,omitempty\"`; hidden pol }"},
		{src: `b, err := json.Marshal(&cfg{P: 1, N: 2}); string(b), err`, want: `{"p":"new","n":2,"subs":null} <nil>`},
		{src: `l := level(3); b, _ := json.Marshal(cfg{Subs: []sub{{P: 1, L: &l}}, M: map[string]pol{"a": 1}, S: &sub{}, Any: sub{P: 1}}); string(b)`,
			want: `{"p":"limits","n":0,"subs":[{"p":"new","l":"L3"}],"m":{"a":"new"},"s":{"p":"limits"},"any":{"p":"new"}}`},
		{src: `c := cfg{N: 9, hidden: 1}; err := json.Unmarshal([]byte("{\"p\":\"new\",\"subs\":[{\"p\":\"new\",\"l\":\"L7\"}],\"m\":{\"k\":\"new\"},\"s\":{\"p\":\"new\"}}"), &c); err, int(c.P), c.N, int(c.hidden), int(c.Subs[0].P), int(*c.Subs[0].L), int(c.M["k"]), int(c.S.P)`,
			want: `<nil> 1 9 1 1 7 1 1`},
		{src: `var p pol; err := json.Unmarshal([]byte("\"new\""), &p); int(p), err`, want: `1 <nil>`},
		{src: `var buf bytes.Buffer; _ = json.NewEncoder(&buf).Encode(sub{P: 1}); var s sub; err := json.NewDecoder(&buf).Decode(&s); strings.TrimSpace(buf.String()), int(s.P), err`, want: ` 1 <nil>`},
		// The arguments are left untouched by encoding: slices keep their identity.
		{src: `subs := []sub{{P: 1}}; c := cfg{Subs: subs}; _, _ = json.Marshal(&c); &c.Subs[0] == &subs[0]`, want: `true`},
	})
}
