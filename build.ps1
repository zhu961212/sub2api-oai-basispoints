# 构建、测试并打包 Sub2API 插件（Windows）。
#
#   .\build.ps1
#   .\build.ps1 -SigningKey C:\secure\publisher.private -KeyId my-publisher-v1
param(
    [string]$Targets = "windows/amd64,linux/amd64",
    [string]$SigningKey = "",
    [string]$KeyId = "",
    [string]$Output = "",
    [switch]$SkipBuild
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
$KeyId = $KeyId.Trim()

Write-Host "==> go test ./... -skip TestLive -count=1 -timeout 120s"
go test ./... -skip TestLive -count=1 -timeout 120s
if ($LASTEXITCODE -ne 0) { throw "Go tests failed; packaging stopped." }

Write-Host "==> go vet ./..."
go vet ./...
if ($LASTEXITCODE -ne 0) { throw "Go vet failed; packaging stopped." }

Write-Host "==> node --check ui/assets/*.js"
$node = Get-Command node -ErrorAction SilentlyContinue
if ($node) {
    & node --check ui/assets/bridge-v1.js
    if ($LASTEXITCODE -ne 0) { throw "Bridge syntax check failed; packaging stopped." }
    & node --check ui/assets/app.js
    if ($LASTEXITCODE -ne 0) { throw "UI syntax check failed; packaging stopped." }
    Write-Host "==> node --test tools/ui.test.cjs"
    & node --test tools/ui.test.cjs
    if ($LASTEXITCODE -ne 0) { throw "UI regression tests failed; packaging stopped." }
} else {
    Write-Host "    node not found, skipping UI syntax check"
}

$arguments = @("run", "./tools/packager", "-targets", $Targets)
if ($SkipBuild) { $arguments += "-skip-build" }
if ($Output) { $arguments += @("-output", $Output) }
if ($SigningKey) { $arguments += @("-signing-key", $SigningKey) }
if ($KeyId) { $arguments += @("-key-id", $KeyId) }

Write-Host "==> go $($arguments -join ' ')"
go @arguments
if ($LASTEXITCODE -ne 0) { throw "Plugin packaging failed." }

# 独立校验：不复用打包器的自检逻辑，重新算哈希并（在有公钥时）验证签名。
$manifest = Get-Content manifest.source.json -Raw | ConvertFrom-Json
$package = if ($Output) { $Output } else { Join-Path "dist" "$($manifest.id)-$($manifest.version).s2plugin" }
$python = Get-Command python -ErrorAction SilentlyContinue
if ($SigningKey -and -not $python) { throw "Python is required for independent signature verification." }
if (-not (Test-Path -LiteralPath $package)) { throw "Expected plugin package was not created: $package" }
if ($python) {
    $verifyArgs = @("tools/verify_package.py", $package)
    $verifyKey = "build/keys/publisher.public"
    if ($SigningKey) {
        $matchingKey = [System.IO.Path]::ChangeExtension($SigningKey, "public")
        if (-not (Test-Path -LiteralPath $matchingKey)) { throw "Matching publisher public key is required: $matchingKey" }
        $verifyKey = $matchingKey
        $verifyArgs += @("--require-signature", "--expected-key-id", $KeyId)
    }
    if (Test-Path -LiteralPath $verifyKey) {
        $verifyArgs += @("--public-key", $verifyKey)
    }
    Write-Host "==> python $($verifyArgs -join ' ')"
    & python -X utf8 @verifyArgs
    if ($LASTEXITCODE -ne 0) { throw "Independent package verification failed." }
}
