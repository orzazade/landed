package decide

import "testing"

func TestParseVerify(t *testing.T) {
	items := []VerifyItem{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got, err := parseVerify("```json\n"+`[{"id":"a","records":true,"quote":"spout stays ceramic","why":"x"},{"id":"b","records":false,"quote":"","why":"y"},{"id":"c","quote":"no records field"}]`+"\n```", items)
	if err == nil || len(got) != 2 || !got[0].Records || got[0].Quote != "spout stays ceramic" || got[1].Records {
		t.Errorf("got %+v, err %v; want a and b, and an error for c", got, err)
	}
}

func TestParseRestate(t *testing.T) {
	items := []RestateItem{{ID: "a", Earlier: []string{"0", "1"}}, {ID: "b", Earlier: []string{"0"}}, {ID: "c", Earlier: []string{"0"}}}
	got, err := parseRestate(`[{"id":"a","restates":1,"why":"same rule"},{"id":"b","restates":-1},{"id":"c","restates":3}]`, items)
	if err == nil || len(got) != 2 || got[0].Restates != 1 || got[1].Restates != -1 {
		t.Errorf("got %+v, err %v; want a and b, c refused (no earlier 3)", got, err)
	}
}
