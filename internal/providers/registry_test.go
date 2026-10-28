package providers

import "testing"

func TestListIncludesEveryRegisteredDescriptor(t *testing.T) {
	listed := List()
	seen := make(map[string]struct{}, len(listed))
	for _, descriptor := range listed {
		if _, duplicate := seen[descriptor.ID]; duplicate {
			t.Fatalf("duplicate provider %q", descriptor.ID)
		}
		seen[descriptor.ID] = struct{}{}
	}
	for id := range registry {
		if _, ok := seen[id]; !ok {
			t.Fatalf("provider %q is registered but missing from List", id)
		}
	}
	codex, ok := Get("codex")
	if !ok {
		t.Fatal("codex descriptor missing")
	}
	if codex.Runtime != RuntimeInProcess || codex.DefaultRegion != "global" || !codex.SupportsAuthType(AuthOAuth) {
		t.Fatalf("unexpected codex descriptor: %+v", codex)
	}
}

// The fork carries three in-process adapters upstream does not ship (zcode,
// raccoon, antigravity) alongside the shared families. A merge that resolves
// registry.go toward the upstream side silently drops them, so pin the full
// set here: losing one takes its accounts offline.
func TestListKeepsForkAndSharedProviders(t *testing.T) {
	listed := List()
	seen := make(map[string]struct{}, len(listed))
	for _, descriptor := range listed {
		seen[descriptor.ID] = struct{}{}
	}
	for _, want := range []string{
		"qoder", "workbuddy", "trae", "devin", "command", "codex",
		"zhipu", "qwenwork", "kimi", "meta",
		"zcode", "raccoon", "antigravity",
	} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("provider %q missing from List", want)
		}
	}
}

// muse is a display alias only: it must resolve to the meta family so accounts
// and routing never store a second provider id.
func TestMuseAliasResolvesToMeta(t *testing.T) {
	descriptor, ok := Get("muse")
	if !ok {
		t.Fatal("muse alias must resolve")
	}
	if descriptor.ID != "meta" {
		t.Fatalf("muse resolved to %q, want meta", descriptor.ID)
	}
	if descriptor.DefaultRegion != "global" || !descriptor.SupportsAuthType(AuthOAuth) {
		t.Fatalf("unexpected meta descriptor: %+v", descriptor)
	}
}

func TestQwenWorkAdvertisesDailyCheckin(t *testing.T) {
	for _, region := range []string{"global", "cn"} {
		policy, ok := CheckinFor("qwenwork", region)
		if !ok || policy == nil {
			t.Fatalf("region %s: qwenwork check-in not advertised", region)
		}
		if policy.Timezone != "Asia/Shanghai" {
			t.Fatalf("region %s: timezone=%q", region, policy.Timezone)
		}
	}
	descriptor, ok := Get("qwenwork")
	if !ok || !descriptor.SupportsCheckin() {
		t.Fatal("qwenwork descriptor must support check-in")
	}
}
