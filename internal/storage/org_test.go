package storage

import "testing"

func TestSlugNorm(t *testing.T) {
	cases := map[string]string{
		"Grace Hopper":            "grace_hopper",
		"  ada lovelace ":         "ada_lovelace",
		"José García-López":       "jos_garc_a_l_pez", // accents fold to underscores, never dropped
		"42_widget":               "42_widget",
		"__padded__":              "padded",
		"Jean-Pierre O'Brien":     "jean_pierre_o_brien",
		"Already_Snake":           "already_snake",
		"trailing.dot@weird.com!": "trailing_dot_weird_com",
	}
	for in, want := range cases {
		if got := SlugNorm(in); got != want {
			t.Errorf("SlugNorm(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPgArray(t *testing.T) {
	cases := map[string]string{
		`quote"inside`: `{"quote\"inside"}`,
		`back\slash`:   `{"back\\slash"}`,
		"plain":        `{"plain"}`,
		`a"b\c`:        `{"a\"b\\c"}`,
	}
	// exercise through the two-arg form used by callers
	for in, want := range cases {
		got := pgArray([]string{in})
		if got != want {
			t.Errorf("pgArray([%q]) = %s, want %s", in, got, want)
		}
	}
	if got := pgArray(nil); got != "{}" {
		t.Errorf("pgArray(nil) = %s, want {}", got)
	}
}

func TestSQLAnyString(t *testing.T) {
	if v := sqlAnyString(nil); v != nil {
		t.Errorf("nil should map to NULL, got %v", v)
	}
	empty := ""
	if v := sqlAnyString(&empty); v != nil {
		t.Errorf("empty string should map to NULL (unique emails must not collide), got %v", v)
	}
	v := "a@b.c"
	if got := sqlAnyString(&v); got != "a@b.c" {
		t.Errorf("value passthrough failed: %v", got)
	}
}
