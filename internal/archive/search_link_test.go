package archive

import (
	"strings"
	"testing"
)

func TestGuideSearchScriptConfinesLinks(t *testing.T) {
	for _, expected := range []string{
		`!/^[a-z][a-z\d+.-]*:/i.test(row.url)`,
		`href = new URL(row.url, location.href).href`,
		`if (!href || !href.startsWith(new URL(".", location.href).href)) continue;`,
	} {
		if !strings.Contains(guideSearchJS, expected) {
			t.Errorf("guide search script does not confine links: missing %q", expected)
		}
	}
}
