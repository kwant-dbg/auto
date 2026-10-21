package textutil

import (
	"strings"
	"testing"
)

func TestHTMLToText(t *testing.T) {
	page := `<html><head><title>Pricing &amp; Plans</title><style>p{}</style><script>var x="hidden";</script></head>
<body><nav>Home</nav><h1>Plans</h1><p>Starter costs   $49/month.</p><div>Pro:<br>$199/month</div>
<noscript>enable js</noscript><ul><li>One</li><li>Two</li></ul></body></html>`
	title, text, err := HTMLToText(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	if title != "Pricing & Plans" {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{"Starter costs $49/month.", "Pro:\n$199/month", "One\n\nTwo"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"hidden", "enable js", "p{}"} {
		if strings.Contains(text, bad) {
			t.Errorf("text contains %q:\n%s", bad, text)
		}
	}
	// Deterministic: same input, same output.
	_, again, _ := HTMLToText(strings.NewReader(page))
	if again != text {
		t.Error("conversion is not deterministic")
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("  a \t b \n\n\n\n c  "); got != "a b\n\nc" {
		t.Errorf("Normalize = %q", got)
	}
}
