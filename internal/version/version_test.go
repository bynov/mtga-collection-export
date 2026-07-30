package version

import "testing"

func TestStringIsSet(t *testing.T) {
	if String == "" {
		t.Fatal("version.String must not be empty")
	}
}
