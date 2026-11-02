package providers

import "testing"

// The registry already resolved the shorthand "muse" to the meta family, so
// the web-session provider must live under its own id and must not be shadowed
// by that alias.
func TestMuseWebDescriptorRegistered(t *testing.T) {
	descriptor, ok := Get("muse-web")
	if !ok {
		t.Fatal("Get(\"muse-web\") not found")
	}
	if descriptor.ID != "muse-web" {
		t.Fatalf("descriptor.ID = %q, want muse-web", descriptor.ID)
	}
	if _, ok := descriptor.Region("global"); !ok {
		t.Fatal("muse-web has no global region")
	}
	if !descriptor.SupportsCredentialFormat("muse-cookie-v1") {
		t.Fatal("muse-web must declare the muse-cookie-v1 credential format")
	}
	if descriptor.Capabilities.PATLogin || descriptor.Capabilities.Login {
		t.Fatal("muse-web has no login round: its credential is an imported browser session")
	}

	alias, ok := Get("muse")
	if !ok || alias.ID != "meta" {
		t.Fatalf("Get(\"muse\") = %q, want the meta family alias to stay intact", alias.ID)
	}

	var listed bool
	for _, candidate := range List() {
		if candidate.ID == "muse-web" {
			listed = true
		}
	}
	if !listed {
		t.Fatal("muse-web is missing from List(), so the console would never offer it")
	}
}
