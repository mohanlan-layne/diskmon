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
		// CNC 程式目录：从料号文件夹名提取业务 ID（见 internal/bizrule/partnumber.go）。
		"partnumber": bizrule.PartNumberRule{},
		// 图档库（东莞）：从文件名提取料号，移植老 DC FileMon GenerateFNumber
		// （见 internal/bizrule/filename.go）。
		"filename": bizrule.FileNameRule{},
	}
}
