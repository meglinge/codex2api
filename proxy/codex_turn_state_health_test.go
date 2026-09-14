package proxy

import "testing"

func TestInspectCodexTurnStateHealth(t *testing.T) {
	if got := InspectCodexTurnStateHealth("  ", "plus"); got != nil {
		t.Fatalf("empty value must return nil, got %#v", got)
	}

	healthy := InspectCodexTurnStateHealth(fakeCodexTurnStateFernet(160), "plus")
	if healthy == nil || healthy.Degraded || healthy.Error != "" || healthy.CipherLen != 160 || healthy.ExpectedCipherLen != 160 {
		t.Fatalf("160-byte personal blob must be healthy: %#v", healthy)
	}

	degraded := InspectCodexTurnStateHealth(fakeCodexTurnStateFernet(176), "plus")
	if degraded == nil || !degraded.Degraded || degraded.CipherLen != 176 || degraded.ExpectedCipherLen != 160 {
		t.Fatalf("176-byte personal blob must be degraded: %#v", degraded)
	}

	team := InspectCodexTurnStateHealth(fakeCodexTurnStateFernet(192), "team")
	if team == nil || team.Degraded || team.ExpectedCipherLen != 192 {
		t.Fatalf("192-byte team blob must be healthy: %#v", team)
	}

	invalid := InspectCodexTurnStateHealth("not-a-fernet-token!!", "plus")
	if invalid == nil || invalid.Error == "" || invalid.Degraded || invalid.CipherLen != 0 || invalid.ExpectedCipherLen != 160 {
		t.Fatalf("unparseable blob must report an error without a degraded verdict: %#v", invalid)
	}
}
