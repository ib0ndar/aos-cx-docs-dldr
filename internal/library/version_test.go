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
	unsupported := []string{
		"", "0", "0.4", "0.5", "0.6", "0.7", "0.9", "0.10", "1.0", "9.9", "00.6", "0.06", "v0.6", "0.6.0",
		"1.0.1", "1.1.0", "2.0.0", "1.0.0.0", "01.0.0", "1.00.0", "1.0.00", "1..0", "1.0.", "1.0.0-rc1",
	}
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

func TestFormerTwoComponentVersionsAreRecognisedAsOtherVersions(t *testing.T) {
	// Libraries written before 1.0.0 record two-component versions. They must
	// be refused as another version, with the actionable message, rather than
	// as malformed state.
	for _, value := range []string{"0.9", "0.10"} {
		err := supportedNativeApplicationVersion(value)
		if err == nil || !strings.Contains(err.Error(), "is unsupported; this application reads and writes only version "+model.Version) {
			t.Fatalf("former version %s was not refused as another version: %v", value, err)
		}
	}
	if strings.Count(model.Version, ".") != 2 {
		t.Fatalf("application version %s is not major.minor.patch", model.Version)
	}
	if _, err := makeNativeVersionPolicy("1.0"); err == nil {
		t.Fatal("a two-component output version was accepted")
	}
	ordered := []string{"0.9", "0.10", "1.0.0", "1.0.1", "1.1.0", "2.0.0"}
	for index := 1; index < len(ordered); index++ {
		previous, err := parseApplicationVersion(ordered[index-1])
		if err != nil {
			t.Fatal(err)
		}
		current, err := parseApplicationVersion(ordered[index])
		if err != nil {
			t.Fatal(err)
		}
		if !applicationVersionBefore(previous, current) || applicationVersionBefore(current, previous) {
			t.Fatalf("%s does not order before %s", ordered[index-1], ordered[index])
		}
	}
}
