package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/tinfoilsh/encrypted-http-body-protocol/protocol"

	"lebrel-router/internal/auth"
)

// An image by the API, attested like an answer (27 November 2026). Lebrel's own image model draws on the
// edition's GPUs; until now an image was asked for at the platform's own address, which read the request.
// Here the request arrives in plaintext over this router's own TLS, inside the attested machine, is
// encrypted to the runtime's published key exactly as an edition's completion is, and goes to Lebrel's
// edge, which admits and bills it without reading it. The runtime opens it, draws, and signs a receipt
// for the image; the answer comes back encrypted to this process and is handed to the client as OpenAI's
// image endpoint answers, with the receipt beside it. The plaintext of the request and of the image
// exists only here and on the machine that draws.
const (
	imageGenerationPath = "/v1/images/generations"
	// imageRequestMax is what the image endpoint takes as a request: 32 KiB, the platform's own bound.
	imageRequestMax = 32 << 10
	// imageAnswerMax bounds a decrypted answer: one image in base64 with its fields.
	imageAnswerMax = 24 << 20
	// imageAskedHeader tells the edge, which cannot read the request, a digest of what is asked: the
	// request's name (its Idempotency-Key) is bound to it, so the same name with another request is another image.
	imageAskedHeader = "X-Lebrel-Image-Asked"
	// imageManifestPath is where the image model's signed manifest is published: what an image's receipt names.
	imageManifestPath = "/.well-known/proof-of-edition/image"
)

// handleImageManifest relays the image model's manifest, signed by the runtime that draws, from the edge:
// a public document, as the edition's serving manifest is.
func (s *Server) handleImageManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	route := s.editionRoute()
	if route == nil {
		s.writeError(w, http.StatusNotFound, "manifest_not_found", "no such document")
		return
	}
	s.editionDocument(w, r, route, imageManifestPath, "manifest_not_found")
}

// imageResponseHeaders are the answer's headers handed to the client as they came.
var imageResponseHeaders = []string{receiptHeader, "Retry-After"}

