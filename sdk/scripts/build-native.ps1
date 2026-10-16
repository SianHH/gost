# Build gost.dll (the native shared library) for driving the FFI from a desktop
# Flutter app. This is the fast iteration loop: no device or emulator needed.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File scripts/build-native.ps1
#   powershell -ExecutionPolicy Bypass -File scripts/build-native.ps1 -Tags gost_all
#
# Output: build/gost.dll + gost.h
#
# CGO is required: -buildmode=c-shared is a cgo feature, and the Go install on
# this machine defaults to CGO_ENABLED=0. A C compiler must be on PATH.

param(
    [string]$OutDir = "build",
    [string]$Tags = ""
)

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

$versionFile = "../gost/cmd/gost/version.go"
$version = "dev"
if (Test-Path $versionFile) {
    $m = Select-String -Path $versionFile -Pattern 'version = "(.*)"' | Select-Object -First 1
    if ($m) { $version = $m.Matches[0].Groups[1].Value }
}

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$env:CGO_ENABLED = "1"

$buildArgs = @(
    "build",
    "-buildmode=c-shared",
    "-trimpath",
    "-ldflags=-s -w -X github.com/go-gost/sdk/engine.version=$version",
    "-o", "$OutDir/gost.dll",
    "."
)
if ($Tags) { $buildArgs = @("build", "-tags=$Tags") + $buildArgs[1..($buildArgs.Length - 1)] }

Write-Host "==> windows/amd64 (CGO_ENABLED=1)"
go @buildArgs

Write-Host ""
Write-Host "version: $version"
Get-ChildItem "$OutDir/*.dll", "$OutDir/*.h" | Select-Object Name, Length
