package proxy

import "testing"

func TestCloudflareColoPlace(t *testing.T) {
	city, country, ok := cloudflareColoPlace("ICN")
	if !ok || city != "Seoul" || country != "KR" {
		t.Fatalf("ICN = %s %s %v", city, country, ok)
	}
	if _, _, ok := cloudflareColoPlace("NOPE"); ok {
		t.Fatal("unknown code should not match")
	}
}

func TestCloudflareRayColo(t *testing.T) {
	if got := cloudflareRayColo("a40a331fbf22e9f7-ICN"); got != "ICN" {
		t.Fatal(got)
	}
	if got := cloudflareRayColo("not-a-ray"); got != "" {
		t.Fatal(got)
	}
}
