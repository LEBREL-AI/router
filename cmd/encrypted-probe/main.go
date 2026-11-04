// encrypted-probe sends one end-to-end encrypted request to a Lebrel router and prints the answer:
// what the SDK does, as a command anyone can read.
//
//	encrypted-probe -router https://router-tee.lebrel.ai -model deepseek/deepseek-v4.1-flash \
//	    -key $LEBREL_API_KEY [-edge-token <hex>] [-prompt "..."]
//
// Steps: fetch /.well-known/attestation (the receipt key and the encryption key the quote binds;
// verify the quote with scripts/verify-attestation.py), fetch the route's signed key document and
// check its signature with the attested receipt key and its key id against the attested
// encryption key, encrypt the request to that key, decrypt the answer with the per-request keys.
// The API key travels in clear (it is not content); the prompt and the answer never do.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
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
	fmt.Printf("attested receipt key %s…, encryption key id %s…, instance %s (verify the quote with scripts/verify-attestation.py)\n", str(attested["signing_key_id"])[:16], encryptionKeyID[:16], str(attested["instance_id"]))

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

	body, _ := json.Marshal(map[string]any{"model": *model, "messages": []map[string]string{{"role": "user", "content": *prompt}}, "max_tokens": 16})
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
	if json.Unmarshal(answer, &parsed) == nil {
		if choices, ok := parsed["choices"].([]any); ok && len(choices) > 0 {
			if message, ok := choices[0].(map[string]any)["message"].(map[string]any); ok {
				fmt.Printf("answer (decrypted): %v\n", message["content"])
				return
			}
		}
	}
	fmt.Printf("answer (decrypted): %s\n", answer)
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
