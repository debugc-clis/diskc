package macosystem

import "testing"

func TestRecommendationProtectsSystemFiles(t *testing.T) {
	recommendation, risk := recommendation("/System/Library/CoreServices/example", 20<<30)
	if risk != "do not delete" || recommendation == "" {
		t.Fatalf("unexpected system recommendation: %q / %q", recommendation, risk)
	}
}

func TestRecommendationClassifiesCaches(t *testing.T) {
	recommendation, risk := recommendation("/Users/test/Library/Caches/example/cache.db", 12<<30)
	if risk != "review before cleaning" || recommendation == "" {
		t.Fatalf("unexpected cache recommendation: %q / %q", recommendation, risk)
	}
}

func TestRecommendationProtectsApplicationData(t *testing.T) {
	recommendation, risk := recommendation("/Users/test/Library/Application Support/App/database.sqlite", 12<<30)
	if risk != "do not delete manually" || recommendation == "" {
		t.Fatalf("unexpected application data recommendation: %q / %q", recommendation, risk)
	}
}
