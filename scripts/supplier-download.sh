#!/usr/bin/env bash
# supplier-download.sh — 「联合东创供应商自助下图」全链路测试
#
# 复现 Saber3 前端(suppliersdownload.vue)的真实请求,验证 diskmon 能
# 接管老 PrintServer 的 5 个接口。可反复运行。
#
# 用法:
#   ./scripts/supplier-download.sh
#   BASE_URL=http://127.0.0.1:8080 SERVER_ID=dc-it-s-31 ./scripts/supplier-download.sh
#
# 依赖:
#   - 本地已起 diskmon server(默认 127.0.0.1:8080),连 192.168.1.190 测试库 + AList
#   - mysql 客户端(取真实料号样本);没有则用 SAMPLE_KEYS 环境变量手动指定:
#       SAMPLE_KEYS="0931-0042690V11 0931-0046305V11" ./scripts/supplier-download.sh
#   - curl;jq 可选(无 jq 时退化为 grep 解析)
set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
SERVER_ID="${SERVER_ID:-dc-it-s-31}"
API="$BASE_URL/api/pdf-printing/$SERVER_ID"
DRAW="$BASE_URL/api/drawing/$SERVER_ID"

MYSQL_BIN="${MYSQL_BIN:-/usr/local/opt/mysql-client/bin/mysql}"
DB_HOST="${DB_HOST:-192.168.1.190}"
DB_PORT="${DB_PORT:-3306}"
DB_USER="${DB_USER:-yudao}"
DB_PASS="${DB_PASS:-Yudao@2025}"
DB_NAME="${DB_NAME:-diskmon}"

OUT_DIR="${OUT_DIR:-/tmp/diskmon-supplier-test}"
mkdir -p "$OUT_DIR"

c_g="\033[32m"; c_r="\033[31m"; c_y="\033[33m"; c_0="\033[0m"
ok(){   echo -e "  ${c_g}✅ $1${c_0}"; }
bad(){  echo -e "  ${c_r}❌ $1${c_0}"; exit 1; }
note(){ echo -e "  ${c_y}· $1${c_0}"; }
step(){ echo -e "\n${c_y}── $1 ──${c_0}"; }

# 从响应里取 mergeName(优先 jq,退化 grep)
get_merge(){
  if command -v jq >/dev/null 2>&1; then
    echo "$1" | jq -r '.mergeName // empty'
  else
    echo "$1" | grep -o '"mergeName":"[^"]*"' | head -1 | sed 's/.*:"//;s/"//'
  fi
}

echo "=========================================================="
echo " 联合东创供应商自助下图 — 全链路测试"
echo " BASE_URL = $BASE_URL"
echo " SERVER   = $SERVER_ID"
echo "=========================================================="

# ── 0. server 存活 ────────────────────────────────────────
step "0. server 存活检查"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 "$BASE_URL/api/servers" || echo 000)
[ "$code" = "200" ] || bad "/api/servers 返回 $code(server 没起?)"
ok "/api/servers 200"

# ── 1. 取真实料号样本 ─────────────────────────────────────
step "1. 取真实料号样本(排除 daily 临时目录)"
if [ -n "${SAMPLE_KEYS:-}" ]; then
  read -r -a KEYS <<< "$SAMPLE_KEYS"
  note "使用手动指定: ${KEYS[*]}"
else
  SQL="SELECT biz_key FROM file_catalog
       WHERE server_id='$SERVER_ID' AND is_dir=0 AND LOWER(ext)='.pdf' AND size>0
         AND biz_key REGEXP '^[0-9]+[0-9A-Za-z-]*\$'
         AND path NOT LIKE '%\\\\\\\\daily\\\\\\\\%'
       GROUP BY biz_key ORDER BY MAX(updated_at) DESC LIMIT 2;"
  KEYS=()
  while IFS= read -r line; do [ -n "$line" ] && KEYS+=("$line"); done < <(
    "$MYSQL_BIN" -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" -p"$DB_PASS" "$DB_NAME" -N -e "$SQL" 2>/dev/null
  )
  [ "${#KEYS[@]}" -ge 2 ] || bad "取样料号不足(库里查不到,或设 SAMPLE_KEYS 手动指定)"
  note "样本料号: ${KEYS[*]}"
fi
ok "拿到 ${#KEYS[@]} 个料号"

