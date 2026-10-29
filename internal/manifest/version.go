package manifest

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

// Version is a release version of the form MAJOR.MINOR.PATCH. Pre-release and
// build suffixes are rejected so that ordering is never ambiguous.
type Version struct {
	Major, Minor, Patch int
}

// ParseVersion parses a strict MAJOR.MINOR.PATCH string.
func ParseVersion(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid version %q: want MAJOR.MINOR.PATCH", s)
	}
	var n [3]int
	for i, p := range parts {
		if !isNumber(p) {
			return Version{}, fmt.Errorf("invalid version %q: %q is not a number", s, p)
		}
		n[i], _ = strconv.Atoi(p)
	}
	return Version{Major: n[0], Minor: n[1], Patch: n[2]}, nil
}

// isNumber accepts decimal digits without leading zeros, short enough that
// Atoi cannot overflow.
func isNumber(s string) bool {
	if s == "" || len(s) > 9 || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare returns -1, 0, or +1 depending on whether v is older than, equal
// to, or newer than o.
func (v Version) Compare(o Version) int {
	if c := cmp.Compare(v.Major, o.Major); c != 0 {
		return c
	}
	if c := cmp.Compare(v.Minor, o.Minor); c != 0 {
		return c
	}
	return cmp.Compare(v.Patch, o.Patch)
}

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool { return v.Compare(o) < 0 }

// IsZero reports whether v is the unset version 0.0.0.
func (v Version) IsZero() bool { return v == Version{} }

func (v Version) MarshalText() ([]byte, error) { return []byte(v.String()), nil }

func (v *Version) UnmarshalText(b []byte) error {
	parsed, err := ParseVersion(string(b))
	if err != nil {
		return err
	}
	*v = parsed
	return nil
}
