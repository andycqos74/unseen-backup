package server

import "testing"

func TestKopiaSourceNames(t *testing.T) {
	cases := map[string]string{
		"/home/u/docs/":  "agent@a1:/home/u/docs",
		"/":              "agent@a1:/",
		`C:\Data\`:       `agent@a1:C:\Data`,
		`C:\`:            `agent@a1:C:\`,
		"  /srv/app//  ": "agent@a1:/srv/app",
	}
	for in, want := range cases {
		if got := sourceFor("a1", in); got != want {
			t.Errorf("sourceFor(%q) = %q, want %q", in, got, want)
		}
	}
	if !validPlatformWord("amd64") || validPlatformWord("../x") || validPlatformWord("") {
		t.Error("validPlatformWord")
	}
	if _, err := decodeKopiaKey("blobs/x"); err == nil {
		t.Error("non-kopia key accepted")
	}
}
