package router

import (
	"os"
	"testing"
)

// The tests' binary is also the PDF reader, as the router's binary is (cmd/router): started with the reader's one
// argument it reads a PDF from its input and exits.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == PDFWorkerArg {
		os.Exit(RunPDFWorker(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}
