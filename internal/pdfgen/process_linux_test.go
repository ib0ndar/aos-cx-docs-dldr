package pdfgen

import (
	"syscall"
	"testing"
)

func TestSidecarProcessAttributesKillOnParentDeath(t *testing.T) {
	attributes := sidecarProcessAttributes()
	if !attributes.Setpgid || attributes.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("Linux sidecar attributes = %+v, want own process group and SIGKILL on parent death", attributes)
	}
}