# ── 2. 料号预览(GET /drawing/{料号}) ─────────────────────
step "2. 料号预览 → 期望直接返回 PDF 流(同老 PrintServer)"
prev="$OUT_DIR/preview.pdf"
code=$(curl -s --max-time 30 -o "$prev" -w '%{http_code}' "$DRAW/${KEYS[0]}")
[ "$code" = "200" ] || bad "预览返回 $code"
head4=$(head -c 4 "$prev")
[ "$head4" = "%PDF" ] || bad "预览返回的不是 PDF(头=$head4)"
ok "预览 ${KEYS[0]} → 200 PDF ($(wc -c <"$prev") 字节)"

# ── 3. 合并打印(POST /join) ──────────────────────────────
step "3. 合并打印 → POST /join(前端格式 [{pdfName,pdfExplain}])"
join_body='['
for i in "${!KEYS[@]}"; do
  [ "$i" -gt 0 ] && join_body+=','
  join_body+="{\"pdfName\":\"${KEYS[$i]}\",\"pdfExplain\":\"计划跟踪号:TRK-$i 采购单号:PO-2024 数量:$((i+1))\"}"
done
join_body+=']'
note "请求体: $join_body"
resp=$(curl -s --max-time 60 -X POST "$API/join" -H 'Content-Type: application/json' -d "$join_body")
print_name=$(get_merge "$resp")
[ -n "$print_name" ] || bad "join 未返回 mergeName,响应: $resp"
ok "合并成功 → mergeName=$print_name"

# ── 4. 下载打印件(GET /print/{name}) ─────────────────────
step "4. 下载打印件 → GET /print/$print_name"
pdf="$OUT_DIR/print.pdf"
code=$(curl -s --max-time 60 -o "$pdf" -w '%{http_code}' "$API/print/$print_name")
[ "$code" = "200" ] || bad "下载打印件返回 $code"
head4=$(head -c 4 "$pdf")
[ "$head4" = "%PDF" ] || bad "打印件不是 PDF(头=$head4)"
ok "打印件已下载: $pdf ($(wc -c <"$pdf") 字节, PDF ✅)"

# ── 5. 批量打包(POST /zip,前端字符串数组) ───────────────
step "5. 批量打包 → POST /zip(前端格式 [\"料号\"])"
zip_body='['
for i in "${!KEYS[@]}"; do [ "$i" -gt 0 ] && zip_body+=','; zip_body+="\"${KEYS[$i]}\""; done
zip_body+=']'
note "请求体: $zip_body"
resp=$(curl -s --max-time 60 -X POST "$API/zip" -H 'Content-Type: application/json' -d "$zip_body")
zip_name=$(get_merge "$resp")
[ -n "$zip_name" ] || bad "zip(数组格式)未返回 mergeName,响应: $resp"
ok "打包成功 → mergeName=$zip_name"

# ── 6. 下载打包件(GET /zip-download/{name}) ──────────────
step "6. 下载打包件 → GET /zip-download/$zip_name"
zipf="$OUT_DIR/pack.zip"
code=$(curl -s --max-time 60 -o "$zipf" -w '%{http_code}' "$API/zip-download/$zip_name")
[ "$code" = "200" ] || bad "下载打包件返回 $code"
head2=$(head -c 2 "$zipf")
[ "$head2" = "PK" ] || bad "打包件不是 ZIP(头=$head2)"
ok "打包件已下载: $zipf ($(wc -c <"$zipf") 字节, ZIP ✅)"

# ── 7. 打包向后兼容(diskmon 原生对象格式) ────────────────
step "7. 打包向后兼容 → POST /zip(原生格式 {biz_keys:[]})"
obj_body="{\"biz_keys\":[\"${KEYS[0]}\"]}"
note "请求体: $obj_body"
resp=$(curl -s --max-time 60 -X POST "$API/zip" -H 'Content-Type: application/json' -d "$obj_body")
zip_name2=$(get_merge "$resp")
[ -n "$zip_name2" ] || bad "zip(对象格式)未返回 mergeName,响应: $resp"
ok "对象格式打包成功 → mergeName=$zip_name2(向后兼容 ✅)"

echo -e "\n${c_g}=========================================================="
echo -e " 🎉 全链路通过 — diskmon 可接管供应商自助下图"
echo -e " 产物目录: $OUT_DIR"
echo -e "==========================================================${c_0}"
