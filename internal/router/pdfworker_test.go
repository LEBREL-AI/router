package router

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A PDF written to hurt its reader ends the reader's process, never the router's (27 November 2026). Each of
// these files ended or pinned the router while it read PDFs itself.

// hostilePDF writes objects (numbered from 1) with a classic cross-reference table.
func hostilePDF(objects ...string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, object := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func xrefStreamPDF(dictionary, stream string) []byte {
	return []byte("%PDF-1.5\n1 0 obj\n<< /Type /XRef " + dictionary + " >>\nstream\n" + stream + "\nendstream\nendobj\nstartxref\n9\n%%EOF\n")
}

func TestAHostilePDFEndsItsReaderAndNotTheRouter(t *testing.T) {
	old := pdfWorkerTimeout
	pdfWorkerTimeout = 4 * time.Second
	t.Cleanup(func() { pdfWorkerTimeout = old })
	// operands and no operator, deflated: a few kilobytes that become hundreds of megabytes on the reader's stack
	var packed bytes.Buffer
	writer, _ := zlib.NewWriterLevel(&packed, zlib.BestCompression)
	_, _ = writer.Write(bytes.Repeat([]byte("1 "), 6_000_000))
	writer.Close()
	flood := fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", packed.Len(), packed.String())
	files := map[string][]byte{
		"an object that holds itself (the stack is spent)":    xrefStreamPDF("/Size 2 /W [1 1 1] /Root 1 0 R /Length 6", "\x00\x00\x00\x02\x01\x00"),
		"a table of six trillion entries (allocated whole)":   xrefStreamPDF("/Size 6000000000000 /W [1 1 1] /Index [0 1] /Length 3", "\x00\x00\x00"),
		"a page that is its own parent (never returns)":       hostilePDF("<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 3 0 R /Contents 4 0 R >>", "<< /Length 13 >>\nstream\nBT (hi) Tj ET\nendstream"),
		"a page tree that lists itself (never returns)":       hostilePDF("<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [2 0 R] /Count 5 >>"),
		"a small stream that inflates to a flood of operands": hostilePDF("<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>", flood),
		"no PDF at all": []byte("%PDF-1.4\nnothing that follows is a PDF"),
	}
	for name, data := range files {
		started := time.Now()
		text, err := isolatedPDFText(context.Background(), data)
		var refused *formatError
		if err == nil || text != "" || !asFormatError(err, &refused) {
			t.Errorf("%s (%d bytes): read as %d characters, err %v", name, len(data), len(text), err)
			continue
		}
		if elapsed := time.Since(started); elapsed > pdfWorkerTimeout+3*time.Second {
			t.Errorf("%s: refused only after %s", name, elapsed.Round(time.Millisecond))
		}
		t.Logf("%s (%d bytes): %q after %s", name, len(data), refused.message, time.Since(started).Round(time.Millisecond))
	}
	// and the router still reads a PDF
	text, err := isolatedPDFText(context.Background(), tinyPDF("The total is 4,250 euros."))
	if err != nil || !strings.Contains(text, "4,250 euros") {
		t.Fatalf("an ordinary PDF after the hostile ones: %q, %v", text, err)
	}
}

func asFormatError(err error, target **formatError) bool {
	refused, ok := err.(*formatError)
	if ok {
		*target = refused
	}
	return ok
}

func TestThePDFReaderSaysWhyAPDFHasNoText(t *testing.T) {
	// what the reader itself refuses reaches the customer in its own words
	scan := hostilePDF("<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R >>")
	_, err := isolatedPDFText(context.Background(), scan)
	var refused *formatError
	if !asFormatError(err, &refused) || !strings.Contains(refused.message, "no text layer") {
		t.Fatalf("a PDF with no text: %v", err)
	}
	// a customer who left is not waited for
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := isolatedPDFText(gone, tinyPDF("hello")); err == nil {
		t.Fatal("a PDF was read for a request that had ended")
	}
}

func TestOnlyAFewPDFsAreReadAtATime(t *testing.T) {
	oldWait := pdfWorkerWait
	pdfWorkerWait = 300 * time.Millisecond
	t.Cleanup(func() { pdfWorkerWait = oldWait })
	for i := 0; i < pdfWorkersAtOnce; i++ {
		pdfWorkerSlots <- struct{}{}
	}
	_, err := isolatedPDFText(context.Background(), tinyPDF("hello"))
	for i := 0; i < pdfWorkersAtOnce; i++ {
		<-pdfWorkerSlots
	}
	var refused *formatError
	if !asFormatError(err, &refused) || !strings.Contains(refused.message, "send it again") {
		t.Fatalf("with every reader busy: %v", err)
	}
	if text, err := isolatedPDFText(context.Background(), tinyPDF("hello again")); err != nil || !strings.Contains(text, "hello again") {
		t.Fatalf("once a reader is free: %q, %v", text, err)
	}
}
