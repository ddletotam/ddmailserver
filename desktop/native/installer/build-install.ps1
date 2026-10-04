# ddmail — сборка, инсталлятор и переустановка на Windows-станции одной командой
# (аналог install_linux.sh). Запуск из любого каталога:
#   powershell -ExecutionPolicy Bypass -File desktop\native\installer\build-install.ps1
#   ... -SkipBuild    — не собирать, упаковать то, что уже лежит в кэше сборки
#   ... -NoInstall    — только собрать инсталлятор
#
# Каждый шаг здесь — грабли, на которые уже наступали:
#   * CARGO_TARGET_DIR бывает не выставлен в окружении инструмента, и cargo
#     молча собирает in-tree, а ISCC пакует старый exe из кэша станции;
#   * ISCC стоит per-user в %LOCALAPPDATA%\Programs и не лежит в PATH;
#   * инсталлятор в конце сам запускает клиент, поэтому Start-Process -Wait
#     (ждёт всё дерево процессов) не возвращается — ждём только сам setup;
#   * «установилось» не значит «установилось то, что собрали» — сверяем хэш.

[CmdletBinding()]
param(
    [switch]$SkipBuild,
    [switch]$NoInstall
)

$ErrorActionPreference = 'Stop'
$native = Split-Path -Parent $PSScriptRoot

if (-not $env:CARGO_TARGET_DIR) {
    $env:CARGO_TARGET_DIR = Join-Path $env:LOCALAPPDATA 'builds\cargo-target'
}
$release = Join-Path $env:CARGO_TARGET_DIR 'release'
$built = Join-Path $release 'ddmail-native.exe'

$version = (Select-String -Path (Join-Path $native 'Cargo.toml') -Pattern '^version\s*=\s*"([^"]+)"' |
    Select-Object -First 1).Matches[0].Groups[1].Value
if (-not $version) { throw 'не нашёл version в Cargo.toml' }

if (-not $SkipBuild) {
    Write-Host "==> cargo build --release ($env:CARGO_TARGET_DIR)"
    Push-Location $native
    try {
        cargo build --release
        if ($LASTEXITCODE -ne 0) { throw "cargo build завершился с кодом $LASTEXITCODE" }
    } finally {
        Pop-Location
    }
}
if (-not (Test-Path $built)) { throw "нет $built — соберите без -SkipBuild" }

$iscc = @(
    (Get-Command ISCC.exe -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Source),
    (Join-Path $env:LOCALAPPDATA 'Programs\Inno Setup 6\ISCC.exe'),
    (Join-Path ${env:ProgramFiles(x86)} 'Inno Setup 6\ISCC.exe'),
    (Join-Path $env:ProgramFiles 'Inno Setup 6\ISCC.exe')
) | Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1
if (-not $iscc) { throw 'ISCC.exe (Inno Setup 6) не найден' }

Write-Host "==> инсталлятор $version"
& $iscc /Q "/DSrcDir=$release" "/DAppVersion=$version" (Join-Path $PSScriptRoot 'ddmail.iss')
if ($LASTEXITCODE -ne 0) { throw "ISCC завершился с кодом $LASTEXITCODE" }
$setup = Join-Path $PSScriptRoot "out\ddmail-setup-$version.exe"
Write-Host "    $setup"
if ($NoInstall) { return }

Write-Host '==> установка'
Get-Process ddmail-native -ErrorAction SilentlyContinue | Stop-Process -Force
$started = Get-Date
$proc = Start-Process $setup -ArgumentList '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/CLOSEAPPLICATIONS' -PassThru
# WaitForExit ждёт только сам setup, не запущенный им клиент.
$proc.WaitForExit()
if ($proc.ExitCode -ne 0) { throw "инсталлятор завершился с кодом $($proc.ExitCode)" }

$installed = Join-Path $env:LOCALAPPDATA 'Programs\ddmail\ddmail-native.exe'
$want = (Get-FileHash $built).Hash
$have = (Get-FileHash $installed).Hash
if ($want -ne $have) { throw "установленный exe не совпадает с собранным ($installed)" }
Write-Host "    exe совпадает со сборкой ($((Get-Item $built).LastWriteTime))"

$client = $null
for ($i = 0; $i -lt 30 -and -not $client; $i++) {
    Start-Sleep -Milliseconds 500
    $client = Get-Process ddmail-native -ErrorAction SilentlyContinue |
        Where-Object { $_.StartTime -ge $started } | Select-Object -First 1
}
if (-not $client) {
    Write-Host '    клиент не поднялся сам — запускаю'
    Start-Process $installed
} else {
    Write-Host "    клиент запущен (PID $($client.Id))"
}
