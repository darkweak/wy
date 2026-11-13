package tree

import "testing"

// encoding/json promotes the exported fields of an embedded unexported
// struct, as nats.go's streamCreateResponse{apiResponse; *StreamInfo} needs.
func TestJSONEmbeddedUnexportedStruct(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: "type apiError struct{ Code int `json:\"code\"` }\ntype apiResponse struct{ Type string `json:\"type\"`; Error *apiError `json:\"error,omitempty\"` }\ntype Info struct{ Name string `json:\"name\"` }\ntype createResponse struct{ apiResponse; *Info }"},
		{src: `var r createResponse; err := json.Unmarshal([]byte("{\"type\":\"t\",\"error\":{\"code\":400}}"), &r); err, r.Type, r.Error.Code, r.Info == nil`, want: "<nil> t 400 true"},
		{src: `var r2 createResponse; _ = json.Unmarshal([]byte("{\"type\":\"t\",\"name\":\"n\"}"), &r2); r2.Error == nil, r2.Name`, want: "true n"},
		{src: `b, _ := json.Marshal(createResponse{apiResponse: apiResponse{Type: "x"}}); string(b)`, want: `{"type":"x"}`},
		// The field keeps its Go name for selectors and composite literals.
		{src: `c := createResponse{apiResponse: apiResponse{Type: "y"}}; c.apiResponse.Type, c.Type`, want: "y y"},
	})
}

func TestPrintEmbeddedUnexportedStruct(t *testing.T) {
	in := New(Options{})
	runEval(t, in, []evalStep{
		{src: "type inner struct{ A int }\ntype outer struct{ inner; B int }"},
		{src: `fmt.Sprintf("%+v", outer{inner{1}, 2})`, want: "{inner:{A:1} B:2}"},
	})
}
