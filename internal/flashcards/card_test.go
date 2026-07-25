package flashcards

import "testing"

func TestValidType(t *testing.T) {
	if !ValidType(TypeCode) || ValidType(CardType("trace-scheduled")) {
		t.Fatal("ValidType wrong")
	}
}

func TestStemHashIgnoresWordingNoise(t *testing.T) {
	a := StemHash("What does Fuse() return when the pane is active?")
	b := StemHash("what does fuse  return when the pane is active")
	if a != b {
		t.Fatalf("stem hash should ignore case/punct/whitespace: %s vs %s", a, b)
	}
	if StemHash("A totally different question") == a {
		t.Fatal("distinct stems must differ")
	}
}

func TestAnchorAndHashStable(t *testing.T) {
	if Anchor("loom", TypeCode, "internal/status/status.go") != "loom|code|internal/status/status.go" {
		t.Fatal("anchor format")
	}
	if Hash("x") == "" || Hash("x") != Hash("x") || Hash("x") == Hash("y") {
		t.Fatal("Hash unstable/colliding")
	}
}
