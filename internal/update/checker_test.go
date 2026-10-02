package update

import (
	"context"
	"testing"
)

type releaseSourceStub struct {
	releases []Release
	err      error
	calls    int
}

func (s *releaseSourceStub) ListReleases(context.Context) ([]Release, error) {
	s.calls++
	return s.releases, s.err
}

func TestCheckerTargetsLatestReleaseAndReportsSkippedVersions(t *testing.T) {
	source := &releaseSourceStub{releases: []Release{
		{TagName: "v0.2.4", Name: "latest"},
		{TagName: "v0.2.2", Name: "next"},
		{TagName: "v0.2.3", Name: "middle"},
	}}
	checker := NewChecker("v0.2.1", source)

	info, err := checker.Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Managed || !info.HasUpdate || info.NextVersion != "v0.2.4" {
		t.Fatalf("info = %+v", info)
	}
	if len(info.SkippedVersions) != 2 || info.SkippedVersions[0] != "v0.2.2" || info.SkippedVersions[1] != "v0.2.3" {
		t.Fatalf("skipped = %v", info.SkippedVersions)
	}
	if info.Release == nil || info.Release.Name != "latest" {
		t.Fatalf("release = %+v", info.Release)
	}
	if len(info.RecentReleases) != 4 || info.RecentReleases[0].TagName != "v0.2.4" || info.RecentReleases[3].TagName != "v0.2.1" {
		t.Fatalf("recent = %+v", info.RecentReleases)
	}
}

func TestCheckerDisablesDevelopmentBuilds(t *testing.T) {
	source := &releaseSourceStub{releases: []Release{{TagName: "v0.2.2"}}}
	checker := NewChecker("dev", source)

	info, err := checker.Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if info.Managed || info.HasUpdate || info.NextVersion != "" {
		t.Fatalf("a development build must not be offered an update: %+v", info)
	}
	if source.calls != 1 {
		t.Fatalf("calls=%d want the release feed consulted for the history", source.calls)
	}
	if len(info.RecentReleases) != 2 || info.RecentReleases[0].TagName != "dev" || info.RecentReleases[1].TagName != "v0.2.2" {
		t.Fatalf("recent=%+v want the running build then v0.2.2", info.RecentReleases)
	}
}

func TestCheckerForkBuildListsUpstreamHistory(t *testing.T) {
	source := &releaseSourceStub{releases: []Release{
		{TagName: "v0.6.3"},
		{TagName: "v0.6.2"},
		{TagName: "v0.6.4", Draft: true},
		{TagName: "nightly", Prerelease: true},
	}}
	checker := NewChecker("v0.6.18-fork.4931d1b", source)

	info, err := checker.Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if info.Managed || info.HasUpdate || info.NextVersion != "" {
		t.Fatalf("a fork build must not be offered an update: %+v", info)
	}
	if len(info.RecentReleases) != 3 {
		t.Fatalf("recent=%+v want the running build plus the two published releases", info.RecentReleases)
	}
	if info.RecentReleases[0].TagName != "v0.6.18-fork.4931d1b" || info.RecentReleases[1].TagName != "v0.6.3" || info.RecentReleases[2].TagName != "v0.6.2" {
		t.Fatalf("recent=%+v", info.RecentReleases)
	}
}

func TestCheckerForkBuildToleratesReleaseListingFailure(t *testing.T) {
	source := &releaseSourceStub{err: context.DeadlineExceeded}
	info, err := NewChecker("v0.6.18-fork.4931d1b", source).Check(context.Background(), true)
	if err != nil {
		t.Fatalf("a failed listing must not fail the check: %v", err)
	}
	if len(info.RecentReleases) != 0 || info.Warning == "" {
		t.Fatalf("info=%+v want no history and a warning", info)
	}
}

func TestCheckerForceCheckDoesNotUseStaleRelease(t *testing.T) {
	source := &releaseSourceStub{releases: []Release{{TagName: "v0.2.2"}}}
	checker := NewChecker("v0.2.1", source)
	if _, err := checker.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	source.err = context.DeadlineExceeded
	if _, err := checker.Check(context.Background(), true); err == nil {
		t.Fatal("forced update check unexpectedly used stale cache")
	}
}
