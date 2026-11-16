package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// Images and documents in a request, made into what every upstream reads (16 November 2026).
//
// An image is an image_url content part whose url is a base64 data: URL of a PNG, JPEG, WebP or GIF picture,
// checked down to its first bytes. An image given by address is fetched here, inside the attested machine, as
// Anthropic's and OpenAI's own APIs fetch one, and goes on inline: the upstream never fetches on a customer's
// behalf. Which routes read images, and what one costs them at most, is the route's (Route.ImageTokens, measured
// per model); a route that reads none refuses them with a reason instead of letting a model drop them unseen.
//
// A document (a PDF, or a text file) is the text a model reads of it: a PDF's text layer page by page, which is
// what OpenRouter's pdf-text engine gives a model without native PDF input, or a text file as it is. A scanned PDF
// has no text layer and is refused with that reason.

const (
	// mediaMaxBytes bounds one image or document, decoded or fetched (Anthropic takes images up to 5 MB and
	// PDFs up to 32 MB per request; a request here is at most MAX_REQUEST_BYTES, 32 MB, in all).
	mediaMaxBytes = 20 << 20
	// mediaFetchTimeout bounds fetching one file by address, headers and body.
	mediaFetchTimeout = 20 * time.Second
	// mediaFetchRedirects is how many redirects a fetch follows, each one checked again.
	mediaFetchRedirects = 3
	// documentMaxPages and documentMaxBytes bound what is read of one document.
	documentMaxPages = 1000
	documentMaxBytes = 4 << 20
	// documentPartType marks a document part between the translation of a request and the reading of its text.
	documentPartType = "_lebrel_document"
	// mediaUserAgent names the fetcher to the sites it fetches from.
	mediaUserAgent = "LebrelMediaFetch/1.0 (+https://lebrel.ai/docs)"
)

// sniffImage is an image's media type by its first bytes; "" when it is none of the four every upstream reads.
func sniffImage(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	}
	return ""
}

// decodeBase64 reads standard or URL-safe base64, padded or not, line breaks and spaces ignored.
func decodeBase64(value string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, value)
	if base64.StdEncoding.DecodedLen(len(clean)) > mediaMaxBytes+3 {
		return nil, formatErrorf("a file may be at most %d MB", mediaMaxBytes>>20)
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if data, err := encoding.DecodeString(clean); err == nil {
			return data, nil
		}
	}
	return nil, formatErrorf("a file's data must be base64")
}

// parseDataURL reads a base64 data: URL: its media type and its bytes.
func parseDataURL(value string) (string, []byte, error) {
	header, data, found := strings.Cut(strings.TrimPrefix(value, "data:"), ",")
	if !strings.HasPrefix(value, "data:") || !found {
		return "", nil, formatErrorf("a data: URL must be data:<media type>;base64,<data>")
	}
	parameters := strings.Split(header, ";")
	if len(parameters) < 2 || !strings.EqualFold(parameters[len(parameters)-1], "base64") {
		return "", nil, formatErrorf("a data: URL must be base64 (data:<media type>;base64,<data>)")
	}
	decoded, err := decodeBase64(data)
	if err != nil {
		return "", nil, err
	}
	return strings.ToLower(strings.TrimSpace(parameters[0])), decoded, nil
}

