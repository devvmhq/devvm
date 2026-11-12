package platform

import (
	"slices"
	"testing"
)

// A clipboard tool's failure is an empty clipboard only when it says so
// in its own words; anything else, such as a display or a compositor that
// can't be reached, is the tool's failure, for xclip as for wl-paste.
func TestSaysEmpty(t *testing.T) {
	for _, c := range []struct {
		words  []string
		stderr string
		want   bool
	}{
		{wlPasteEmpty(), "Nothing is copied\n", true},
		{wlPasteEmpty(), "No selection\n", true},
		{wlPasteEmpty(), "Failed to connect to a Wayland server\n", false},
		{xclipEmpty(), "xclip: Error: There is no owner for the CLIPBOARD selection\n", true},
		{xclipEmpty(), "Error: target TARGETS not available\n", true},
		{xclipEmpty(), "xclip: Error: Can't open display: :0\n", false},
		{xclipEmpty(), "Error: Can't open display: (null)\n", false},
		{xclipEmpty(), "", false},
	} {
		if got := saysEmpty(c.words, c.stderr); got != c.want {
			t.Errorf("%q: %v, want %v", c.stderr, got, c.want)
		}
	}
}

// chooseTypes picks a PDF before a copied file's reference, which a PDF
// viewer names beside the PDF it copied, a file's reference before any
// image, PNG before the other image types, and nothing from text alone.
func TestChooseTypes(t *testing.T) {
	for _, c := range []struct {
		listed string
		want   []string
	}{
		{"text/plain\nSTRING\nUTF8_STRING\n", nil},
		{"", nil},
		{"image/png\ntext/html\n", []string{"image/png"}},
		{"image/webp\nimage/jpeg\nimage/gif\nimage/png\n", []string{"image/png", "image/jpeg", "image/gif", "image/webp"}},
		{"image/png\ntext/uri-list\nx-special/gnome-copied-files\n", []string{"text/uri-list", "x-special/gnome-copied-files", "image/png"}},
		{"TARGETS\r\nimage/bmp\r\nimage/png\r\n", []string{"image/png"}},
		{"image/svg+xml\nimage/bmp\n", nil},
		{"image/png\napplication/pdf\n", []string{"application/pdf", "image/png"}},
		{"application/pdf\ntext/uri-list\n", []string{"application/pdf", "text/uri-list"}},
		{"image/png\ntext/uri-list\napplication/pdf\n", []string{"application/pdf", "text/uri-list", "image/png"}},
	} {
		if got := chooseTypes([]byte(c.listed)); !slices.Equal(got, c.want) {
			t.Errorf("%q: %q, want %q", c.listed, got, c.want)
		}
	}
}

// uriListPaths reads file: URIs, percent-decoded, past comments and
// blank lines, with or without a localhost, with or without CRLF, and
// GNOME's first line; a web address and another scheme are left out.
func TestURIListPaths(t *testing.T) {
	for _, c := range []struct {
		data  string
		gnome bool
		want  []string
	}{
		{"file:///home/me/shot.png\r\n", false, []string{"/home/me/shot.png"}},
		{"file:///home/me/shot.png\n", false, []string{"/home/me/shot.png"}},
		{"file://localhost/home/me/a%20b.png\n", false, []string{"/home/me/a b.png"}},
		{"# a comment\n\nfile:///a\nfile:///b\n", false, []string{"/a", "/b"}},
		{"https://example.com/x.png\n", false, nil},
		{"file://nas/share/x.png\n", false, nil},
		{"copy\nfile:///home/me/shot.png\n", true, []string{"/home/me/shot.png"}},
		{"cut\nfile:///a\nfile:///b\n", true, []string{"/a", "/b"}},
		{"file:///a\n", true, []string{"/a"}},
		{"copy\n", true, nil},
		{"file:///caf%C3%A9/%E2%9C%93.png\n", false, []string{"/café/✓.png"}},
	} {
		if got := uriListPaths([]byte(c.data), c.gnome); !slices.Equal(got, c.want) {
			t.Errorf("%q gnome=%v: %q, want %q", c.data, c.gnome, got, c.want)
		}
	}
}

// dataOf takes an image or a PDF whose bytes are the type claimed, and
// refuses bytes of another type or text.
func TestDataOf(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + "rest")
	if c, ok := dataOf(png, "image/png"); !ok || c.Type != "image/png" || string(c.Data) != string(png) {
		t.Errorf("a PNG: %v %+v", ok, c)
	}
	pdf := []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\nrest")
	if c, ok := dataOf(pdf, TypePDF); !ok || c.Type != TypePDF || string(c.Data) != string(pdf) {
		t.Errorf("a PDF: %v %+v", ok, c)
	}
	for _, c := range []struct {
		name string
		data []byte
		want string
	}{
		{"a PNG as image/jpeg", png, "image/jpeg"},
		{"a PNG as a PDF", png, TypePDF},
		{"a PDF as image/png", pdf, "image/png"},
		{"text as a PDF", []byte("PDF hello"), TypePDF},
		{"text as image/png", []byte("hello"), "image/png"},
		{"an SVG", []byte("<svg/>"), "image/svg+xml"},
	} {
		if _, ok := dataOf(c.data, c.want); ok {
			t.Errorf("%s: taken", c.name)
		}
	}
	if DataExt("image/jpeg") != "jpg" || DataExt("image/webp") != "webp" || DataExt(TypePDF) != "pdf" || DataExt("text/plain") != "" {
		t.Error("DataExt")
	}
}
