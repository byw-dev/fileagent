package trollsift

import "testing"

func TestNormalizeTemplate(t *testing.T) {
	cases := map[string]string{
		"/{agent_name}/{time:yyyy/MM/dd}/{filename}": "{agent_name}/{time:yyyy/MM/dd}/{filename}",
		"{agent_name}/{filename}":                    "{agent_name}/{filename}",
		"/{filename}":                                "{filename}",
		"out/{filename}":                             "out/{filename}",
		"":                                           "",
		"/":                                          "",
	}
	for in, want := range cases {
		if got := NormalizeTemplate(in); got != want {
			t.Errorf("NormalizeTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeObjectKey(t *testing.T) {
	if got := NormalizeObjectKey("/2026/x.csv"); got != "2026/x.csv" {
		t.Errorf("got %q", got)
	}
	if got := NormalizeObjectKey("2026/x.csv"); got != "2026/x.csv" {
		t.Errorf("got %q", got)
	}
}

// The whole point of normalising: a key composed from a template must parse
// back against that same template. Before normalisation a leading "/" made this
// round trip fail for every template the Web UI creates by default (IC-BUG-16).
func TestNormalizedTemplateRoundTrips(t *testing.T) {
	const template = "/{year}/{filename}"

	composer, err := New(NormalizeTemplate(template))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key, err := composer.Compose(map[string]Value{
		"year":     S("2026"),
		"filename": S("x.csv"),
	}, false)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if key != "2026/x.csv" {
		t.Fatalf("composed key = %q", key)
	}

	vals, err := composer.Parse(NormalizeObjectKey(key))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if vals["year"].Raw != "2026" || vals["filename"].Raw != "x.csv" {
		t.Fatalf("parsed back %+v", vals)
	}
}

// Guard against the regression directly: the raw template does not match the
// key the agent actually writes.
func TestRawTemplateDoesNotMatchStrippedKey(t *testing.T) {
	p, err := New("/{year}/{filename}")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Parse("2026/x.csv"); err == nil {
		t.Fatal("expected the un-normalised template to fail; if this now passes, " +
			"trollsift changed and NormalizeTemplate may no longer be needed")
	}
}
