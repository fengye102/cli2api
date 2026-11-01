package autoclaw

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
)

// Windows DPAPI blobs.
//
// On a Windows install the desktop client stores its tokens as a DPAPI blob
// (CryptProtectData). A blob is encrypted with a master key that the operating
// system itself protects: it is derived from the logged-on user's password (or,
// for machine scope, from an LSA secret only that machine's registry holds).
// The ciphertext is therefore bound to one Windows profile on one machine, and
// nothing running on this server can open it - no amount of parsing helps,
// because the key material is not in the file.
//
// The old import path did not know that. It read the blob as if it were a
// token, stored it, and the account then failed every request with a 401 that
// looked like an expired login. What this file does is recognize the blob and
// say so, with the two ways forward: log in from the console (see login.go -
// the tokens are then minted here and refresh themselves), or decrypt it on
// that Windows box with the one-liner below and paste the result.
//
// Only the fixed header is parsed here (version, provider, master key id,
// flags, description, algorithms). Everything after it is a variable-length
// chain whose offsets would only matter if the key material were available,
// and parsing it would add a way for a malformed file to be misread as
// something else.

// dpapiProviderGUID is the well-known DPAPI provider GUID
// df9d8cd0-1501-11d1-8c7a-00c04fc297eb in on-disk (little-endian) form.
var dpapiProviderGUID = []byte{
	0xd0, 0x8c, 0x9d, 0xdf, 0x01, 0x15, 0xd1, 0x11,
	0x8c, 0x7a, 0x00, 0xc0, 0x4f, 0xc2, 0x97, 0xeb,
}

// cryptProtectLocalMachine is CRYPTPROTECT_LOCAL_MACHINE: the blob is bound to
// the machine rather than to the user who wrote it.
const cryptProtectLocalMachine = 0x4

// dpapiCryptAlgorithms names the block ciphers a blob can declare.
var dpapiCryptAlgorithms = map[uint32]string{
	0x6602: "3DES (112-bit)",
	0x6603: "3DES (168-bit)",
	0x6610: "AES-256",
}

// DPAPIBlob is what the header of a DPAPI blob tells us.
type DPAPIBlob struct {
	// MasterKeyGUID identifies the master key that would be needed.
	MasterKeyGUID string
	// MachineScope is true when the blob was written with
	// CRYPTPROTECT_LOCAL_MACHINE (any account on the machine can open it).
	MachineScope bool
	// Description is the caller's own label, usually empty.
	Description string
	// Algorithm is the block cipher the payload uses.
	Algorithm string
}

// String renders the blob for a log line.
func (b DPAPIBlob) String() string {
	scope := "user"
	if b.MachineScope {
		scope = "machine"
	}
	return fmt.Sprintf("DPAPI blob (dpapi scope=%s, master key %s, %s)", scope, b.MasterKeyGUID, b.Algorithm)
}

// DescribeDPAPIBlob parses the fixed header of a DPAPI blob.
func DescribeDPAPIBlob(raw []byte) (DPAPIBlob, error) {
	// version(4) provider(16) masterKeyVersion(4) masterKeyGUID(16) flags(4)
	// descriptionLen(4) = 48 bytes before the description.
	if len(raw) < 48 {
		return DPAPIBlob{}, fmt.Errorf("not a DPAPI blob: %d bytes is too short", len(raw))
	}
	if version := binary.LittleEndian.Uint32(raw[0:4]); version != 1 {
		return DPAPIBlob{}, fmt.Errorf("not a DPAPI blob: version %d", version)
	}
	provider := raw[4:20]
	if !bytesEqual(provider, dpapiProviderGUID) {
		return DPAPIBlob{}, fmt.Errorf("not a DPAPI blob: provider is %s", guidString(provider))
	}
	blob := DPAPIBlob{
		MasterKeyGUID: guidString(raw[24:40]),
		MachineScope:  binary.LittleEndian.Uint32(raw[40:44])&cryptProtectLocalMachine != 0,
	}
	descriptionLen := binary.LittleEndian.Uint32(raw[44:48])
	offset := 48
	if descriptionLen > 0 {
		end := offset + int(descriptionLen)*2
		if end > len(raw) {
			return DPAPIBlob{}, fmt.Errorf("not a DPAPI blob: description runs past the end")
		}
		blob.Description = utf16LEString(raw[offset:end])
		offset = end
	}
	if offset+4 > len(raw) {
		// A header that stops before declaring a cipher is still a DPAPI blob;
		// report it with an unknown algorithm rather than failing the import
		// diagnosis over a truncated tail.
		blob.Algorithm = "unknown"
		return blob, nil
	}
	algorithm := binary.LittleEndian.Uint32(raw[offset : offset+4])
	blob.Algorithm = dpapiCryptAlgorithms[algorithm]
	if blob.Algorithm == "" {
		blob.Algorithm = fmt.Sprintf("algorithm 0x%x", algorithm)
	}
	return blob, nil
}

