package bizrule

import "testing"

func TestPartNumberRule(t *testing.T) {
	const base = `E:\1.2_部门文件\1042 数智运营研发中心\1046 制造部\1046 精密加工中心\10号厂房程式\CNC\2026\5\SHIYI`
	r := PartNumberRule{}

	tests := []struct {
		name string
		path string
		want string
	}{
		{"plain part number", base + `\A-2214870321A\ZM\A-2214870321A.NC`, "A-2214870321A"},
		{"C prefix", base + `\C-2510541184B\x.NC`, "C-2510541184B"},
		{"with description after space", base + `\A-5019930A 解锁连接块\file.NC`, "A-5019930A"},
		{"chinese description", base + `\A-2535270045A 舞肌-针板固定大板-B\ZM\x.png`, "A-2535270045A"},
		{"XIU suffix no space", base + `\A-2317810064A-XIU\x.NC`, "A-2317810064A"},
		{"XIU suffix with space", base + `\A-2519600074A -XIU\x.NC`, "A-2519600074A"},
		{"XIU no trailing letter", base + `\A-2518321501-XIU\x.NC`, "A-2518321501"},
		{"version letter plus XIU", base + `\A-2524040010D-XIU\x.NC`, "A-2524040010D"},
		{"AL material code", base + `\AL6061-0502\x.NC`, "AL6061-0502"},
		{"the folder itself (is_dir record)", base + `\A-2535270045A 舞肌-针板固定大板-B`, "A-2535270045A"},

		// Material/process suffix → folded into the main part number.
		{"material suffix -6061", base + `\A-2315490102A-6061\x.NC`, "A-2315490102A"},
		{"process suffix -JCB", base + `\A-2510540547A-JCB\x.NC`, "A-2510540547A"},
		{"suffix -AL6061", base + `\A-2520320160A-AL6061\x.NC`, "A-2520320160A"},
		{"suffix -YZ no trailing letter", base + `\A-2522880044-YZ\x.NC`, "A-2522880044"},
		{"chinese appended no space", base + `\A-2422480371A一出四\x.NC`, "A-2422480371A"},
		{"bracket appended", base + `\A-2534150021A(1X2)\x.NC`, "A-2534150021A"},

		// Numeric part numbers: 31-/32- completed to 0931-/0932-.
		{"num 31 prefix completed", base + `\31-0053180V14\x.NC`, "0931-0053180V14"},
		{"num 0931 prefix kept", base + `\0931-0093849V11-XIU\x.NC`, "0931-0093849V11"},
		{"num 31 with -V1.0", base + `\31-0046302-V1.0\x.NC`, "0931-0046302"},

		// Non-part-number paths → empty.
		{"operator dir only", base, ""},
		{"loose prt file in 程式 root", `E:\...\10号厂房程式\A-2520340592A.prt`, ""},
		{"editor dir", `E:\...\10号厂房程式\CIMCOEdit5.5\x.exe`, ""},
		{"chinese dir", `E:\...\10号厂房程式\编程\x.dwg`, ""},
		{"S07 line", `E:\...\1042 数智运营研发中心\S07组装线\x.txt`, ""},
		{"date-named prt", `E:\...\10号厂房程式\2021-11-10myrole.mtx`, ""},
		{"pure number", `E:\...\SHIYI\111111\x.NC`, ""},
		{"size code", `E:\...\SHIYI\339x595\x.NC`, ""},
		{"short numeric code stays empty", `E:\...\SHIYI\0330-GK\x.NC`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.ExtractBizKey(tt.path); got != tt.want {
				t.Errorf("ExtractBizKey(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
