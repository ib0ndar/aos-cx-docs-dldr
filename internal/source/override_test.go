package source

import (
	"strings"
	"testing"
)

func TestGuideSourceOverrideReplacesOneMapping(t *testing.T) {
	c := peerCatalog()
	hpe := "https://support.hpe.com/hpesc/public/docDisplay?docId=sd00000001en_us"
	note, err := OverrideGuideSource(&c, "copp", "8325", "10.16", hpe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "AOS-CX/10.16/HTML/acopp_8100-83xx-9300-10000/Content/home.htm") || !strings.Contains(note, hpe) {
		t.Fatalf("note %q", note)
	}
	d := mappedDocument(t, c, "8325", "10.16")
	if d.URL != hpe || d.Kind != "hpe" || d.RouteOrigin != RouteOriginUserSupplied {
		t.Fatalf("overridden document = %+v", d)
	}
	if other := mappedDocument(t, c, "8320", "10.16"); other.RouteOrigin == RouteOriginUserSupplied {
		t.Fatalf("override leaked to another platform: %+v", other)
	}
	if _, err := OverrideGuideSource(&c, "copp", "8325", "10.16", hpe); err == nil {
		t.Fatal("second override of one guide accepted")
	}
	same := "https://example.test/techdocs/AOS-CX/10.16/PDF/copp_8100-83xx-9300-10000.pdf"
	if _, err := OverrideGuideSource(&c, "copp", "8320", "10.16", same); err != nil {
		t.Fatalf("documentation-host PDF refused: %v", err)
	}
	for name, tc := range map[string]struct{ id, platform, version, raw string }{
		"unknown guide":    {"vsx", "8325", "10.16", hpe},
		"unmapped release": {"copp", "8325", "10.18", hpe},
		"foreign host":     {"copp", "8400", "10.16", "https://mirror.example/AOS-CX/copp.pdf"},
		"folder name":      {"copp", "8400", "10.16", "copp_8400"},
		"HPE search page":  {"copp", "8400", "10.16", "https://support.hpe.com/hpesc/public/km/search"},
	} {
		c := c
		if _, err := OverrideGuideSource(&c, tc.id, tc.platform, tc.version, tc.raw); err == nil {
			t.Errorf("%s: override accepted", name)
		}
	}
}
