package cli

import (
	"fmt"
	"strings"
	"unicode"
)

func safeDisplayText(value string) string {
	var output strings.Builder
	for _, char := range value {
		if !unicode.In(char, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			output.WriteRune(char)
			continue
		}
		if char <= 0xffff {
			fmt.Fprintf(&output, `\u%04X`, char)
		} else {
			fmt.Fprintf(&output, `\U%08X`, char)
		}
	}
	return output.String()
}
