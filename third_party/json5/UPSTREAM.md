# Local JSON5 decoder fork

Upstream: https://github.com/titanous/json5, tag `v1.0.0`, commit
`5f74668fe3ed2dc3cc990224d823e5dfa0a678ac`.
Module checksum: `h1:hJf8Su1d9NuI/ffpxgxQfxh/UiBFZX7bMPid0rIL/7s=`.

The original decoder source notices and `LICENSE` are retained. The decoder is
BSD-3-Clause-derived Go encoding/json code; the included upstream JSON5 parse
fixtures carry the MIT notice in the same license file. No module-cache files
were modified. The root module uses an explicit local replacement; this is not
a root vendor tree and does not affect unrelated dependencies.

Only decoder source and upstream parse data are copied. Upstream Otto-based
JavaScript comparison tests/dependencies are deliberately not included or run.
Local conformance and Python differential checks are development tests, not
runtime parser dependencies. Go toolchain minimum here matches the application.

Patch inventory and conformance status are maintained by this project. This is
project-owned maintenance, not a claim that upstream provides duplicate or
resource guards. The fork is conditional on a bounded conformance gate; do not
expand it into a new parser without the coordinating parent's approval.

Localized changes:

- JSON5 Unicode whitespace and line terminators without changing byte offsets.
- Full IdentifierName decoding for quoted/unquoted keys, `\uXXXX` escapes,
  surrogate pairs, ZWNJ/ZWJ and Other_ID properties.
- JSON5 `\xHH`, `\v`, `\0`, non-escape characters and Unicode line continuation.
- Signed NaN and raw `Number` retention, with stricter malformed-number checks.
- Duplicate decoded-key rejection for maps, structs and interface trees.
- Byte, scanner-depth and value-count limits, invalid UTF-8/unpaired-surrogate
  rejection, and unterminated block-comment detection.
- Local conformance, upstream parse-data, Python differential, safety and fuzz
  tests. No JavaScript execution or upstream Otto comparison dependency.
