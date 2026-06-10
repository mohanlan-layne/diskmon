package bizrule

import (
	"regexp"
	"strings"
)

// PartNumberRule extracts a part-number business key from the first path segment
// that starts with a part number, as used by the CNC program directories. It
// takes the part number at the head of the segment and drops everything after
// it — a trailing description, a revision suffix (-XIU), a material/process
// suffix (-6061, -JCB, -YZ), or a directly-appended Chinese name / bracket — so
// every file under a part-number folder, and all its variants, fold into one
// key. Numeric part numbers are normalised: a 31-/32- prefix is completed to
// 0931-/0932-.
//
//	...\SHIYI\A-2535270045A 舞肌-针板固定大板-B\ZM\x.NC → A-2535270045A
//	...\SHIYI\A-2317810064A-XIU\...                     → A-2317810064A
//	...\SHIYI\A-2315490102A-6061\...                    → A-2315490102A
//	...\SHIYI\A-2422480371A一出四\...                   → A-2422480371A
//	...\SHIYI\C-2510541184B\...                         → C-2510541184B
//	...\SHIYI\31-0053180V14\...                         → 0931-0053180V14
//	...\SHIYI\0931-0093849V11-XIU\...                   → 0931-0093849V11
//
// Year/month/operator dirs, Chinese-named dirs, pure numbers, sizes (339x595)
// and loose files (A-2520340592A.prt) don't yield a key.
type PartNumberRule struct{}

// letterPartRe matches a letter-prefixed part number at the head of a segment:
// a letter, then letters/digits, a hyphen, digits, and an optional trailing
// code. Anything after (—6061, -XIU, 一出四, "(1X2)") is excluded by stopping at
// the next non [A-Za-z0-9] character. Examples: A-2214870321A, C-2510541184B,
// AL6061-0502, A-2522880044.
var letterPartRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-\d+[A-Za-z0-9]*`)

// numPartRe matches a numeric part number with a 31-/32-/0931-/0932- prefix at
// the head of a segment, capturing the prefix so 31-/32- can be completed.
var numPartRe = regexp.MustCompile(`^(0931|0932|31|32)-\d+[A-Za-z0-9]*`)

// ExtractBizKey returns the part number of the first matching path segment, or
// "" when no segment starts with a recognised part number.
func (PartNumberRule) ExtractBizKey(path string) string {
	path = strings.ReplaceAll(path, "/", `\`)
	for _, seg := range strings.Split(path, `\`) {
		if key := partNumberFromSegment(seg); key != "" {
			return key
		}
	}
	return ""
}

// partNumberFromSegment extracts the main part number from a single segment, or
// "" if the segment does not start with a part number.
func partNumberFromSegment(seg string) string {
	if m := letterPartRe.FindString(seg); m != "" {
		if isFileName(seg, m) {
			return ""
		}
		return m
	}
	if m := numPartRe.FindStringSubmatch(seg); m != nil {
		full := m[0]
		if isFileName(seg, full) {
			return ""
		}
		// Complete the short numeric prefix: 31- → 0931-, 32- → 0932-.
		if m[1] == "31" || m[1] == "32" {
			full = "09" + full
		}
		return full
	}
	return ""
}

// isFileName reports whether the matched part number is immediately followed by
// a file extension in seg — a "." then a letter (A-2520340592A.prt) — rather
// than a part-number folder. A "." followed by a digit (a version like V1.0) is
// not treated as an extension.
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
