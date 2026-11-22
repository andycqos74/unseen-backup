package kopia

import "testing"

func TestDecodeSnapshots(t *testing.T) {
	obj := `{"id":"a1","source":{"host":"h","userName":"u","path":"/x"},"rootEntry":{"obj":"k1","summ":{"size":5,"files":2,"numFailed":1,"errors":[{"path":"bad","error":"denied"}]}}}`
	for name, in := range map[string]string{
		"single":   obj,
		"stream":   obj + "\n" + `{"id":"a2","rootEntry":{"obj":"k2"}}`,
		"array":    "[" + obj + `,{"id":"a2","rootEntry":{"obj":"k2"}}]`,
		"trailing": obj + "\n\n",
	} {
		got, err := DecodeSnapshots([]byte(in))
		if err != nil || len(got) == 0 || got[0].ID != "a1" || got[0].RootEntry.Summ.NumFailed != 1 ||
			got[0].RootEntry.Summ.Errors[0].Path != "bad" || got[0].Source.Path != "/x" {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	if got, err := DecodeSnapshots(nil); err != nil || got != nil {
		t.Errorf("empty: %v %v", got, err)
	}
}
