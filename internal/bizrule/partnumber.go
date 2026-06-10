package bizrule

import (
	"regexp"
	"strings"
)

// PartNumberRule extracts a part-number business key from the first path segment
// that looks like a part number. It recognises three shapes and folds a version
// suffix into the key, while excluding process folders (ZM-1, FM-2…) and batch
// numbers (JLDC1910002):
//
//	A-2535270045A / B-2510541184B / AL6061-0502  → as-is (letter + ≥4-digit code)
//	31-0053180V14 / 0931-5000013                 → 31-/32- completed to 0931-/0932-
//	GC000003771-V1.0 / GA00000011060-V2.3        → GC000003771V1 / GA00000011060V2
//
// Rules:
//   - A "-" followed by fewer than 4 digits (ZM-1, B-1, A-02) is a process/serial
//     folder, not a part number → skipped.
//   - A bare "letters+digits" token (JLDC1910002) is a batch number unless it
//     carries a "-V<n>" version, which marks it as a part number (GC…-V1.0).
//   - A version suffix "-V1.x" / "-v2.x" folds to "V1" / "V2" appended to the key.
//   - A trailing description (after a space), a revision suffix (-XIU) and a
//     material/process suffix (-6061, -JCB) are dropped.
type PartNumberRule struct{}

// letterDashRe: letter-prefixed part number with a hyphen and ≥4 digits, e.g.
// A-2535270045A, C-2510541184B, AL6061-0502. The ≥4-digit requirement excludes
// process/serial folders like ZM-1, B-1, A-02.
var letterDashRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-\d{4,}[A-Za-z0-9]*`)

// numPartRe: numeric part number with a 31-/32-/0931-/0932- prefix.
var numPartRe = regexp.MustCompile(`^(0931|0932|31|32)-\d+[A-Za-z0-9]*`)

// letterNumRe: letters directly followed by ≥4 digits (no hyphen), e.g.
// GC000003771, JLDC1910002. Only counts as a part number when a -V version
// follows (see partNumberFromSegment), which distinguishes part numbers from
// batch numbers.
var letterNumRe = regexp.MustCompile(`^[A-Za-z]+\d{4,}`)

// versionRe: a "-V1.0" / "-v2.3" suffix; group 1 is the major version.
var versionRe = regexp.MustCompile(`^-[Vv](\d+)`)

// ExtractBizKey returns the part number of the first matching path segment, or
// "" when no segment looks like a part number.
func (PartNumberRule) ExtractBizKey(path string) string {
	path = strings.ReplaceAll(path, "/", `\`)
	for _, seg := range strings.Split(path, `\`) {
		if key := partNumberFromSegment(seg); key != "" {
			return key
		}
	}
	return ""
}

func partNumberFromSegment(seg string) string {
	// A description, if any, follows the first space.
	token := seg
	if i := strings.IndexByte(token, ' '); i >= 0 {
		token = token[:i]
	}

	// Shape 1: letter-prefixed with a hyphen and ≥4 digits.
	if m := letterDashRe.FindString(token); m != "" {
		if isFileName(token, m) {
			return ""
		}
		return m + versionSuffix(token, m)
	}

	// Shape 2: numeric part number; 31-/32- completed to 0931-/0932-.
	if m := numPartRe.FindStringSubmatch(token); m != nil {
		full := m[0]
		if isFileName(token, full) {
			return ""
		}
		ver := versionSuffix(token, full)
		if m[1] == "31" || m[1] == "32" {
			full = "09" + full
		}
		return full + ver
	}

	// Shape 3: letters+digits with no hyphen — a part number only if a -V
	// version follows; otherwise it's a batch number and is skipped.
	if m := letterNumRe.FindString(token); m != "" {
		ver := versionSuffix(token, m)
		if ver == "" {
			return ""
		}
		return m + ver
	}
	return ""
}

// versionSuffix returns "V"+major when token has "-V<n>.x" right after base.
func versionSuffix(token, base string) string {
	if mm := versionRe.FindStringSubmatch(token[len(base):]); mm != nil {
		return "V" + mm[1]
	}
	return ""
}

// isFileName reports whether the matched part number is immediately followed by
// a file extension (".") then a letter — a file name like A-2520340592A.prt —
// rather than a folder. A "." then a digit (version V1.0) is not an extension.
func isFileName(seg, match string) bool {
	if len(match) >= len(seg) || seg[len(match)] != '.' {
		return false
	}
	rest := seg[len(match)+1:]
	return rest != "" && isASCIILetter(rest[0])
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}
