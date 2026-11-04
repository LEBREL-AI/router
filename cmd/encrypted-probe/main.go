// encrypted-probe sends one end-to-end encrypted request to a Lebrel router and prints the answer:
// what the SDK does, as a command anyone can read.
//
//	encrypted-probe -router https://router-tee.lebrel.ai -model deepseek/deepseek-v4.1-flash \
//	    -key $LEBREL_API_KEY [-edge-token <hex>] [-prompt "..."] \
//	    [-public-key <hex from RELEASES.md>] [-compose-hash <hex from RELEASES.md>]
//
// Steps: fetch /.well-known/attestation (the receipt key and the encryption key the quote binds;
// verify the quote with scripts/verify-attestation.py, and pin here the receipt key and the compose
// hash that script or RELEASES.md gave you), fetch the route's signed key document and check its
// signature with the attested receipt key and its key id against the attested encryption key,
// encrypt the request to that key, decrypt the answer with the per-request keys. A salt of ours
// goes first inside the encrypted body, so the receipt's digests are checkable only by the two
// ends. The API key travels in clear (it is not content); the prompt and the answer never do.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tinfoilsh/encrypted-http-body-protocol/identity"
)

func main() {
	router := flag.String("router", "https://router-tee.lebrel.ai", "router origin")
	model := flag.String("model", "deepseek/deepseek-v4.1-flash", "routed model id")
	key := flag.String("key", os.Getenv("LEBREL_API_KEY"), "Lebrel API key")
	edge := flag.String("edge-token", os.Getenv("LEBREL_EDGE_TOKEN"), "edge authorization (lebrel.ai's edge sets it; direct calls need it)")
	prompt := flag.String("prompt", "Reply with the single word: encrypted", "prompt")
	pinnedKey := flag.String("public-key", "", "receipt key (hex) the attestation must name: RELEASES.md's row")
	pinnedCompose := flag.String("compose-hash", "", "compose hash (hex) the attestation must measure: RELEASES.md's row")
	flag.Parse()
	if *key == "" {
		fmt.Fprintln(os.Stderr, "an API key is required (-key or LEBREL_API_KEY)")
		os.Exit(2)
	}
	slug := strings.ReplaceAll(*model, "/", "--")
	client := &http.Client{Timeout: 120 * time.Second}

	attested, err := getJSON(client, *router+"/.well-known/attestation")
	must(err, "attestation")
	receiptKey, _ := hex.DecodeString(str(attested["public_key"]))
	encryptionKeyID := str(attested["encryption_key_id"])
	if len(receiptKey) != ed25519.PublicKeySize || encryptionKeyID == "" {
		fail("the attestation names no receipt key or no encryption key: this instance does not accept encrypted requests")
	}
	info, _ := attested["info"].(map[string]any)
	if *pinnedKey != "" && !strings.EqualFold(str(attested["public_key"]), *pinnedKey) {
		fail("the attestation names receipt key " + str(attested["public_key"]) + ", not the pinned one")
	}
	if *pinnedCompose != "" && !strings.EqualFold(str(info["compose_hash"]), *pinnedCompose) {
		fail("the attestation measures compose " + str(info["compose_hash"]) + ", not the pinned one")
	}
	fmt.Printf("attested receipt key %s…, encryption key id %s…, compose %s…, instance %s (verify the quote with scripts/verify-attestation.py)\n", str(attested["signing_key_id"])[:16], encryptionKeyID[:16], str(info["compose_hash"])[:16], str(attested["instance_id"]))

	envelope, err := getJSON(client, *router+"/m/"+slug+"/.well-known/lebrel-encryption")
	must(err, "key document")
	payload, _ := base64.StdEncoding.DecodeString(str(envelope["payload"]))
	signature, _ := base64.StdEncoding.DecodeString(str(envelope["signature"]))
	if !ed25519.Verify(receiptKey, payload, signature) {
		fail("the key document is not signed by the attested receipt key")
	}
	var metadata struct {
		HPKEConfig string `json:"hpkeConfig"`
		KeyID      string `json:"keyId"`
		ModelID    string `json:"modelId"`
		ExpiresAt  int64  `json:"expiresAt"`
	}
	must(json.Unmarshal(payload, &metadata), "key document payload")
	config, _ := base64.StdEncoding.DecodeString(metadata.HPKEConfig)
	digest := sha256.Sum256(config)
	if hex.EncodeToString(digest[:]) != metadata.KeyID || metadata.KeyID != encryptionKeyID || metadata.ModelID != *model || metadata.ExpiresAt < time.Now().Unix() {
		fail("the key document does not match the attested encryption key, the model or the time")
	}
	remote, err := identity.UnmarshalPublicConfig(config)
	must(err, "key configuration")
	fmt.Printf("key document signed by the attested key, encryption key %s… matches the quote\n", metadata.KeyID[:16])

	inner, _ := json.Marshal(map[string]any{"model": *model, "messages": []map[string]string{{"role": "user", "content": *prompt}}, "max_tokens": 16})
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	// The salt goes first: the router cuts it out and salts the receipt's digests with it.
	body := append([]byte(`{"lebrel_receipt_salt":"`+hex.EncodeToString(salt)+`",`), inner[1:]...)
	request, _ := http.NewRequest(http.MethodPost, *router+"/m/"+slug+"/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+*key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Lebrel-Encryption-Key-Id", metadata.KeyID)
	if *edge != "" {
		request.Header.Set("X-Lebrel-Edge-Authorization", "v1="+*edge)
	}
	context, err := remote.EncryptRequestWithContext(request)
	must(err, "encryption")
	response, err := client.Do(request)
	must(err, "request")
	defer response.Body.Close()
	if response.Header.Get("Ehbp-Response-Nonce") == "" {
		raw, _ := io.ReadAll(response.Body)
		fail(fmt.Sprintf("the answer came in clear (status %d): %s", response.StatusCode, raw))
	}
	must(context.DecryptResponse(response), "response decryption")
	answer, err := io.ReadAll(response.Body)
	must(err, "answer")
	fmt.Printf("status %d, upstream %s, verification %s, request %s\n", response.StatusCode, response.Header.Get("X-Lebrel-Upstream"), response.Header.Get("X-Lebrel-Verification"), response.Header.Get("X-Request-Id"))
	var parsed map[string]any
	text := string(answer)
	if json.Unmarshal(answer, &parsed) == nil {
		if choices, ok := parsed["choices"].([]any); ok && len(choices) > 0 {
			if message, ok := choices[0].(map[string]any)["message"].(map[string]any); ok {
				text = str(message["content"])
			}
		}
	}
	fmt.Printf("answer (decrypted): %s\n", text)
	checkReceipt(response.Header.Get("Proof-Of-Edition-Receipt"), receiptKey, salt, inner, text)
}

