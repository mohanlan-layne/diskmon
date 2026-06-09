// Package bizrule defines the interface for extracting business identifiers
// (material number + version) from file paths. Each volume can have its own
// implementation registered by name in cmd/client/rules.go.
package bizrule

// VolumeRule extracts a business key from a file path.
// The business key identifies a logical asset (e.g. a material + version pair)
// and is stored in file_catalog.biz_key for cross-volume querying.
//
// Implementations are registered per volume in cmd/client/rules.go.
// Adding support for a new disk requires only a new implementation here
// and a registration entry there — no other code changes.
type VolumeRule interface {
	// ExtractBizKey returns the business key for the given absolute path.
	// Returns an empty string when the path does not match the expected structure.
	ExtractBizKey(path string) string
}

// Registry maps a rule name (as specified in config.VolumeConfig.BizRule) to its implementation.
type Registry map[string]VolumeRule

// Get returns the VolumeRule for the given name, or a NoopRule if not found.
func (r Registry) Get(name string) VolumeRule {
	if rule, ok := r[name]; ok {
		return rule
	}
	return NoopRule{}
}

// NoopRule always returns an empty business key. Used when no rule is configured.
type NoopRule struct{}

func (NoopRule) ExtractBizKey(_ string) string { return "" }
