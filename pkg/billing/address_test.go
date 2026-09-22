// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import "testing"

func TestFormatAddress(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"json null", "null", ""},
		{"plain text", "11618 Cedar Chase Road\nHerndon, VA 20170", "11618 Cedar Chase Road\nHerndon, VA 20170"},
		{"windows newlines and blanks", "Line one\r\n\r\n  Line two  \r\n", "Line one\nLine two"},
		{"quoted JSON string", `"11618 Cedar Chase Road\nHerndon, Virginia 20170\nUS"`, "11618 Cedar Chase Road\nHerndon, Virginia 20170\nUS"},
		{"structured object",
			`{"line1":"11618 Cedar Chase Road","line2":"Suite 4","city":"Herndon","state":"Virginia","postal_code":"20170","country":"US"}`,
			"11618 Cedar Chase Road\nSuite 4\nHerndon, Virginia 20170\nUS"},
		{"alternate field names", `{"street":"12 MG Road","city":"Bengaluru","zip":"560001","country":"India"}`, "12 MG Road\nBengaluru, 560001\nIndia"},
		{"object with only a country", `{"country":"India"}`, "India"},
		{"unparseable is kept, not dropped", "{not json", "{not json"},
	}
	for _, c := range cases {
		if got := FormatAddress(c.in); got != c.want {
			t.Errorf("%s: FormatAddress(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
