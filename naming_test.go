package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Servers send names in forms the standard does not allow in a plain filename
// and browsers decode anyway: percent escapes, which are frequent on Chinese
// sites, and RFC 2047 encoded words. The name is decoded first and cleaned
// after, so what is decoded gets no way round the rules for a safe name.
func TestPlainDispositionNamesAreDecodedThenCleaned(t *testing.T) {
	cases := []struct {
		name string
		disp string
		want string
	}{
		{"percent, bare", `attachment; filename=%E4%B8%AD%E6%96%87.zip`, "中文.zip"},
		{"percent, quoted", `attachment; filename="%E4%B8%AD%E6%96%87.zip"`, "中文.zip"},
		{"percent in some of it", `attachment; filename="my%20file.zip"`, "my file.zip"},
		{"encoded word, base64", `attachment; filename="=?UTF-8?B?5Lit5paHLnppcA==?="`, "中文.zip"},
		{"encoded word, quoted-printable", `attachment; filename="=?UTF-8?Q?=E4=B8=AD=E6=96=87.zip?="`, "中文.zip"},
		{"encoded word, latin-1", `attachment; filename="=?ISO-8859-1?Q?caf=E9.txt?="`, "café.txt"},
		{"encoded word, unquoted", `attachment; filename==?UTF-8?B?5Lit5paHLnppcA==?=`, "中文.zip"},

		// A literal percent is not an escape, and what filename* decoded is
		// not decoded again.
		{"lone percent", `attachment; filename="100%.zip"`, "100%.zip"},
		{"percent and a space", `attachment; filename="50% off.zip"`, "50% off.zip"},
		{"filename* is decoded once", `attachment; filename*=UTF-8''100%2525.zip`, "100%25.zip"},
		{"filename* beside a plain one", `attachment; filename="a%20b.zip"; filename*=UTF-8''c%2520d.zip`, "c%20d.zip"},
		{"bytes that are not utf-8 stay as sent", `attachment; filename=%D6%D0%CE%C4.zip`, "%D6%D0%CE%C4.zip"},
		{"unknown charset stays as sent", `attachment; filename="=?x-unknown?B?5Lit?="`, "=_x-unknown_B_5Lit_="},

		// What decoding reveals is cleaned like any other name.
		{"encoded slash", `attachment; filename=..%2F..%2Fevil.exe`, "evil.exe"},
		{"encoded backslash", `attachment; filename=..%5C..%5Cevil.exe`, "evil.exe"},
		{"encoded colon", `attachment; filename=a%3Ab.txt`, "a_b.txt"},
		{"encoded nul", `attachment; filename=%00a.txt`, "_a.txt"},
		{"encoded reserved name", `attachment; filename=%43%4F%4E.txt`, "_CON.txt"},
		{"encoded path in a word", `attachment; filename="=?UTF-8?B?Li4vZXZpbC5leGU=?="`, "evil.exe"},
		{"encoded sidecar name", `attachment; filename=x%2Egodm`, "x.godm_"},
		{"encoded sidecar name, capitals", `attachment; filename=x%2EGODM%20`, "x.GODM_"},
		{"sidecar name in a word", `attachment; filename="=?UTF-8?B?YS5nb2Rt?="`, "a.godm_"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://x.test/dl/get.php", nil)
			resp := &http.Response{Header: http.Header{"Content-Disposition": {c.disp}}, Request: req}
			got := ResolveFilename("", resp)
			if got != c.want {
				t.Errorf("%s: got %q, want %q", c.disp, got, c.want)
			}
			if isStateName(got) {
				t.Errorf("%s: %q would be read as a sidecar", c.disp, got)
			}
		})
	}
}
