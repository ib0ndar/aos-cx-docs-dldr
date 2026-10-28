package library

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"aos-cx-docs-dldr/internal/model"
)

// applicationVersion is a numeric major.minor.patch application version.
// Versions up to 0.10 had two components; they parse with patch 0 so that
// libraries recording them are recognised, and refused, as other versions.
type applicationVersion struct {
	major int
	minor int
	patch int
}

func applicationVersionBefore(first, second applicationVersion) bool {
	if first.major != second.major {
		return first.major < second.major
	}
	if first.minor != second.minor {
		return first.minor < second.minor
	}
	return first.patch < second.patch
}

// nativeVersionPolicy names the single application version this build writes
// and will reopen. Libraries created by any other version are preserved
// read-only and require a new destination; this application does not upgrade
// them. Manifests may still record an `upgraded_from` version produced by an
// earlier build, and that provenance is validated but never acted on.
type nativeVersionPolicy struct {
	output   string
	accepted map[string]bool
}

// parseApplicationVersion accepts a three-component version or a former
// two-component one.
func parseApplicationVersion(value string) (applicationVersion, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return applicationVersion{}, errors.New("application version must use three numeric components")
	}
	var components [3]int
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 || strconv.Itoa(number) != part {
			return applicationVersion{}, fmt.Errorf(
				"invalid application version %s component", [...]string{"major", "minor", "patch"}[index],
			)
		}
		components[index] = number
	}
	return applicationVersion{major: components[0], minor: components[1], patch: components[2]}, nil
}

func defaultNativeVersionPolicy() (nativeVersionPolicy, error) {
	return makeNativeVersionPolicy(model.Version)
}

func makeNativeVersionPolicy(output string) (nativeVersionPolicy, error) {
	if _, err := parseApplicationVersion(output); err != nil {
		return nativeVersionPolicy{}, fmt.Errorf("invalid native output version: %w", err)
	}
	if strings.Count(output, ".") != 2 {
		return nativeVersionPolicy{}, errors.New("invalid native output version: application version must use three numeric components")
	}
	return nativeVersionPolicy{output: output, accepted: map[string]bool{output: true}}, nil
}

func (p nativeVersionPolicy) supports(value string) error {
	if _, err := parseApplicationVersion(value); err != nil {
		return err
	}
	if p.accepted[value] {
		return nil
	}
	return fmt.Errorf(
		"application version %q is unsupported; this application reads and writes only version %s libraries",
		value, p.output,
	)
}

func supportedNativeApplicationVersion(value string) error {
	policy, err := defaultNativeVersionPolicy()
	if err != nil {
		return fmt.Errorf("invalid current application version: %w", err)
	}
	return policy.supports(value)
}
