package bizrule

import (
	"strings"
)

// FileNameRule extracts a material number (料号) from a file's name, porting the
// legacy DC FileMon GenerateFNumber logic used for the 图档库 (drawings library).
// The material number is derived from the file name itself, not the directory —
// drawings are named "<料号> <描述>.<ext>", e.g.:
//
//	0931-1234 法兰盘.pdf   → 0931-1234
//	31-5000013-V1.0.pdf    → 0931-5000013-V1.0
//	A1234+B5678.dwg        → A1234
//	1234_56.step           → 1234
//
// Steps (in order): drop the extension, cut at the first Chinese character, then
// take the part before the first '+', space, or '_'; finally a 31-/32- prefix is
// completed to 0931-/0932-. Directories yield no material number.
type FileNameRule struct{}

// ExtractBizKey returns the material number for the file at path, or "" when the
// name has no recognisable material number.
func (FileNameRule) ExtractBizKey(path string) string {
	name := lastSegment(path)
	if name == "" {
		return ""
	}
	return generateFNumber(name)
}

// generateFNumber mirrors the legacy MysqlMapping.GenerateFNumber.
func generateFNumber(fileName string) string {
	result := fileName

	// Drop the extension (everything after the last dot, if any).
	if i := strings.LastIndex(result, "."); i > 0 {
		result = result[:i]
	}

	// Cut at the first Chinese character.
	if i := firstChineseIndex(result); i >= 0 {
		result = result[:i]
	}

	// Take the segment before the first '+', space, or '_'.
	if i := strings.IndexByte(result, '+'); i >= 0 {
		result = result[:i]
	}
	if i := strings.IndexByte(result, ' '); i >= 0 {
		result = result[:i]
	}
	if i := strings.IndexByte(result, '_'); i >= 0 {
		result = result[:i]
	}

	// Complete a short numeric prefix: 31- → 0931-, 32- → 0932-.
	if strings.HasPrefix(result, "31-") || strings.HasPrefix(result, "32-") {
		result = "09" + result
	}

	return strings.TrimSpace(result)
}

// firstChineseIndex returns the byte index of the first CJK character in s, or
// -1 if none. Matches the legacy [一-龥] range.
func firstChineseIndex(s string) int {
	for i, r := range s {
		if r >= 0x4e00 && r <= 0x9fa5 {
			return i
		}
	}
	return -1
}

// lastSegment is defined in the handler package; bizrule needs its own copy.
func lastSegment(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	if i := strings.LastIndex(p, `\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
