package main

import "testing"

func TestStripInlineComment(t *testing.T) {
	cases := map[string]string{
		`""                  # default: <db dir>/cloud-buffer`: `""`,
		`"https://app.phpray.dev" # console`:                 `"https://app.phpray.dev"`,
		`32               # disk buffer limit`:                `32`,
		`"a # not a comment"`:                                 `"a # not a comment"`,
		`true`:                                                `true`,
	}
	for in, want := range cases {
		if got := stripInlineComment(in); got != want {
			t.Errorf("stripInlineComment(%q) = %q, want %q", in, got, want)
		}
	}
}
