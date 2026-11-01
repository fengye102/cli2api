package autoclaw

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
)

// dpapiBlobFixture builds a DPAPI header the way CryptProtectData writes it.
// Only the fixed part is real: the payload is irrelevant here, because the point
// of the parser is to recognize a blob and name the key it would need.
func dpapiBlobFixture(machineScope bool, description string) []byte {
	const masterKeyGUID = "9c8b1e44-3a71-4a4d-95a0-2b7bd0b9d1c3"
	blob := make([]byte, 0, 128)
	appendUint32 := func(value uint32) {
		buffer := make([]byte, 4)
		binary.LittleEndian.PutUint32(buffer, value)
		blob = append(blob, buffer...)
	}
	appendUint32(1) // version
	blob = append(blob, dpapiProviderGUID...)
	appendUint32(2) // master key version
	// GUIDs on disk are stored with the first three fields little-endian.
	blob = append(blob, []byte{
		0x44, 0x1e, 0x8b, 0x9c, // Data1 = 9c8b1e44
		0x71, 0x3a, // Data2 = 3a71
		0x4d, 0x4a, // Data3 = 4a4d
		0x95, 0xa0, 0x2b, 0x7b, 0xd0, 0xb9, 0xd1, 0xc3,
	}...)
	flags := uint32(0x1) // CRYPTPROTECT_UI_FORBIDDEN
	if machineScope {
		flags |= cryptProtectLocalMachine
	}
	appendUint32(flags)
	units := make([]uint16, 0, len(description)+1)
	for _, r := range description {
		units = append(units, uint16(r))
	}
	units = append(units, 0)
	appendUint32(uint32(len(units)))
	for _, unit := range units {
		buffer := make([]byte, 2)
		binary.LittleEndian.PutUint16(buffer, unit)
		blob = append(blob, buffer...)
	}
	appendUint32(0x6610) // CALG_AES_256
	blob = append(blob, make([]byte, 32)...)
	_ = masterKeyGUID
	return blob
}

func TestDescribeDPAPIBlobReadsTheHeader(t *testing.T) {
	blob, err := DescribeDPAPIBlob(dpapiBlobFixture(false, "AutoClaw"))
	if err != nil {
		t.Fatalf("DescribeDPAPIBlob: %v", err)
	}
	if blob.MasterKeyGUID != "9c8b1e44-3a71-4a4d-95a0-2b7bd0b9d1c3" {
		t.Fatalf("master key guid = %q", blob.MasterKeyGUID)
	}
	if blob.MachineScope {
		t.Fatal("this fixture is user-scoped")
	}
	if blob.Description != "AutoClaw" {
		t.Fatalf("description = %q", blob.Description)
	}
	if blob.Algorithm != "AES-256" {
		t.Fatalf("algorithm = %q", blob.Algorithm)
	}
	if !strings.Contains(blob.String(), "scope=user") {
		t.Fatalf("String() = %q", blob.String())
	}

	machine, err := DescribeDPAPIBlob(dpapiBlobFixture(true, ""))
	if err != nil {
		t.Fatalf("DescribeDPAPIBlob(machine): %v", err)
	}
	if !machine.MachineScope || !strings.Contains(machine.String(), "scope=machine") {
		t.Fatalf("machine scope not detected: %+v", machine)
	}
}

func TestLooksLikeDPAPIAcceptsBase64(t *testing.T) {
	raw := dpapiBlobFixture(false, "")
	for name, encoded := range map[string]string{
		"std":    base64.StdEncoding.EncodeToString(raw),
		"raw":    base64.RawStdEncoding.EncodeToString(raw),
		"quoted": `"` + base64.StdEncoding.EncodeToString(raw) + `"`,
	} {
		if _, ok := LooksLikeDPAPI(encoded); !ok {
			t.Errorf("%s base64 was not recognized", name)
		}
	}
	if _, ok := LooksLikeDPAPI("this-is-just-a-token"); ok {
		t.Error("a plain token must not be read as a DPAPI blob")
	}
	if _, ok := LooksLikeDPAPI(""); ok {
		t.Error("an empty value must not be read as a DPAPI blob")
	}
}

func TestDescribeDPAPIBlobRejectsForeignBytes(t *testing.T) {
	if _, err := DescribeDPAPIBlob([]byte("short")); err == nil {
		t.Fatal("a short buffer is not a DPAPI blob")
	}
	wrongProvider := dpapiBlobFixture(false, "")
	wrongProvider[4] = 0x00
	if _, err := DescribeDPAPIBlob(wrongProvider); err == nil {
		t.Fatal("a foreign provider GUID is not a DPAPI blob")
	}
}

// TestImportRejectsDPAPIBlob pins the actual bug this replaces: the blob used to
// be stored as if it were a token, and the account then failed every request
// with a 401 that looked like an expired login.
func TestImportRejectsDPAPIBlob(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(dpapiBlobFixture(false, "autoclaw"))
	payload := []byte(`{"accessToken":"` + encoded + `"}`)
	_, err := (importer{region: RegionCN}).PrepareImport(payload)
	if err == nil {
		t.Fatal("a DPAPI blob must not be imported as a token")
	}
	for _, want := range []string{"DPAPI", "9c8b1e44", "FromBase64String"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %s", want, err)
		}
	}

	bare := []byte(encoded)
	if err := ValidateCredential(bare); err == nil || !strings.Contains(err.Error(), "DPAPI") {
		t.Fatalf("a bare blob pasted as a token must be refused too: %v", err)
	}
}
