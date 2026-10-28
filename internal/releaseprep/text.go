// Package releaseprep turns raw file bytes into the text the checks read.
// input: a file body as stored (any encoding) and, for pages, its HTML
// output: decoded text (never skipped), the text a page shows (no comments, <template>, <script>, <style> or tag attributes such as <meta content>), one element attribute, and <script> bodies
// pos: shared by the stale scan (history.go) and the Surfaces manifest (manifest.go)
// note: if this file changes, update this header and README.md. Deliberate visual hiding (CSS, hidden attributes) is out of scope on purpose; see README.md.
package releaseprep

import (
	"encoding/binary"
	"html"
	"regexp"
	"strings"
	"unicode/utf16"
)

// decodeText returns the text of a file as stored. No file is skipped for
// its encoding: a UTF-16 file with a BOM is decoded, a UTF-8 BOM dropped,
// NUL bytes removed (so UTF-16 without a BOM and ASCII strings inside binary
// files still read as text), and invalid UTF-8 replaced with U+FFFD.
func decodeText(raw string) string {
	switch {
	case strings.HasPrefix(raw, "\xff\xfe"):
		return decodeUTF16(raw[2:], binary.LittleEndian)
	case strings.HasPrefix(raw, "\xfe\xff"):
		return decodeUTF16(raw[2:], binary.BigEndian)
	}
	raw = strings.TrimPrefix(raw, "\xef\xbb\xbf")
	raw = strings.ReplaceAll(raw, "\x00", "")
	return strings.ToValidUTF8(raw, "\uFFFD")
}

func decodeUTF16(b string, order binary.ByteOrder) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = order.Uint16([]byte(b[2*i : 2*i+2]))
	}
	return string(utf16.Decode(u))
}

// hasMarkup reports whether path renders HTML: pages and Markdown (whose
// inline HTML renders).
func hasMarkup(path string) bool {
	return strings.HasSuffix(path, ".html") || strings.HasSuffix(path, ".htm") || strings.HasSuffix(path, ".md")
}

var (
	// An unclosed element runs to the end of the file.
	templateRE = regexp.MustCompile(`(?is)<template\b.*?(?:</template\s*>|$)`)
	styleRE    = regexp.MustCompile(`(?is)<style\b.*?(?:</style\s*>|$)`)
	scriptRE   = regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)(?:</script\s*>|$)`)
	htmlTagRE  = regexp.MustCompile(`<[A-Za-z/!?][^<>]*>`)
	// A block element breaks text, so "v1</p><p>— latest" does not read as one run.
	blockTagRE = regexp.MustCompile(`(?i)^</?(?:p|div|li|ul|ol|br|hr|h[1-6]|section|article|header|footer|nav|table|tr|td|th|pre|body|html|head|title|details|summary|blockquote|main|aside)\b`)
	wsRE       = regexp.MustCompile(`[ \t\r\n\f]+`)
)

// shownText is the text a page shows a reader, as cheap normalization only:
// no HTML comments, no <template>, <script> or <style> content, no tags (so
// no attribute such as <meta content>), entities decoded, whitespace
// collapsed, and a line break at each block element.
func shownText(body string) string {
	s := stripHTMLComments(body)
	for _, re := range []*regexp.Regexp{templateRE, scriptRE, styleRE} {
		s = re.ReplaceAllString(s, "\n")
	}
	s = wsRE.ReplaceAllString(s, " ")
	s = htmlTagRE.ReplaceAllStringFunc(s, func(tag string) string {
		if blockTagRE.MatchString(tag) {
			return "\n"
		}
		return ""
	})
	return html.UnescapeString(s)
}

// attrOf returns attribute name of the first <elem> tag outside comments.
func attrOf(body, elem, name string) (string, bool) {
	tag := regexp.MustCompile(`(?is)<` + regexp.QuoteMeta(elem) + `\b[^>]*>`).FindString(stripHTMLComments(body))
	m := regexp.MustCompile(`(?is)\s` + regexp.QuoteMeta(name) + `\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`).FindStringSubmatch(tag)
	if m == nil {
		return "", false
	}
	return html.UnescapeString(m[1] + m[2] + m[3]), true
}

// scriptBodies returns the text inside every <script> element.
func scriptBodies(body string) []string {
	var out []string
	for _, m := range scriptRE.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}