// LooksLikeDPAPI reports whether a value is a DPAPI blob, either as raw bytes,
// as base64, or as base64 inside a JSON-ish string. It returns what the header
// said when it is one.
func LooksLikeDPAPI(value string) (DPAPIBlob, bool) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < 48 {
		return DPAPIBlob{}, false
	}
	trimmed = strings.Trim(trimmed, `"`)
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(trimmed); err == nil {
			if blob, err := DescribeDPAPIBlob(decoded); err == nil {
				return blob, true
			}
		}
	}
	return DPAPIBlob{}, false
}

// dpapiImportError is the message an operator gets instead of a stored blob.
//
// It names the master key, states the one hard fact (the key is not in the
// file), and gives both ways out. The decrypt one-liner is deliberately
// complete: pasting a command that works beats pointing at documentation.
func dpapiImportError(blob DPAPIBlob) error {
	scope := "the Windows user who created it"
	if blob.MachineScope {
		scope = "that Windows machine"
	}
	return fmt.Errorf(
		"autoclaw credential is a Windows DPAPI blob (%s), which is encrypted to %s - "+
			"the master key it needs (dpapi_machine=0 dpapi_userkey=%s) is not in this file, so it cannot be opened on this server. "+
			"Two ways to get the account in: (1) log in from the console - 国内版 uses a phone + SMS code, 国际版 uses OAuth - "+
			"which mints the tokens here and keeps them refreshed; or (2) decrypt it on that Windows machine and paste the output: "+
			`powershell -NoProfile -Command "$b=[Convert]::FromBase64String('<paste the value from auth.json>'); `+
			`[Text.Encoding]::UTF8.GetString([Security.Cryptography.ProtectedData]::Unprotect($b,$null,'CurrentUser'))"`,
		blob, scope, blob.MasterKeyGUID)
}

// guidString formats a GUID in its canonical mixed-endian form.
func guidString(raw []byte) string {
	if len(raw) < 16 {
		return ""
	}
	return fmt.Sprintf("%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		binary.LittleEndian.Uint32(raw[0:4]),
		binary.LittleEndian.Uint16(raw[4:6]),
		binary.LittleEndian.Uint16(raw[6:8]),
		raw[8], raw[9], raw[10], raw[11], raw[12], raw[13], raw[14], raw[15])
}

// utf16LEString decodes a NUL-terminated UTF-16LE string.
func utf16LEString(raw []byte) string {
	units := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		unit := binary.LittleEndian.Uint16(raw[i : i+2])
		if unit == 0 {
			break
		}
		units = append(units, unit)
	}
	return strings.TrimSpace(string(utf16Decode(units)))
}

// utf16Decode converts UTF-16 code units to a Go string, including surrogate
// pairs; descriptions are user text and may hold any BMP character.
func utf16Decode(units []uint16) []rune {
	out := make([]rune, 0, len(units))
	for i := 0; i < len(units); i++ {
		unit := units[i]
		if unit >= 0xd800 && unit <= 0xdbff && i+1 < len(units) {
			next := units[i+1]
			if next >= 0xdc00 && next <= 0xdfff {
				out = append(out, rune(0x10000+((int32(unit)-0xd800)<<10)+(int32(next)-0xdc00)))
				i++
				continue
			}
		}
		out = append(out, rune(unit))
	}
	return out
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
