package bizrule

import "strings"

// DrawingsRule extracts the business key from a drawings volume where the
// first directory level under Root is the material+version identifier.
//
// Example:
//
//	Root:  D:\drawings\
//	Path:  D:\drawings\1234-56_A\subdir\file.pdf
//	BizKey: 1234-56_A
type DrawingsRule struct {
	Root string // e.g. `D:\drawings\`
}

func (r *DrawingsRule) ExtractBizKey(path string) string {
	// Normalize to backslash.
	path = strings.ReplaceAll(path, "/", `\`)
	root := strings.ReplaceAll(r.Root, "/", `\`)
	if !strings.HasSuffix(root, `\`) {
		root += `\`
	}

	if !strings.HasPrefix(strings.ToLower(path), strings.ToLower(root)) {
		return ""
	}
	rel := path[len(root):]
	if rel == "" {
		return ""
	}
	// The first path segment is the biz key.
	idx := strings.IndexByte(rel, '\\')
	if idx < 0 {
		return rel // path is directly under root with no subdirectory
	}
	return rel[:idx]
}
