package library

import (
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

func TestNativeApplicationVersionCompatibility(t *testing.T) {
	if err := supportedNativeApplicationVersion(model.Version); err != nil {
		t.Fatalf("current version %s rejected: %v", model.Version, err)
	}
	// This application reads and writes exactly one library version. Earlier
	// versions are preserved read-only rather than upgraded.
	unsupported := []string{"", "0", "0.4", "0.5", "0.6", "0.7", "1.0", "9.9", "00.6", "0.06", "v0.6", "0.6.0"}
	for _, value := range unsupported {
		if value == model.Version {
			t.Fatalf("unsupported-version fixture %q is actually current", value)
		}
		if err := supportedNativeApplicationVersion(value); err == nil {
			t.Fatalf("unsupported version %q accepted", value)
		}
	}
	if _, err := parseApplicationVersion(model.Version); err != nil {
		t.Fatal(err)
	}
	future := futureApplicationVersion(t)
	if err := supportedNativeApplicationVersion(future); err == nil ||
		!strings.Contains(err.Error(), "only version "+model.Version+" libraries") {
		t.Fatalf("future-version error is not actionable: %v", err)
	}
	if slices.Contains(unsupported, model.Version) {
		t.Fatal("fixture list contains the current version")
	}
}
