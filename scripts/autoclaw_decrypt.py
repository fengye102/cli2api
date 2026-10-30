#!/usr/bin/env python3
"""Decrypt AutoClaw auth.json values once the Chromium OSCrypt AES key is known.

Why this exists: AutoClaw is an Electron app and stores its tokens with
`safeStorage`, i.e. Chromium's os_crypt scheme. Two steps are involved:

  1. DPAPI unwrap  -- `%APPDATA%/AutoClaw/Local State` holds
     `os_crypt.encrypted_key` = base64(DPAPI(aes_key)). The first 5 bytes of the
     decoded blob are the ASCII marker "DPAPI"; the rest is the DPAPI blob.
     Unwrapping it needs Windows AND the same user account, so it cannot run
     here.
  2. AES-256-GCM   -- the `enc:` values in auth.json. Layout:

         [0..3)    "v10"          os_crypt version marker
         [3..15)   nonce          12 bytes
         [15..n)   ciphertext
         [n..n+16) tag            GCM auth tag, POSTFIX (at the tail)

Step 2 is pure arithmetic and platform-independent, which is why it can run
here: hand over the 32-byte key from step 1 and this script produces the token.

Usage:
    python3 autoclaw_decrypt.py <base64-or-hex AES key> <auth.json path>

The plaintext is the token itself (not JSON). Both fields come out carrying a
"Bearer " prefix, which is stripped here.
"""

import base64
import json
import sys

OS_CRYPT_PREFIX = b"v10"
NONCE_LEN = 12
TAG_LEN = 16
AES_KEY_LEN = 32


def load_key(raw: str) -> bytes:
    """Accept the key as base64 or hex, since either is easy to paste."""
    raw = raw.strip()
    for decoder in (base64.b64decode, bytes.fromhex):
        try:
            key = decoder(raw)
        except Exception:
            continue
        if len(key) == AES_KEY_LEN:
            return key
    raise SystemExit(
        f"could not read a {AES_KEY_LEN}-byte key from that value "
        f"(got {len(raw)} chars; paste base64 or hex, not the DPAPI blob)"
    )


def decrypt_value(value: str, key: bytes) -> str:
    """Decrypt one auth.json field.

    Values without the `enc:` prefix are returned verbatim: older AutoClaw
    builds and hand-edited files can hold plaintext, and treating that as a
    decryption failure would be wrong.
    """
    if not value.startswith("enc:"):
        return value

    blob = base64.b64decode(value[4:])
    if blob[:3] != OS_CRYPT_PREFIX:
        raise ValueError(f"unexpected os_crypt version prefix: {blob[:3]!r}")

    payload = blob[3:]
    # Guard copied from the source implementation: <= 12 + 16 is malformed
    # (exactly 28 bytes counts as malformed too, not just fewer).
    if len(payload) <= NONCE_LEN + TAG_LEN:
        raise ValueError("encrypted payload is too short")

    nonce = payload[:NONCE_LEN]
    # The tag sits at the TAIL (Electron's postfix layout), not right after
    # the nonce. Handing "ciphertext + tag" to the AEAD as one blob matches
    # that layout, which is what the cryptography library expects.
    body = payload[NONCE_LEN:]

    try:
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    except ImportError:
        raise SystemExit(
            "this script needs the `cryptography` package: pip install cryptography"
        )

    plain = AESGCM(key).decrypt(nonce, body, None)
    text = plain.decode("utf-8", "replace")
    if text.startswith("Bearer "):
        text = text[len("Bearer "):]
    return text.strip()


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 2
    key = load_key(sys.argv[1])
    with open(sys.argv[2], "r", encoding="utf-8") as handle:
        auth = json.load(handle)

    out = {}
    for field in ("token", "refreshToken"):
        value = auth.get(field, "")
        if not value:
            out[field] = ""
            continue
        try:
            out[field] = decrypt_value(value, key)
        except Exception as error:
            out[field] = f"<failed: {error}>"

    # Emit the canonical credential payload the gateway imports directly.
    payload = {
        "format": "autoclaw-credential-v1",
        "access_token": out["token"],
        "refresh_token": out["refreshToken"],
        "device_id": auth.get("deviceId", ""),
        "user_id": str((auth.get("userInfo") or {}).get("user_id", "")),
        "region": "cn",
    }
    print(json.dumps({"decrypted": out, "credential": payload}, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
