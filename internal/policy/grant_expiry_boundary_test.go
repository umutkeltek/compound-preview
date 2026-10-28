package policy

import (
	"testing"
	"time"
)

func TestStandingGrantExpiryHalfOpenAcrossConsumers(t *testing.T) {
	expiry := fixedNow.Add(time.Hour)
	grant := StandingGrant{ID: "grant_boundary", Capability: "deploy.run", MutationClass: "public",
		Reversibility: "manual", BlastRadius: "public", TargetPath: "/synthetic/project/**",
		DesktopSurface: DesktopSurfaceOperatorBrowser, NotBefore: fixedNow.Format(time.RFC3339Nano),
		ExpiresAt: expiry.Format(time.RFC3339Nano)}
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{grant}
	action := mkAction("public", "manual", "/synthetic/project/page.html", "public")
	action.Capability = "deploy.run"
	for _, row := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before-start", fixedNow.Add(-time.Nanosecond), false},
		{"at-start", fixedNow, true},
		{"before-expiry", expiry.Add(-time.Nanosecond), true},
		{"at-expiry", expiry, false},
		{"after-expiry", expiry.Add(time.Nanosecond), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, _, ordinary := matchGrant(cfg.StandingGrants, action, cfg, row.at)
			_, scope := autonomyEnvelopeScopeGrant(action, cfg, row.at)
			desktop := desktopSurfaceGranted(cfg.StandingGrants, DesktopSurfaceOperatorBrowser, row.at)
			if ordinary != row.want || scope != row.want || desktop != row.want {
				t.Fatalf("eligible ordinary=%t scope=%t desktop=%t; want %t at %s", ordinary, scope, desktop, row.want, row.at.Format(time.RFC3339Nano))
			}
		})
	}
	cfg.StandingGrants[0].ExpiresAt = ""
	_, _, ordinary := matchGrant(cfg.StandingGrants, action, cfg, expiry.Add(time.Hour))
	_, scope := autonomyEnvelopeScopeGrant(action, cfg, expiry.Add(time.Hour))
	desktop := desktopSurfaceGranted(cfg.StandingGrants, DesktopSurfaceOperatorBrowser, expiry.Add(time.Hour))
	if !ordinary || !scope || !desktop {
		t.Fatal("grant without expiry changed")
	}
}
