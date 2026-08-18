package toolvm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// binaryTool stands up a tool with net.http granted against a loopback server.
func binaryTool(t *testing.T, body string) (*Host, *httptest.Server, func(string) (string, error)) {
	t.Helper()
	allowTestLoopback(t)

	var srv *httptest.Server
	dir := t.TempDir()
	writeTool(t, dir, "b", `
name = "b"
kind = "js"
entrypoint = "./b.js"
description = "b"
[capabilities]
net = ["http"]
`, body)
	h := openHost(t, dir, map[string]Grant{
		"b": {HTTP: &HTTPGrant{AllowHosts: []string{"127.0.0.1"}, Methods: []string{"GET", "POST"}}},
	})
	call := func(url string) (string, error) {
		args, err := json.Marshal(map[string]string{"url": url})
		if err != nil {
			t.Fatal(err)
		}
		return h.Call(context.Background(), "b", args)
	}
	return h, srv, call
}

// The bug this milestone exists for: a PNG's bytes used to arrive with every
// invalid byte replaced by U+FFFD, res.ok true, and nothing reporting it.
func TestBinaryResponseSurvivesIntact(t *testing.T) {
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe, 0x00, 0x01}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	_, _, call := binaryTool(t, `export default async ({ url }) => {
  const res = await fetch(url);
  const b = await res.bytes();
  return Array.from(b).join(",");
};`)

	out, err := call(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := "137,80,78,71,13,10,26,10,255,254,0,1"
	if out != want {
		t.Errorf("bytes()  = %s\nwant      %s", out, want)
	}
}

// text() must refuse rather than hand back mojibake — the failure should surface
// where the mistake is, not three transformations later as a wrong answer.
func TestBinaryResponseTextThrows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte{0xff, 0xfe, 0x00}) //nolint:errcheck
	}))
	defer srv.Close()

	for _, method := range []string{"text", "json"} {
		t.Run(method, func(t *testing.T) {
			_, _, call := binaryTool(t, `export default async ({ url }) => {
  const res = await fetch(url);
  return await res.`+method+`();
};`)
			_, err := call(srv.URL)
			if err == nil {
				t.Fatalf("%s() on a binary body did not fail", method)
			}
			if !strings.Contains(err.Error(), "not valid UTF-8") {
				t.Errorf("error does not explain the problem: %v", err)
			}
		})
	}
}

// Text must keep behaving exactly as it did: this is the overwhelmingly common
// case and no tool should have to decode it.
func TestTextResponseUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"hi":"there","unicode":"héllo 中文 🎉"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	_, _, call := binaryTool(t, `export default async ({ url }) => {
  const res = await fetch(url);
  const j = await res.json();
  return j.hi + "|" + j.unicode + "|" + (await (await fetch(url)).text()).length;
};`)
	out, err := call(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "there|héllo 中文 🎉|") {
		t.Errorf("text/json path changed: %q", out)
	}
}

// bytes() on a *text* response must still work, and must be UTF-8 rather than
// whatever Latin-1 would make of it.
func TestBytesOnTextResponseIsUTF8(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("é中")) //nolint:errcheck
	}))
	defer srv.Close()

	_, _, call := binaryTool(t, `export default async ({ url }) => {
  const res = await fetch(url);
  return Array.from(await res.bytes()).join(",");
};`)
	out, err := call(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// é = C3 A9, 中 = E4 B8 AD
	const want = "195,169,228,184,173"
	if out != want {
		t.Errorf("bytes() = %s, want %s", out, want)
	}
}

// A Uint8Array body used to be String()-ed onto the wire as "1,2,3,255".
func TestBinaryRequestBodyIsSentAsBytes(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 32)
		n, _ := r.Body.Read(b)
		got <- b[:n]
		w.Write([]byte("ok")) //nolint:errcheck
	}))
	defer srv.Close()

	for name, body := range map[string]string{
		"Uint8Array":  `new Uint8Array([1,2,3,255])`,
		"ArrayBuffer": `new Uint8Array([1,2,3,255]).buffer`,
		"typed view":  `new Uint8Array(new Uint8Array([9,1,2,3,255]).buffer, 1)`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, call := binaryTool(t, `export default async ({ url }) => {
  const res = await fetch(url, { method: "POST", body: `+body+` });
  return await res.text();
};`)
			if _, err := call(srv.URL); err != nil {
				t.Fatal(err)
			}
			sent := <-got
			want := []byte{1, 2, 3, 255}
			if len(sent) != len(want) {
				t.Fatalf("server received %v (%d bytes), want %v", sent, len(sent), want)
			}
			for i := range want {
				if sent[i] != want[i] {
					t.Fatalf("server received %v, want %v", sent, want)
				}
			}
		})
	}
}

