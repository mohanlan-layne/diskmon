# diskmon-client 安装脚本
# 以管理员身份运行: .\install-client.ps1
# 可选参数:
#   -InstallDir   安装目录，默认 C:\diskmon
#   -Rescan       安装后立即执行全量扫描

param(
    [string]$InstallDir = "C:\diskmon",
    [switch]$Rescan
)

$ServiceName = "diskmon-client"
$ExeName     = "diskmon-client.exe"
$ConfigName  = "config.yaml"

# ── 检查管理员权限 ────────────────────────────────────────────────
if (-not ([Security.Principal.WindowsPrincipal] `
    [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error "请以管理员身份运行此脚本"
    exit 1
}

# ── 检查文件 ──────────────────────────────────────────────────────
$ExePath     = Join-Path $InstallDir $ExeName
$ConfigPath  = Join-Path $InstallDir $ConfigName
$LogDir      = Join-Path $InstallDir "logs"
$CheckpointDir = Join-Path $InstallDir "checkpoints"

if (-not (Test-Path $ExePath)) {
    Write-Error "找不到 $ExePath，请先把编译好的 exe 放到 $InstallDir"
    exit 1
}
if (-not (Test-Path $ConfigPath)) {
    Write-Error "找不到 $ConfigPath，请先创建配置文件"
    exit 1
}

# ── 创建目录 ──────────────────────────────────────────────────────
New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
New-Item -ItemType Directory -Force -Path $CheckpointDir | Out-Null
Write-Host "✓ 目录创建完成: $InstallDir"

# ── 停止并删除旧服务（如果存在）──────────────────────────────────
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    Write-Host "停止旧服务..."
    Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Seconds 2
}

# ── 全量扫描（可选，首次推荐执行）────────────────────────────────
if ($Rescan) {
    Write-Host "执行全量扫描（可能需要几分钟）..."
    & $ExePath --config $ConfigPath --rescan
    if ($LASTEXITCODE -ne 0) {
        Write-Error "全量扫描失败，退出"
        exit 1
    }
    Write-Host "✓ 全量扫描完成"
}

# ── 注册 Windows 服务 ─────────────────────────────────────────────
$BinPath = "`"$ExePath`" --service --config `"$ConfigPath`""
sc.exe create $ServiceName `
    binPath= $BinPath `
    start= auto `
    DisplayName= "DiskMon File Monitor" | Out-Null

# 配置故障自动重启（第1次30s后，第2次60s后，之后每次120s后）
sc.exe failure $ServiceName reset= 86400 actions= restart/30000/restart/60000/restart/120000 | Out-Null

Write-Host "✓ 服务注册完成"

# ── 启动服务 ──────────────────────────────────────────────────────
Start-Service -Name $ServiceName
$svc = Get-Service -Name $ServiceName
Write-Host "✓ 服务状态: $($svc.Status)"

Write-Host ""
Write-Host "═══════════════════════════════════════════════════════════"
Write-Host "  diskmon-client 安装完成"
Write-Host "  服务状态: $($svc.Status)"
Write-Host "═══════════════════════════════════════════════════════════"
Write-Host ""

# ── 输出注册信息（复制到 diskmon 管理界面）────────────────────────
Write-Host "正在获取注册信息..."
Start-Sleep -Seconds 2
& $ExePath --config $ConfigPath --print-info

Write-Host ""
Write-Host "═══════════════════════════════════════════════════════════"
Write-Host "  在 diskmon 管理界面填入上方「注册 JSON」中的字段即可联通"
Write-Host ""
Write-Host "  实时查看日志:"
Write-Host "  Get-Content -Wait '$LogDir\client.log' -Tail 50"
Write-Host ""
Write-Host "  服务管理:"
Write-Host "  Start-Service $ServiceName"
Write-Host "  Stop-Service  $ServiceName"
Write-Host "  Get-Service   $ServiceName"
Write-Host "═══════════════════════════════════════════════════════════"
