package buildinfo

import (
	"runtime/debug"
	"testing"
)

// fake replaces the build information for one test.
func fake(t *testing.T, version string, info *debug.BuildInfo, ok bool) {
	t.Helper()
	oldVersion, oldRead := Version, readBuildInfo
	t.Cleanup(func() { Version, readBuildInfo = oldVersion, oldRead })
	Version = version
	readBuildInfo = func() (*debug.BuildInfo, bool) { return info, ok }
}

func module(path, version string) *debug.BuildInfo {
	return &debug.BuildInfo{Main: debug.Module{Path: path, Version: version}}
}

func TestNameIsTheLastElementOfTheModulePath(t *testing.T) {
	fake(t, "", module("github.com/okdp/okdp-control-plane-server", "(devel)"), true)
	if got := Name("fallback"); got != "okdp-control-plane-server" {
		t.Errorf("Name = %q", got)
	}
}

func TestNameFallsBackWithoutBuildInfo(t *testing.T) {
	fake(t, "", nil, false)
	if got := Name("OKDP control plane"); got != "OKDP control plane" {
		t.Errorf("Name = %q", got)
	}
	fake(t, "", module("", ""), true)
	if got := Name("OKDP control plane"); got != "OKDP control plane" {
		t.Errorf("Name with an empty module path = %q", got)
	}
}

func TestTheRealBinaryIsNamedAfterTheModule(t *testing.T) {
	if got := Name("fallback"); got != "okdp-control-plane-server" {
		t.Errorf("Name = %q", got)
	}
}

func TestVersionSetByTheLinkerWins(t *testing.T) {
	fake(t, "0.9.0", module("github.com/okdp/okdp-control-plane-server", "v0.8.0"), true)
	if got := CurrentVersion(); got != "0.9.0" {
		t.Errorf("CurrentVersion = %q", got)
	}
	fake(t, "v1.2.3", nil, false)
	if got := CurrentVersion(); got != "1.2.3" {
		t.Errorf("CurrentVersion with a leading v = %q, want 1.2.3", got)
	}
}

func TestVersionFallsBackToTheModuleVersion(t *testing.T) {
	fake(t, "", module("github.com/okdp/okdp-control-plane-server", "v0.8.1"), true)
	if got := CurrentVersion(); got != "0.8.1" {
		t.Errorf("CurrentVersion = %q", got)
	}
}

func TestVersionIsDevOtherwise(t *testing.T) {
	for name, info := range map[string]*debug.BuildInfo{
		"devel":   module("github.com/okdp/okdp-control-plane-server", "(devel)"),
		"empty":   module("github.com/okdp/okdp-control-plane-server", ""),
		"no info": nil,
	} {
		fake(t, "", info, info != nil)
		if got := CurrentVersion(); got != "dev" {
			t.Errorf("%s: CurrentVersion = %q, want dev", name, got)
		}
	}
}
