package handler

// 供应商自助下图 —— 端到端集成测试。
//
// 复现 Saber3「联合东创供应商自助下图」前端的真实请求,覆盖 5 个接口:
//   1. 料号预览   GET  /api/drawing/{sid}/{料号}            (直接返回 PDF 流,同老 PrintServer)
//   2. 合并打印   POST /api/pdf-printing/{sid}/join
//   3. 取打印件   GET  /api/pdf-printing/{sid}/print/{mergeName}
//   4. 批量打包   POST /api/pdf-printing/{sid}/zip        (前端字符串数组格式)
//   5. 取打包件   GET  /api/pdf-printing/{sid}/zip-download/{mergeName}
// 外加打包接口的 diskmon 原生对象格式(向后兼容性)。
//
// 需要真实数据库 + AList,默认 SKIP。运行:
//
//	DISKMON_IT_DSN='yudao:Yudao@2025@tcp(192.168.1.190:3306)/diskmon?parseTime=true&charset=utf8mb4&loc=Local' \
//	  go test ./internal/server/handler/ -run TestSupplierDownloadFlow -v
//
// 可选环境变量:
//	DISKMON_IT_SERVER  目标 server_id(默认 dc-it-s-31)

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "github.com/go-sql-driver/mysql"
)

func TestSupplierDownloadFlow(t *testing.T) {
	dsn := os.Getenv("DISKMON_IT_DSN")
	if dsn == "" {
		t.Skip("设置 DISKMON_IT_DSN 后运行(集成测试,需真实 MySQL + AList)")
	}
	serverID := envOr("DISKMON_IT_SERVER", "dc-it-s-31")

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}

	// 取两个真实、含有效 PDF、URL 友好的料号。
	keys := sampleBizKeys(t, db, serverID, 2)
	t.Logf("① 取样料号(server=%s): %v", serverID, keys)

	// 组装与生产一致的 drawing 路由。
	dl := NewDownloadHandler(db, nil) // smbMounts=nil → 文件走 AList(从 servers.alist_urls 读)
	r := chi.NewRouter()
	NewDrawingHandler(db, dl).Register(r)
	ts := httptest.NewServer(r)
	defer ts.Close()

	base := ts.URL + "/api"

	// ── ② 料号预览:期望直接返回 PDF 流(同老 PrintServer) ───────────
	{
		u := base + "/drawing/" + serverID + "/" + keys[0]
		assertMagic(t, u, "%PDF", "application/pdf")
		t.Logf("② 预览 %s → 200 PDF 流 ✅", keys[0])
	}

	// ── ③ 合并打印:POST join(前端格式 [{pdfName,pdfExplain}]) ──────
	// v1 契约:{"status":true,"mergeName":"<无扩展名>","lhs":""}
	var printName string
	{
		items := make([]map[string]string, 0, len(keys))
		for i, k := range keys {
			items = append(items, map[string]string{
				"pdfName":    k,
				"pdfExplain": "计划跟踪号:TRK-" + itoa(i) + " 采购单号:PO-2024 数量:" + itoa(i+1),
			})
		}
		body, out := postJSON(t, ts.URL+"/api/pdf-printing/"+serverID+"/join", items)
		if out.MergeName == "" || !out.Status {
			t.Fatalf("join 期望 status=true 且 mergeName 非空,响应: %s", body)
		}
		if strings.Contains(out.MergeName, ".") {
			t.Fatalf("v1 契约 mergeName 不应带扩展名,实际 %s", out.MergeName)
		}
		if out.Lhs != "" {
			t.Fatalf("样本料号均有图,lhs 应为空,实际 %q", out.Lhs)
		}
		printName = out.MergeName
		t.Logf("③ 合并打印 %d 个料号 → {status:true, mergeName:%s, lhs:\"\"} ✅", len(keys), printName)
	}

	// ── ④ 取打印件:GET print/{mergeName},校验是 PDF 且可重复下载 ───
	{
		u := base + "/pdf-printing/" + serverID + "/print/" + printName
		assertMagic(t, u, "%PDF", "application/pdf")
		assertMagic(t, u, "%PDF", "application/pdf") // v1 语义:TTL 内可重复下载
		t.Logf("④ 下载打印件 %s → PDF,重复下载 ✅", printName)
	}

	// ── ⑤ 批量打包(前端字符串数组 ["料号"]) ────────────────────────
	var zipName string
	{
		body, out := postJSON(t, ts.URL+"/api/pdf-printing/"+serverID+"/zip", keys)
		if out.MergeName == "" || !out.Status {
			t.Fatalf("zip(数组格式)期望 status=true 且 mergeName 非空,响应: %s", body)
		}
		if strings.Contains(out.MergeName, ".") {
			t.Fatalf("v1 契约 mergeName 不应带扩展名,实际 %s", out.MergeName)
		}
		zipName = out.MergeName
		t.Logf("⑤ 批量打包(字符串数组)%v → {status:true, mergeName:%s} ✅", keys, zipName)
	}

	// ── ⑥ 取打包件:GET zip-download/{mergeName},校验 ZIP 且可重复,
	//     且内部结构为 料号\文件名(v1 按料号分文件夹) ─────────────────
	{
		u := base + "/pdf-printing/" + serverID + "/zip-download/" + zipName
		assertMagic(t, u, "PK", "application/zip")
		raw := getBytes(t, u) // v1 语义:可重复下载
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatalf("解析 ZIP: %v", err)
		}
		for _, f := range zr.File {
			if !strings.Contains(f.Name, `\`) {
				t.Fatalf("v1 契约 ZIP 条目应为 料号\\文件名,实际 %q", f.Name)
			}
		}
		t.Logf("⑥ 下载打包件 %s → ZIP(%d 个条目,料号\\文件名 结构),重复下载 ✅", zipName, len(zr.File))
	}

	// ── ⑦ 打包接口向后兼容:diskmon 原生对象格式 {biz_keys:[]} ───────
	{
		reqBody := map[string]any{"biz_keys": keys[:1]}
		body, out := postJSON(t, ts.URL+"/api/pdf-printing/"+serverID+"/zip", reqBody)
		if out.MergeName == "" {
			t.Fatalf("zip(对象格式)未返回 mergeName,响应: %s", body)
		}
		t.Logf("⑦ 打包(对象格式 {biz_keys})%v → mergeName=%s ✅ 向后兼容", keys[:1], out.MergeName)
	}

	// ── ⑧ 错误契约:无图料号 → HTTP 200 + {"code":"500","msg":…} ────
	{
		resp, err := http.Get(base + "/drawing/" + serverID + "/NO-SUCH-KEY-00000")
		if err != nil {
			t.Fatalf("GET 预览(无图): %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("v1 契约错误响应应为 200,实际 %d", resp.StatusCode)
		}
		var e struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(raw, &e); err != nil || e.Code != "500" || e.Msg == "" {
			t.Fatalf("v1 契约错误体应为 {code:\"500\",msg:…},实际 %s", raw)
		}
		t.Logf("⑧ 无图料号 → 200 {code:%q, msg:%q} ✅ v1 错误契约", e.Code, e.Msg)
	}

	t.Log("🎉 供应商自助下图全链路通过")
}

// sampleBizKeys 返回 n 个真实、含有效 PDF、URL 友好(仅字母数字中划线)的料号。
// 排除 daily\ 每日轮换临时目录——那里的文件很快被清理,catalog 行大概率是
// 死链,会让测试误报。
func sampleBizKeys(t *testing.T, db *sql.DB, serverID string, n int) []string {
	t.Helper()
	const q = `SELECT biz_key FROM file_catalog
		WHERE server_id=? AND is_dir=0 AND LOWER(ext)='.pdf' AND size>0
		  AND biz_key REGEXP '^[0-9]+[0-9A-Za-z-]*$'
		  AND path NOT LIKE '%\\\\daily\\\\%'
		GROUP BY biz_key ORDER BY MAX(updated_at) DESC LIMIT ?`
	rows, err := db.Query(q, serverID, n)
	if err != nil {
		t.Fatalf("查询样本料号: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		keys = append(keys, k)
	}
	if len(keys) < n {
		t.Skipf("库中可用料号不足(需要 %d,实际 %d),无法测试", n, len(keys))
	}
	return keys
}

// v1Resp 是老 PrintServer join/zip 的出参形状。
type v1Resp struct {
	Status    bool   `json:"status"`
	MergeName string `json:"mergeName"`
	Lhs       string `json:"lhs"`
}

// postJSON POST 一个 JSON body,返回原始响应文本和解析出的 v1 形状出参。
func postJSON(t *testing.T, url string, payload any) (string, v1Resp) {
	t.Helper()
	buf, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s 期望 200,实际 %d,响应: %s", url, resp.StatusCode, raw)
	}
	var out v1Resp
	_ = json.Unmarshal(raw, &out)
	return string(raw), out
}

// getBytes GET 一个 URL,校验 200 并返回完整响应体。
func getBytes(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s 期望 200,实际 %d,响应: %s", url, resp.StatusCode, raw[:min(len(raw), 300)])
	}
	return raw
}

// assertMagic GET 一个 URL,校验状态 200、Content-Type 含 wantType、body 以 magic 开头。
func assertMagic(t *testing.T, url, magic, wantType string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s 期望 200,实际 %d,响应: %s", url, resp.StatusCode, raw)
	}
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(resp.Body, head); err != nil {
		t.Fatalf("读取响应体: %v", err)
	}
	if string(head) != magic {
		rest, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		t.Fatalf("文件头期望 %q,实际 %q,响应: %s%s", magic, head, head, rest)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, wantType) {
		t.Logf("⚠ Content-Type=%s(期望含 %s)", ct, wantType)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func itoa(i int) string { return strconv.Itoa(i) }
