<#
.SYNOPSIS
  Unlock an AutoClaw desktop credential (auth.json) on the machine that owns it.

.DESCRIPTION
  AutoClaw stores its tokens with Electron safeStorage, i.e. Chromium's os_crypt
  scheme. There are two layers, and only the first is Windows-only:

    1. DPAPI  - %APPDATA%\AutoClaw\Local State holds os_crypt.encrypted_key, which
                is base64(DPAPI(aes_key)) prefixed with the ASCII marker "DPAPI".
                Unwrapping it needs Windows AND the user account that created it,
                which is why this script must run on YOUR machine.
    2. AES-256-GCM - the "enc:" values in auth.json:

         [0..3)    "v10"          os_crypt version marker
         [3..15)   nonce          12 bytes
         [15..n)   ciphertext
         [n..n+16) tag            GCM auth tag, at the TAIL

  Layer 2 is pure arithmetic, so this script can finish the job locally: it
  prints the plaintext tokens and never the AES key. That is the point - the key
  unlocks every safeStorage secret in the app, the tokens unlock only AutoClaw.

  If this machine has no AES-GCM (Windows PowerShell 5.1 predates .NET Core 3),
  the script says so and offers -PrintKey to emit the key instead, for decryption
  elsewhere. Prefer the default path.

.PARAMETER AuthJson
  Path to auth.json. Defaults to %APPDATA%\AutoClaw\auth.json.

.PARAMETER PrintKey
  Emit the unwrapped AES key (base64) instead of the tokens. Only for the fallback
  case where this machine cannot run AES-GCM. Treat the output as a password.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\autoclaw_unlock.ps1

.EXAMPLE
  pwsh -File .\autoclaw_unlock.ps1 -PrintKey
#>
param(
    [string]$AuthJson = "$env:APPDATA\AutoClaw\auth.json",
    [switch]$PrintKey
)

$ErrorActionPreference = 'Stop'

function Get-OsCryptKey {
    <#
      Unwrap the AES key. Returns a byte[32].

      The 5-byte "DPAPI" prefix is a marker, not part of the blob: handing it to
      Unprotect makes DPAPI reject the input, which is why it is stripped here.
    #>
    param([string]$LocalState = "$env:APPDATA\AutoClaw\Local State")

    if (-not (Test-Path $LocalState)) {
        throw "Local State not found at $LocalState - is AutoClaw installed for this user?"
    }
    $state = Get-Content $LocalState -Raw | ConvertFrom-Json
    $b64 = $state.os_crypt.encrypted_key
    if (-not $b64) { throw "os_crypt.encrypted_key is missing from Local State" }

    $raw = [Convert]::FromBase64String($b64)
    if ($raw.Length -le 5) { throw "encrypted_key is too short ($($raw.Length) bytes)" }
    $marker = [Text.Encoding]::ASCII.GetString($raw, 0, 5)
    if ($marker -ne 'DPAPI') {
        # v20 (app-bound) keys do not use this shape; say so instead of failing cryptically.
        throw "encrypted_key does not start with DPAPI (found '$marker'); this build may use app-bound encryption"
    }
    $blob = $raw[5..($raw.Length - 1)]

    Add-Type -AssemblyName System.Security
    $key = [Security.Cryptography.ProtectedData]::Unprotect(
        $blob, $null, [Security.Cryptography.DataProtectionScope]::CurrentUser)
    if ($key.Length -ne 32) { throw "unwrapped key is $($key.Length) bytes, expected 32" }
    return $key
}

function Test-AesGcm {
    <# AES-GCM needs .NET Core 3+ (PowerShell 7). 5.1 does not have it. #>
    try { $null = [System.Security.Cryptography.AesGcm]; return $true } catch { return $false }
}

function Decrypt-OsCryptValue {
    <#
      Decrypt one "enc:" field with AES-256-GCM.

      The tag sits at the tail (Electron's postfix layout), so it is split off
      before calling GCM, which wants nonce / ciphertext / tag as separate
      arguments.
    #>
    param([string]$Value, [byte[]]$Key)

    if (-not $Value.StartsWith('enc:')) {
        # Older builds and hand-edited files can hold plaintext; that is not a failure.
        return $Value
    }
    $blob = [Convert]::FromBase64String($Value.Substring(4))
    $prefix = [Text.Encoding]::ASCII.GetString($blob, 0, 3)
    if ($prefix -ne 'v10') { throw "unexpected os_crypt prefix '$prefix' (expected v10)" }

    $payload = $blob[3..($blob.Length - 1)]
    if ($payload.Length -le 28) { throw "encrypted payload is too short ($($payload.Length) bytes)" }

    $nonce = $payload[0..11]
    $body = $payload[12..($payload.Length - 1)]
    $tag = $body[($body.Length - 16)..($body.Length - 1)]
    $ct = if ($body.Length - 16 -gt 0) { $body[0..($body.Length - 17)] } else { @() }

    # AesGcm's ctor differs across .NET versions: .NET 8 requires a tag size.
    try { $gcm = [System.Security.Cryptography.AesGcm]::new($Key, 16) }
    catch { $gcm = [System.Security.Cryptography.AesGcm]::new($Key) }

    try {
        $plain = New-Object byte[] $ct.Length
        $gcm.Decrypt($nonce, $ct, $tag, $plain, $null)
        $text = [Text.Encoding]::UTF8.GetString($plain)
    } finally { $gcm.Dispose() }

    if ($text.StartsWith('Bearer ')) { $text = $text.Substring(7) }
    return $text.Trim()
}

# ---- main ----

if (-not (Test-Path $AuthJson)) {
    throw "auth.json not found at $AuthJson"
}

$key = Get-OsCryptKey

if ($PrintKey) {
    Write-Output ([Convert]::ToBase64String($key))
    Write-Output ''
    Write-Output '# Treat the line above as a password: it unlocks every AutoClaw safeStorage secret.'
    return
}

if (-not (Test-AesGcm)) {
    Write-Output 'This machine cannot run AES-GCM (Windows PowerShell 5.1 lacks .NET Core).'
    Write-Output 'Re-run with -PrintKey to emit the key instead, or use pwsh (PowerShell 7).'
    exit 2
}

$auth = Get-Content $AuthJson -Raw | ConvertFrom-Json
$token = Decrypt-OsCryptValue -Value $auth.token -Key $key
$refresh = Decrypt-OsCryptValue -Value $auth.refreshToken -Key $key

$credential = [ordered]@{
    format        = 'autoclaw-credential-v1'
    access_token  = $token
    refresh_token = $refresh
    device_id     = $auth.deviceId
    user_id       = "$($auth.userInfo.user_id)"
    region        = 'cn'
}

Write-Output ''
Write-Output 'access_token :'
Write-Output $token
Write-Output ''
Write-Output 'refresh_token:'
Write-Output $refresh
Write-Output ''
Write-Output 'credential payload (paste this into the gateway import box):'
Write-Output ($credential | ConvertTo-Json -Compress)
Write-Output ''
Write-Output '# The AES key was NOT printed. It never leaves this machine.'
