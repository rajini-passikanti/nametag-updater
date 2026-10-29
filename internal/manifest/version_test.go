package manifest

import "testing"

func TestParseVersion(t *testing.T) {
	valid := map[string]Version{
		"0.0.1":    {0, 0, 1},
		"1.2.3":    {1, 2, 3},
		"10.20.30": {10, 20, 30},
	}
	for s, want := range valid {
		got, err := ParseVersion(s)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", s, got, err, want)
		}
		if got.String() != s {
			t.Errorf("String() = %q, want %q", got.String(), s)
		}
	}

	for _, s := range []string{"", "1", "1.2", "1.2.3.4", "v1.2.3", "1.2.3-beta", "01.2.3", "1.-2.3", "1..3", "1.2.9999999999"} {
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) succeeded, want error", s)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.10.0", "1.9.0", 1},
		{"2.0.0", "1.99.99", 1},
	}
	for _, tt := range tests {
		a, _ := ParseVersion(tt.a)
		b, _ := ParseVersion(tt.b)
		if got := a.Compare(b); got != tt.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseKeyRing(t *testing.T) {
	ring, err := ParseKeyRing("")
	if err != nil || len(ring) != 0 {
		t.Fatalf("empty ring: %v, %v", ring, err)
	}
	_, entry, err := GenerateKey("k1")
	if err != nil {
		t.Fatal(err)
	}
	_, entry2, _ := GenerateKey("k2")
	ring, err = ParseKeyRing(entry + "," + entry2)
	if err != nil || len(ring) != 2 {
		t.Fatalf("two keys: %v, %v", ring, err)
	}
	for _, bad := range []string{"k1", "k1:notbase64!", "k1:AAAA", "bad id:" + entry[3:], entry + "," + entry} {
		if _, err := ParseKeyRing(bad); err == nil {
			t.Errorf("ParseKeyRing(%q) succeeded, want error", bad)
		}
	}
}