func (s *Server) handleEditionImage(w http.ResponseWriter, r *http.Request) {
	s.active.Add(1)
	defer s.active.Add(-1)
	requestID := newRequestID()
	s.privacyHeaders(w, requestID)
	if !s.admit(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	if r.URL.RawQuery != "" {
		s.writeError(w, http.StatusBadRequest, "invalid_request_error", "query strings are not accepted")
		return
	}
	route := s.editionRoute()
	if route == nil {
		// Images are drawn on the edition's machine: a router that serves no edition serves none.
		s.writeError(w, http.StatusNotFound, "not_found", "unknown path")
		return
	}
	ctx, stop := context.WithCancelCause(r.Context())
	defer stop(nil)
	defer context.AfterFunc(s.cutCtx, func() { stop(errShutdownCut) })()
	// The key is looked up here as for every request (a wrong one is counted and refused before anything
	// is encrypted or sent); the edge looks it up again when it takes the hold.
	principal, err := s.settings.Auth.Authenticate(s.authContext(ctx, r), r.Header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			s.writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
		} else {
			s.writeError(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
		}
		return
	}
	releaseKey, err := s.settings.Limiter.Acquire(ctx, "key:"+principal.APIKeyID, principal.Concurrency)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		s.writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many concurrent requests for this key")
		return
	}
	defer releaseKey()
	original, err := io.ReadAll(http.MaxBytesReader(w, r.Body, imageRequestMax))
	if err != nil {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	// The runtime opens the client's own bytes: the receipt's request digest is theirs to check.
	body := bytes.TrimSpace(original)
	if len(body) == 0 || body[0] != '{' {
		s.writeError(w, http.StatusBadRequest, "invalid_request_error", "the request body must be a JSON object")
		return
	}
	asked := sha256.Sum256(body)
	named := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	// One retry when the edge says the key the router encrypted to is no longer the runtime's.
	for attempt := 0; attempt < 2; attempt++ {
		key, err := s.editionKey(ctx, route, attempt > 0)
		if err != nil {
			s.settings.Logger.Printf("request=%s image: edition key: %v", requestID, err)
			s.writeError(w, http.StatusServiceUnavailable, "encryption_unavailable", "the image model's encryption key is unavailable")
			return
		}
		upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, route.Edition.Endpoint()+imageGenerationPath, io.NopCloser(bytes.NewReader(body)))
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "internal_error", "the request could not be built")
			return
		}
		sealed, err := key.identity.EncryptRequestWithContext(upstream)
		if err != nil || sealed == nil {
			s.writeError(w, http.StatusBadGateway, "encryption_failed", "the request could not be encrypted")
			return
		}
		ciphertext, err := io.ReadAll(io.LimitReader(upstream.Body, 2*imageRequestMax+1))
		if err != nil || len(ciphertext) > 2*imageRequestMax {
			s.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
			return
		}
		upstream.Body = io.NopCloser(bytes.NewReader(ciphertext))
		upstream.ContentLength = int64(len(ciphertext))
		upstream.TransferEncoding = nil
		upstream.Header.Del("Transfer-Encoding")
		upstream.Header.Set("Authorization", r.Header.Get("Authorization"))
		upstream.Header.Set("Content-Type", "application/octet-stream")
		upstream.Header.Set("Accept", "application/json")
		upstream.Header.Set("Accept-Encoding", "identity")
		upstream.Header.Set("User-Agent", userAgent)
		upstream.Header.Set(editionKeyIDHeader, key.keyID)
		upstream.Header.Set(imageAskedHeader, hex.EncodeToString(asked[:]))
		if named != "" {
			upstream.Header.Set("Idempotency-Key", named)
		}
		response, err := s.settings.HTTPClient.Do(upstream)
		if err != nil {
			if ctx.Err() != nil {
				if errors.Is(context.Cause(ctx), errShutdownCut) && r.Context().Err() == nil {
					w.Header().Set("Retry-After", "1")
					s.writeError(w, http.StatusServiceUnavailable, "router_restarting", "the router is restarting; retry")
				}
				return
			}
			s.settings.Logger.Printf("request=%s image: edition transport error", requestID)
			s.writeError(w, http.StatusBadGateway, "upstream_unavailable", "the image model could not be reached")
			return
		}
		if id := response.Header.Get("X-Request-ID"); id != "" {
			// The edge's own id for the request: the receipt is kept under it.
			w.Header().Set("X-Request-ID", id)
			w.Header().Set(editionRequestIDHeader, id)
		}
		if response.Header.Get(protocol.ResponseNonceHeader) == "" {
			// The edge's own refusals come in plaintext (a wrong key, no credit, images busy, an image it
			// charged and the runtime refused to draw): relayed by status and code.
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
			response.Body.Close()
			code, message := plaintextEdgeError(raw)
			if response.StatusCode >= 200 && response.StatusCode <= 299 {
				s.writeError(w, http.StatusBadGateway, "encryption_response_invalid", "the image model answered in plaintext")
				return
			}
			rotated := code == "encryption_key_rotated" || code == "encryption_required" ||
				response.StatusCode == http.StatusUnprocessableEntity || strings.HasPrefix(response.Header.Get("Content-Type"), "application/problem+json")
			if attempt == 0 && rotated {
				continue
			}
			if retry := response.Header.Get("Retry-After"); retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			s.writeError(w, response.StatusCode, code, message)
			return
		}
		if response.Header.Get(editionKeyIDHeader) != key.keyID {
			response.Body.Close()
			s.writeError(w, http.StatusBadGateway, "encryption_response_invalid", "the answer is not encrypted to this request")
			return
		}
		if err := sealed.DecryptResponse(response); err != nil {
			response.Body.Close()
			s.writeError(w, http.StatusBadGateway, "encryption_response_invalid", "the answer could not be decrypted")
			return
		}
		// The whole answer is read before any of it is handed on: an image cut midway is no image.
		answer, err := io.ReadAll(io.LimitReader(response.Body, imageAnswerMax+1))
		response.Body.Close()
		if err != nil || len(answer) > imageAnswerMax {
			s.settings.Logger.Printf("request=%s image: answer cut: decryption or transport failed", requestID)
			s.writeError(w, http.StatusBadGateway, "encryption_response_invalid", "the answer could not be decrypted")
			return
		}
		for _, name := range imageResponseHeaders {
			if value := response.Header.Get(name); value != "" {
				w.Header().Set(name, value)
			}
		}
		w.Header().Set("Content-Type", contentTypeOr(response, "application/json"))
		w.Header().Set("X-Lebrel-Encryption", "router-to-runtime")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(answer)
		return
	}
}
