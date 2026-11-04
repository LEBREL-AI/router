package router

import (
	"fmt"
	"os"
	"testing"
)

// TestPrintBillingHashes prints the billing contract hash of every route in ROUTES_FILE so the
// control plane's PUBLIC_MODEL_RELEASES_JSON can be checked against it.
func TestPrintBillingHashes(t *testing.T) {
	path := os.Getenv("ROUTES_FILE")
	if path == "" {
		t.Skip("ROUTES_FILE not set")
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range config.Routes {
		hash, err := route.BillingContractHash()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("HASH %s %s\n", route.ModelID, hash)
	}
}