// A string body must still be sent as text, unchanged.
func TestStringRequestBodyUnchanged(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		got <- string(b[:n])
		w.Write([]byte("ok")) //nolint:errcheck
	}))
	defer srv.Close()

	_, _, call := binaryTool(t, `export default async ({ url }) => {
  const res = await fetch(url, { method: "POST", body: JSON.stringify({a:1}) });
  return await res.text();
};`)
	if _, err := call(srv.URL); err != nil {
		t.Fatal(err)
	}
	if sent := <-got; sent != `{"a":1}` {
		t.Errorf("server received %q, want %q", sent, `{"a":1}`)
	}
}

// Round-trip: fetch bytes, post them back, and confirm what arrives is identical.
// This is the "end to end" of the milestone's name — every byte value 0-255 in
// both directions.
func TestBinaryRoundTrip(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			b := make([]byte, 1024)
			n, _ := r.Body.Read(b)
			got <- b[:n]
			w.Write([]byte("ok")) //nolint:errcheck
			return
		}
		w.Write(all) //nolint:errcheck
	}))
	defer srv.Close()

	_, _, call := binaryTool(t, `export default async ({ url }) => {
  const down = await (await fetch(url)).bytes();
  const res = await fetch(url, { method: "POST", body: down });
  return down.length + ":" + (await res.text());
};`)
	out, err := call(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if out != "256:ok" {
		t.Errorf("got %q, want 256:ok", out)
	}
	sent := <-got
	if len(sent) != 256 {
		t.Fatalf("round-tripped %d bytes, want 256", len(sent))
	}
	for i := range all {
		if sent[i] != all[i] {
			t.Fatalf("byte %d round-tripped as %d", i, sent[i])
		}
	}
}

// A tool returning bytes must produce bytes, not the {"0":137,"1":80,…} that
// JSON.stringify makes of a Uint8Array — which is silently useless.
func TestJSToolCanReturnBytes(t *testing.T) {
	for name, tc := range map[string]struct {
		expr      string
		wantBytes []byte
		wantMedia string
	}{
		"Uint8Array":   {`new Uint8Array([137,80,78,71,0,255])`, []byte{137, 80, 78, 71, 0, 255}, ""},
		"ArrayBuffer":  {`new Uint8Array([1,2,3]).buffer`, []byte{1, 2, 3}, ""},
		"offset view":  {`new Uint8Array(new Uint8Array([9,9,1,2,3]).buffer, 2, 3)`, []byte{1, 2, 3}, ""},
		"with a label": {`({ bytes: new Uint8Array([1,2]), mediaType: "image/png" })`, []byte{1, 2}, "image/png"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTool(t, dir, "b", `
name = "b"
kind = "js"
entrypoint = "./b.js"
description = "b"
`, `export default () => `+tc.expr+`;`)
			h := openHost(t, dir, nil)

			out, err := h.CallOutput(context.Background(), "b", json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if out.Text != "" {
				t.Errorf("bytes arrived as text: %q", out.Text)
			}
			if len(out.Bytes) != len(tc.wantBytes) {
				t.Fatalf("got %v, want %v", out.Bytes, tc.wantBytes)
			}
			for i := range tc.wantBytes {
				if out.Bytes[i] != tc.wantBytes[i] {
					t.Fatalf("got %v, want %v", out.Bytes, tc.wantBytes)
				}
			}
			if out.MediaType != tc.wantMedia {
				t.Errorf("MediaType = %q, want %q", out.MediaType, tc.wantMedia)
			}
		})
	}
}

// Everything that is not bytes must keep going through the text path untouched.
func TestJSToolTextResultsAreUnchanged(t *testing.T) {
	for name, tc := range map[string]struct{ expr, want string }{
		"string": {`"plain"`, "plain"},
		"object": {`({a:1})`, `{"a":1}`},
		"array":  {`[1,2,3]`, `[1,2,3]`},
		"null":   {`null`, ``},
		// An object with a `bytes` key that is not bytes is an ordinary object.
		"bytes-ish object": {`({ bytes: "not really" })`, `{"bytes":"not really"}`},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTool(t, dir, "b", `
name = "b"
kind = "js"
entrypoint = "./b.js"
description = "b"
`, `export default () => `+tc.expr+`;`)
			h := openHost(t, dir, nil)
			out, err := h.CallOutput(context.Background(), "b", json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if out.Bytes != nil {
				t.Fatalf("a text result was treated as bytes: %v", out.Bytes)
			}
			if out.Text != tc.want {
				t.Errorf("Text = %q, want %q", out.Text, tc.want)
			}
		})
	}
}