// imageDataURL is an image's bytes as the data: URL every upstream reads, its media type the one its bytes show.
func imageDataURL(data []byte) (string, error) {
	kind := sniffImage(data)
	if kind == "" {
		return "", formatErrorf("images must be PNG, JPEG, WebP or GIF")
	}
	if len(data) > mediaMaxBytes {
		return "", formatErrorf("an image may be at most %d MB", mediaMaxBytes>>20)
	}
	return "data:" + kind + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// imagePart is the chat content part of an image whose url is a data: URL, or an address still to fetch.
func imagePart(url string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

// imagePartFromBase64 is the part of an image given as base64 (the media type the client named is checked
// against the bytes, which decide).
func imagePartFromBase64(data string) (map[string]any, error) {
	decoded, err := decodeBase64(data)
	if err != nil {
		return nil, err
	}
	inline, err := imageDataURL(decoded)
	if err != nil {
		return nil, err
	}
	return imagePart(inline), nil
}

// imagePartFromURL is the part of an image given by URL: a data: URL is checked now, an http(s) address is
// fetched when the request's media are resolved.
func imagePartFromURL(value string) (map[string]any, error) {
	value = strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(value, "data:"):
		_, decoded, err := parseDataURL(value)
		if err != nil {
			return nil, err
		}
		inline, err := imageDataURL(decoded)
		if err != nil {
			return nil, err
		}
		return imagePart(inline), nil
	case strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://"):
		return imagePart(value), nil
	}
	return nil, formatErrorf("an image must be base64 data or an http(s) address")
}

// documentPart is a document waiting to be read: its bytes (base64), its text, or its address, and its title.
func documentPart(title, mediaType, data, text, address string) map[string]any {
	part := map[string]any{"type": documentPartType}
	if title != "" {
		part["title"] = title
	}
	if mediaType != "" {
		part["media_type"] = mediaType
	}
	switch {
	case data != "":
		part["data"] = data
	case address != "":
		part["url"] = address
	default:
		part["text"] = text
	}
	return part
}

// documentText is what a model reads of a document: a PDF's text layer, or a text file as it is.
func documentText(mediaType string, data []byte) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))
	switch {
	case kind == "application/pdf" || bytes.HasPrefix(data, []byte("%PDF-")):
		return pdfText(data)
	case strings.HasPrefix(kind, "text/") || kind == "application/json" || kind == "application/xml" || kind == "application/yaml" ||
		kind == "application/x-yaml" || kind == "application/javascript" || kind == "application/x-ndjson" || kind == "" || kind == "application/octet-stream":
		if !utf8.Valid(data) {
			return "", formatErrorf("a text document must be UTF-8 (PDF and text documents are read)")
		}
		if len(data) > documentMaxBytes {
			return "", formatErrorf("a text document may be at most %d MB", documentMaxBytes>>20)
		}
		return string(data), nil
	}
	return "", formatErrorf("documents of type %s are not read: send a PDF or a text file", kind)
}

// pdfText is a PDF's text layer, page by page. A damaged PDF is refused (a parser panic included), and so is one
// with no text at all (a scan), with the way round it.
func pdfText(data []byte) (text string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			text, err = "", formatErrorf("the PDF could not be read (it may be damaged)")
		}
	}()
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", formatErrorf("the PDF could not be read (it may be encrypted or damaged)")
	}
	pages := reader.NumPage()
	if pages > documentMaxPages {
		return "", formatErrorf("a PDF may have at most %d pages", documentMaxPages)
	}
	var out strings.Builder
	for number := 1; number <= pages; number++ {
		page := reader.Page(number)
		if page.V.IsNull() {
			continue
		}
		content, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		fmt.Fprintf(&out, "[Page %d]\n%s\n\n", number, content)
		if out.Len() > documentMaxBytes {
			return "", formatErrorf("the PDF's text is longer than %d MB", documentMaxBytes>>20)
		}
	}
	result := strings.TrimSpace(out.String())
	if result == "" {
		return "", formatErrorf("the PDF has no text layer (it is a scan): send its pages as images")
	}
	return result, nil
}

// documentBlock is a document's text as the model reads it, set apart with its title.
func documentBlock(title, text string) string {
	if title == "" {
		title = "document"
	}
	return fmt.Sprintf("<document title=%q>\n%s\n</document>", title, text)
}

// errNotPublic refuses a connection to an address that is not on the public internet.
var errNotPublic = errors.New("the address is not on the public internet")

// nonPublicPrefixes are ranges netip's own predicates leave out: shared, documentation, benchmark and reserved space.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

// publicAddress is whether an address is one a fetch may reach: global unicast, never loopback, private, link-local
// (the cloud's metadata service), shared or reserved.
func publicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// publicOnly checks the address a connection is about to be made to, after the name was resolved: a name that
// resolves to a private address, or one that changes its answer between two lookups, reaches nothing private.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	parsed, err := netip.ParseAddr(host)
	if err != nil || !publicAddress(parsed) {
		return errNotPublic
	}
	return nil
}

// newMediaClient fetches files by address: no proxy from the environment, public addresses only, bounded times
// and redirects, each redirect checked again.
func newMediaClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}
	return &http.Client{
		Timeout: mediaFetchTimeout,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			TLSHandshakeTimeout:    10 * time.Second,
			ResponseHeaderTimeout:  15 * time.Second,
			MaxResponseHeaderBytes: 64 << 10,
			ForceAttemptHTTP2:      true,
			MaxIdleConns:           16,
			IdleConnTimeout:        30 * time.Second,
		},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= mediaFetchRedirects {
				return errors.New("too many redirects")
			}
			if request.URL.Scheme != "https" && request.URL.Scheme != "http" {
				return errors.New("a redirect left http(s)")
			}
			return nil
		},
	}
}

