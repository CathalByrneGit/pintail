package version

import (
	"strings"
	"testing"
)

// The label appears in every screen header, so an empty one leaves a blank there.
func TestLabel(t *testing.T) {
	got := Label()
	if !strings.HasPrefix(got, "v") {
		t.Errorf("Label() = %q, want a leading v", got)
	}
	if len(got) < 2 {
		t.Errorf("Label() = %q, want a version after the v", got)
	}
}
