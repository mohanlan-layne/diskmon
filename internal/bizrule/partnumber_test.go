package bizrule

import "testing"

func TestPartNumberRule(t *testing.T) {
	const base = `E:\file15\1.2_部门文件\...\CNC\2026\5\SHIYI`
	r := PartNumberRule{}

	tests := []struct {
		name string
		path string
		want string
	}{
		// Shape 1: letter + ≥4-digit code (A-/B-/C-/D-/AL).
		{"A part number", base + `\A-2214870321A\ZM\A-2214870321A.NC`, "A-2214870321A"},
		{"B part number", base + `\B-2510541184B\x.NC`, "B-2510541184B"},
		{"D part number", base + `\D-2520340592A\x.NC`, "D-2520340592A"},
		{"C part number", base + `\C-2510541184B\x.NC`, "C-2510541184B"},
		{"AL material code", base + `\AL6061-0502\x.NC`, "AL6061-0502"},
		{"description after space", base + `\A-5019930A 解锁连接块\f.NC`, "A-5019930A"},
		{"XIU suffix dropped", base + `\A-2317810064A-XIU\x.NC`, "A-2317810064A"},
		{"material suffix dropped", base + `\A-2315490102A-6061\x.NC`, "A-2315490102A"},
		{"chinese appended", base + `\A-2422480371A一出四\x.NC`, "A-2422480371A"},

		// Shape 2: numeric part numbers, 31-/32- completed to 0931-/0932-.
		{"num 31 → 0931 with glued version", base + `\31-0053180V14\x.NC`, "0931-0053180V14"},
		{"num 31 with -V1.0", base + `\31-0046302-V1.0\x.NC`, "0931-0046302V1"},
		{"num 0931 kept, glued V11", base + `\0931-0093849V11-XIU\x.NC`, "0931-0093849V11"},

		// Shape 3: letters+digits part number, only with -V version.
		{"GC part number with -V1.0", base + `\GC000003771-V1.0\ZM-1\x.NC`, "GC000003771V1"},
		{"GA part number with -V2.3", base + `\GA00000011060-V2.3\FM-2\x.NC`, "GA00000011060V2"},

		// Process / serial folders → excluded (≥4-digit rule).
		{"process ZM-1", base + `\ZM-1\x.NC`, ""},
		{"process FM-2", base + `\FM-2\x.NC`, ""},
		{"process CM-10", base + `\CM-10\x.NC`, ""},
		{"short B-1", base + `\B-1\x.NC`, ""},
		{"short B-08", base + `\B-08\x.NC`, ""},
		{"short A-2", base + `\A-2\x.NC`, ""},

		// Batch numbers (letters+digits, no -V version) → excluded.
		{"batch JLDC", base + `\JLDC1910002\x.NC`, ""},
		{"batch DCI", base + `\DCI1912011\x.NC`, ""},

		// The real misclassification case: part number sits before the process
		// folder, so it wins.
		{"real: batch/partnum/process",
			base + `\JLDC1910002\GC000003771-V1.0\ZM-1\GC000003771-V1.NC`, "GC000003771V1"},
		{"real: operator/partnum/process",
			base + `\A-2535270045A 舞肌\ZM\x.png`, "A-2535270045A"},

		// Non-part-number paths → empty.
		{"loose prt file", `E:\x\10号厂房程式\A-2520340592A.prt`, ""},
		{"editor dir", `E:\x\CIMCOEdit5.5\a.exe`, ""},
		{"chinese dir", `E:\x\编程\a.dwg`, ""},
		{"pure number", base + `\111111\x.NC`, ""},
		{"size code", base + `\339x595\x.NC`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.ExtractBizKey(tt.path); got != tt.want {
				t.Errorf("ExtractBizKey(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
