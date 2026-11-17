# Install session-index from a local checkout on Windows (PowerShell).
# Builds the binary with Go, puts it in $env:LOCALAPPDATA\session-index\bin,
# installs the Claude Code hooks and the session-search skill, then runs doctor.
# UNTESTED on Windows: written and cross-compile-checked on macOS only.
param([string]$BinDir = (Join-Path $env:LOCALAPPDATA 'session-index\bin'), [switch]$NoHooks)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Go is required to build from source: https://go.dev/dl/' }
New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
$exe = Join-Path $BinDir 'session-index.exe'
$env:CGO_ENABLED = '0'
Push-Location $root
try { go build -ldflags '-s -w' -o $exe ./cmd/session-index } finally { Pop-Location }
& $exe version
if ($env:Path -notlike "*$BinDir*") { Write-Host "NOTE: add $BinDir to your PATH." }
if (-not $NoHooks) { & $exe install-hooks --skill }
& $exe doctor
Write-Host 'Next: session-index backfill --embed'
