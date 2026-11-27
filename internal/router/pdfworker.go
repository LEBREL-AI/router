package router

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"time"
)

// A PDF is read in a process of its own (27 November 2026).
//
// The PDF reader takes a file from anyone with a key, and a file can be written to hurt what reads it: an object
// that holds itself (the reader calls itself until the stack is spent, which no recover catches), a table that
// declares billions of entries (allocated as written), a page tree that is its own parent (never returns), a few
// kilobytes that inflate to hundreds of megabytes. Read inside the router, any of those ends the router and every
// answer in flight with it. So the router starts its own binary again as a reader (PDFWorkerArg), hands it the
// file on its input and takes the text from its output. The reader has nothing of the router's: no environment
// (no key, no token), no network to use, a ceiling on its memory, its stack and its processor time, and a deadline
// after which it is killed. Whatever a file does to it, the router answers "the PDF could not be read".

const (
	// PDFWorkerArg is the one argument that makes the router's binary read a PDF from its input and exit.
	PDFWorkerArg = "pdf-text"
	// pdfWorkerMemory bounds what the reader may allocate, and pdfWorkerStack how deep it may call itself.
	pdfWorkerMemory = 768 << 20
	pdfWorkerStack  = 64 << 20
	// pdfWorkerCPUSeconds bounds the reader's processor time (the kernel ends it past that).
	pdfWorkerCPUSeconds = 10
	// pdfWorkersAtOnce is how many readers run at a time on a machine; a document past that waits its turn.
	pdfWorkersAtOnce = 2
	pdfWorkerText    = 'T'
	pdfWorkerRefusal = 'E'
)

var (
	// pdfWorkerTimeout is how long one PDF has, and pdfWorkerWait how long a document waits for a reader.
	pdfWorkerTimeout = 15 * time.Second
	pdfWorkerWait    = 30 * time.Second
	pdfWorkerSlots   = make(chan struct{}, pdfWorkersAtOnce)
	// pdfWorkerBinary is the binary started as a reader: the router's own.
	pdfWorkerBinary = os.Executable
)

// RunPDFWorker is the reader: a PDF on in, on out one byte that says what follows (the text, or why there is
// none) and then it. It returns the process's exit code. Called by the router's binary and nothing else.
func RunPDFWorker(in io.Reader, out io.Writer) int {
	applyPDFWorkerLimits()
	debug.SetMaxStack(pdfWorkerStack)
	debug.SetMemoryLimit(pdfWorkerMemory * 3 / 4)
	data, err := io.ReadAll(io.LimitReader(in, mediaMaxBytes+1))
	if err != nil || len(data) > mediaMaxBytes {
		_, _ = out.Write(append([]byte{pdfWorkerRefusal}, "the PDF could not be read"...))
		return 0
	}
	text, err := pdfText(data)
	if err != nil {
		message := "the PDF could not be read (it may be damaged)"
		var invalid *formatError
		if errors.As(err, &invalid) {
			message = invalid.message
		}
		_, _ = out.Write(append([]byte{pdfWorkerRefusal}, message...))
		return 0
	}
	_, _ = out.Write(append([]byte{pdfWorkerText}, text...))
	return 0
}

// cappedOutput keeps what a reader writes up to a limit and refuses the rest, which ends the reader.
type cappedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	if c.buffer.Len()+len(p) > c.limit {
		return 0, errors.New("the reader wrote more than a document may hold")
	}
	return c.buffer.Write(p)
}

// isolatedPDFText is a PDF's text layer, read by a process of its own.
func isolatedPDFText(ctx context.Context, data []byte) (string, error) {
	wait, stopWaiting := context.WithTimeout(ctx, pdfWorkerWait)
	defer stopWaiting()
	select {
	case pdfWorkerSlots <- struct{}{}:
		defer func() { <-pdfWorkerSlots }()
	case <-wait.Done():
		return "", formatErrorf("the PDF could not be read now (too many documents are being read): send it again in a moment")
	}
	binary, err := pdfWorkerBinary()
	if err != nil {
		return "", errors.New("pdf reader: " + err.Error())
	}
	deadline, stop := context.WithTimeout(ctx, pdfWorkerTimeout)
	defer stop()
	command := exec.CommandContext(deadline, binary, PDFWorkerArg)
	command.Env = []string{} // nothing of the router's environment: no key and no token reaches the reader
	command.Stdin = bytes.NewReader(data)
	output := &cappedOutput{limit: documentMaxBytes + 4096}
	command.Stdout = output
	command.WaitDelay = time.Second
	err = command.Run()
	read := output.buffer.Bytes()
	switch {
	case deadline.Err() != nil:
		return "", formatErrorf("the PDF took too long to read")
	case err != nil || len(read) == 0:
		// the reader ended badly: the file spent its stack, its memory or its processor time, or the reader could not start
		return "", formatErrorf("the PDF could not be read (it may be damaged)")
	case read[0] == pdfWorkerText:
		return string(read[1:]), nil
	case read[0] == pdfWorkerRefusal:
		return "", formatErrorf("%s", read[1:])
	}
	return "", formatErrorf("the PDF could not be read (it may be damaged)")
}