// fetchMedia fetches a file by its http(s) address: its bytes and the content type it was served with.
func (s *Server) fetchMedia(ctx context.Context, address string) ([]byte, string, error) {
	if s.mediaFetch != nil {
		return s.mediaFetch(ctx, address)
	}
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" || parsed.User != nil {
		return nil, "", formatErrorf("a file's address must be an http(s) URL")
	}
	ctx, cancel := context.WithTimeout(ctx, mediaFetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", formatErrorf("a file's address must be an http(s) URL")
	}
	request.Header.Set("User-Agent", mediaUserAgent)
	request.Header.Set("Accept", "image/*, application/pdf, text/*;q=0.9, */*;q=0.5")
	response, err := s.mediaClient.Do(request)
	if err != nil {
		if errors.Is(err, errNotPublic) {
			return nil, "", formatErrorf("the file at %s is not on the public internet", parsed.Hostname())
		}
		return nil, "", formatErrorf("the file at %s could not be fetched", parsed.Hostname())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", formatErrorf("the file at %s answered HTTP %d", parsed.Hostname(), response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, mediaMaxBytes+1))
	if err != nil {
		return nil, "", formatErrorf("the file at %s could not be fetched", parsed.Hostname())
	}
	if len(data) > mediaMaxBytes {
		return nil, "", formatErrorf("the file at %s is larger than %d MB", parsed.Hostname(), mediaMaxBytes>>20)
	}
	return data, response.Header.Get("Content-Type"), nil
}

// resolveMedia makes a translated request's images and documents what the route's upstream reads: images inline
// (fetched by address here), counted and refused where the route reads none or more than it takes; documents
// their text. A message whose parts are all text again is its text alone, as a request without media always was.
func (s *Server) resolveMedia(ctx context.Context, chat map[string]any, route *Route) error {
	messages, _ := chat["messages"].([]map[string]any)
	images := 0
	for _, message := range messages {
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		resolved := make([]any, 0, len(parts))
		for _, part := range parts {
			object, _ := part.(map[string]any)
			switch object["type"] {
			case "image_url":
				images++
				if route.ImageTokens <= 0 {
					return formatErrorf("%s does not read images; send text, or use a model that reads them", route.ModelID)
				}
				if images > route.MaxImages {
					return formatErrorf("%s reads at most %d images in one request", route.ModelID, route.MaxImages)
				}
				inner, _ := object["image_url"].(map[string]any)
				address, _ := inner["url"].(string)
				if !strings.HasPrefix(address, "data:") {
					data, _, err := s.fetchMedia(ctx, address)
					if err != nil {
						return err
					}
					inline, err := imageDataURL(data)
					if err != nil {
						return err
					}
					object = imagePart(inline)
				}
				resolved = append(resolved, object)
			case documentPartType:
				text, err := s.documentPartText(ctx, object)
				if err != nil {
					return err
				}
				title, _ := object["title"].(string)
				resolved = append(resolved, map[string]any{"type": "text", "text": documentBlock(title, text)})
			default:
				resolved = append(resolved, object)
			}
		}
		message["content"] = joinedIfText(resolved)
	}
	return nil
}

// documentPartText reads a document part: its text, its base64 bytes, or the file at its address.
func (s *Server) documentPartText(ctx context.Context, part map[string]any) (string, error) {
	mediaType, _ := part["media_type"].(string)
	if text, ok := part["text"].(string); ok {
		return text, nil
	}
	if data, ok := part["data"].(string); ok {
		decoded, err := decodeBase64(data)
		if err != nil {
			return "", err
		}
		return documentText(mediaType, decoded)
	}
	address, _ := part["url"].(string)
	data, served, err := s.fetchMedia(ctx, address)
	if err != nil {
		return "", err
	}
	if mediaType == "" {
		mediaType = served
	}
	return documentText(mediaType, data)
}

// joinedIfText is a message's content: its text alone when every part is text, its parts otherwise.
func joinedIfText(parts []any) any {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		object, _ := part.(map[string]any)
		text, isText := object["text"].(string)
		if object["type"] != "text" || !isText {
			return parts
		}
		texts = append(texts, text)
	}
	return strings.Join(texts, "\n")
}
