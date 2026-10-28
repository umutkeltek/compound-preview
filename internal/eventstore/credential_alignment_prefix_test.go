package eventstore

import (
	"reflect"
	"testing"
	"time"
)

func TestCredentialAlignmentPrefixRoutesOnlyOtherWithoutChangingStorage(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		t.Run(map[bool]string{false: "scan", true: "scan_from"}[incremental], func(t *testing.T) {
			now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
			closeCompletionGate(t, store)
			seedAllFamilies(t, store, now)
			const id = "cpaf_0123456789ab"
			const schema = "compound.credential_password_alignment_fence.v0.1"
			if familyOf([]byte(`{"schema_version":"`+schema+`"}`), id) != "other" {
				t.Fatal("fence storage family changed")
			}
			putRecord(t, store, id, schema, now, "native launch intent")
			touched := observeParsedFamilies(t)
			var entries []Entry
			var e error
			if incremental {
				var page ScanPage
				page, e = store.ScanFrom(ScanOptions{Prefixes: []string{"cpaf_"}})
				entries = page.Entries
			} else {
				entries, e = store.Scan(ScanOptions{Prefixes: []string{"cpaf_"}})
			}
			if e != nil || len(entries) != 1 || entries[0].ID != id {
				t.Fatal("exact native launch intent was not read", e)
			}
			if !reflect.DeepEqual(touched(), []string{"other"}) {
				t.Fatal("alignment scan parsed unrelated event families", touched())
			}
		})
	}
}
