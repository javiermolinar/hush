package keyring

import (
	"bytes"
	"strings"
	"testing"
)

func TestMalformedRequestsAreSilentAndDoNotAccessKeyring(t *testing.T) {
	scope := strings.Repeat("0", 64)
	for _, input := range []string{
		`{"operation":"get","scope":"not a scope"}`,
		`{"operation":"list","scope":"` + scope + `"}`,
		`{"operation":"get","scope":"` + scope + `","record":null}`,
		`{"operation":"set","scope":"` + scope + `","record":"not an object"}`,
		`{"operation":"delete","scope":"` + scope + `"} {}`,
		`{"operation":"delete","scope":"` + scope + `","account":"not Hush"}`,
		strings.Repeat(" ", maxCredential+4000),
	} {
		var out bytes.Buffer
		if code := Run(strings.NewReader(input), &out); code != 2 || out.Len() != 0 {
			t.Fatalf("invalid request returned status %d with %d bytes", code, out.Len())
		}
	}
}
