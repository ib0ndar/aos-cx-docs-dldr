package library

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"aos-cx-docs-dldr/internal/model"
)

type applicationVersion struct {
	major int
	minor int
}

func applicationVersionBefore(first, second applicationVersion) bool {
	return first.major < second.major ||
		(first.major == second.major && first.minor < second.minor)
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

func parseApplicationVersion(value string) (applicationVersion, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return applicationVersion{}, errors.New("application version must use two numeric components")
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 || strconv.Itoa(major) != parts[0] {
		return applicationVersion{}, errors.New("invalid application version major component")
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor < 0 || strconv.Itoa(minor) != parts[1] {
		return applicationVersion{}, errors.New("invalid application version minor component")
	}
	return applicationVersion{major: major, minor: minor}, nil
}

func defaultNativeVersionPolicy() (nativeVersionPolicy, error) {
	return makeNativeVersionPolicy(model.Version)
}

func makeNativeVersionPolicy(output string) (nativeVersionPolicy, error) {
	if _, err := parseApplicationVersion(output); err != nil {
		return nativeVersionPolicy{}, fmt.Errorf("invalid native output version: %w", err)
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
