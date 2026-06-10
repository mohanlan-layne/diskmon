package bizrule

import "testing"

func TestFileNameRule(t *testing.T) {
	r := FileNameRule{}
	const base = `D:\drawings\零件图裆库\产品专案资料`

	tests := []struct {
		name string
		path string
		want string
	}{
		{"material + chinese desc", base + `\0931-1234 法兰盘.pdf`, "0931-1234"},
		{"31- completed to 0931-", base + `\31-5000013-V1.0.pdf`, "0931-5000013-V1.0"},
		{"32- completed to 0932-", base + `\32-0001.dwg`, "0932-0001"},
		{"plus separator", base + `\A1234+B5678.dwg`, "A1234"},
		{"underscore separator", base + `\1234_56.step`, "1234"},
		{"space separator", base + `\GB-200 垫片.pdf`, "GB-200"},
		{"chinese right after number", base + `\0931-1234法兰盘.pdf`, "0931-1234"},
		{"no extension", base + `\0931-9999`, "0931-9999"},
		{"plain name", base + `\ABC-001.prt`, "ABC-001"},
		{"forward slashes", `D:/drawings/零件图裆库/0931-1234 件.pdf`, "0931-1234"},
		// Pure-Chinese name → cut at first char → empty.
		{"pure chinese", base + `\说明文档.pdf`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.ExtractBizKey(tt.path); got != tt.want {
				t.Errorf("ExtractBizKey(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
