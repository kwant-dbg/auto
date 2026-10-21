package evidence

import "testing"

func TestLocate(t *testing.T) {
	text := []byte("Line one.\nWe re-key   payouts into\nQuickBooks every month.\n")
	cases := []struct {
		quote string
		want  string
		ok    bool
	}{
		{"re-key   payouts", "re-key   payouts", true},                                                                // exact
		{"We re-key payouts into QuickBooks every month.", "We re-key   payouts into\nQuickBooks every month.", true}, // whitespace-insensitive
		{"  Line one.  ", "Line one.", true},                                                                          // trimmed
		{"We rekey payouts", "", false},                                                                               // paraphrase
		{"QuickBooks every year", "", false},
	}
	for _, c := range cases {
		s, e, ok := Locate(text, c.quote)
		if ok != c.ok || (ok && string(text[s:e]) != c.want) {
			t.Errorf("Locate(%q) = %q, %v; want %q, %v", c.quote, text[s:e], ok, c.want, c.ok)
		}
	}

	// JSON documents store quotes escaped; the span points at the stored bytes.
	js := []byte(`{"review": "It said \"done\" but the export\nwas empty"}`)
	s, e, ok := Locate(js, `It said "done" but the export was empty`)
	if !ok || string(js[s:e]) != `It said \"done\" but the export\nwas empty` {
		t.Fatalf("JSON locate = %q, %v", js[s:e], ok)
	}
	if _, _, ok := Locate(js, ""); ok {
		t.Fatal("empty quote must not match")
	}
}

func TestTier(t *testing.T) {
	cases := []struct {
		kind, cat string
		want      int
	}{
		{KindProcurement, "spend", T1ObservedSpend},
		{KindListing, "spend", T1ObservedSpend},
		{KindListing, "offer", T3PublishedOffer},
		{KindReview, "complaint", T4SpecificComplaint},
		{KindReview, "spend", T4SpecificComplaint},
		{KindWeb, "offer", T3PublishedOffer},
		{KindWeb, "change", T6Commentary},
		{KindFounder, "complaint", T2DirectToUs},
	}
	for _, c := range cases {
		if got := Tier(c.kind, c.cat); got != c.want {
			t.Errorf("Tier(%s, %s) = %d, want %d", c.kind, c.cat, got, c.want)
		}
	}
}

func TestDocKindAndOrigin(t *testing.T) {
	if DocKind("app_marketplace", []byte(`{"kind":"review"}`)) != KindReview || DocKind("app_marketplace", []byte(`{"kind":"listing"}`)) != KindListing {
		t.Error("marketplace kinds")
	}
	if DocKind("usaspending_awards", nil) != KindProcurement || DocKind("web_fetch", nil) != KindWeb {
		t.Error("other kinds")
	}
	if OriginKey("app_marketplace", "abc", "Vendor", "https://x") != "app_marketplace:author:abc" {
		t.Error("author wins")
	}
	if OriginKey("usaspending_awards", "", "Department of  Energy", "") != "usaspending_awards:org:department of energy" {
		t.Error("org normalised")
	}
	if OriginKey("web_fetch", "", "", "https://www.Example.com/a") != "host:example.com" {
		t.Error("host fallback")
	}
}
