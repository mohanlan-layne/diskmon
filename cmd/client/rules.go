//go:build windows

package main

import (
	"diskmon/internal/bizrule"
	"diskmon/internal/config"
)

// buildRules maps biz_rule names (from config) to VolumeRule implementations.
//
// This is the ONLY file that needs to change when deploying to a new disk.
// Add a new entry here and implement the corresponding VolumeRule in internal/bizrule/.
func buildRules(cfg *config.ClientConfig) bizrule.Registry {
	return bizrule.Registry{
		"drawings": &bizrule.DrawingsRule{Root: `D:\drawings\`},
		// Example: "docs": &bizrule.DocsRule{Root: `E:\docs\`},
	}
}