// checkReceipt verifies the route receipt the answer carries: signed by the attested receipt key,
// its digests equal to SHA-256(salt || what we sent) and SHA-256(salt || what we received).
func checkReceipt(encoded string, receiptKey ed25519.PublicKey, salt []byte, sent []byte, received string) {
	if encoded == "" {
		fail("the answer carries no receipt")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	must(err, "receipt")
	var document struct {
		Payload   map[string]any `json:"payload"`
		Signature string         `json:"signature"`
	}
	must(json.Unmarshal(raw, &document), "receipt")
	canonical, err := canonicalJSON(document.Payload)
	must(err, "receipt payload")
	signature, _ := base64.StdEncoding.DecodeString(document.Signature)
	if !ed25519.Verify(receiptKey, canonical, signature) {
		fail("the receipt is not signed by the attested receipt key")
	}
	salted := func(body []byte) string {
		h := sha256.New()
		h.Write(salt)
		h.Write(body)
		return hex.EncodeToString(h.Sum(nil))
	}
	if str(document.Payload["hash_salt_scheme"]) != "client-salt-v1" || str(document.Payload["prompt_sha256"]) != salted(sent) || str(document.Payload["response_sha256"]) != salted([]byte(received)) {
		fail("the receipt's digests are not SHA-256(salt || request) and SHA-256(salt || answer)")
	}
	fmt.Printf("receipt signed by the attested key; prompt and answer digests match under our salt (scheme %s)\n", str(document.Payload["hash_salt_scheme"]))
}

// canonicalJSON is the router's canonical form: keys sorted, no spaces, HTML unescaped, as
// encoding/json writes a map; whole numbers stay integers.
func canonicalJSON(value any) ([]byte, error) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buffer bytes.Buffer
		buffer.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buffer.WriteByte(',')
			}
			name, _ := canonicalJSON(k)
			buffer.Write(name)
			buffer.WriteByte(':')
			encoded, err := canonicalJSON(v[k])
			if err != nil {
				return nil, err
			}
			buffer.Write(encoded)
		}
		buffer.WriteByte('}')
		return buffer.Bytes(), nil
	case []any:
		var buffer bytes.Buffer
		buffer.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buffer.WriteByte(',')
			}
			encoded, err := canonicalJSON(item)
			if err != nil {
				return nil, err
			}
			buffer.Write(encoded)
		}
		buffer.WriteByte(']')
		return buffer.Bytes(), nil
	case float64:
		if v == float64(int64(v)) {
			return []byte(strconv.FormatInt(int64(v), 10)), nil
		}
		return json.Marshal(v)
	default:
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(v); err != nil {
			return nil, err
		}
		return bytes.TrimRight(buffer.Bytes(), "\n"), nil
	}
}

func getJSON(client *http.Client, url string) (map[string]any, error) {
	response, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 400))
		return nil, fmt.Errorf("%s: HTTP %d: %s", url, response.StatusCode, raw)
	}
	var out map[string]any
	return out, json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&out)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func must(err error, what string) {
	if err != nil {
		fail(what + ": " + err.Error())
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "FAIL:", message)
	os.Exit(1)
}
