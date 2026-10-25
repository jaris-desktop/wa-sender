package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"runtime/debug"
	"sync"
)

const protocolVersion = 1

// Set by the release build: -ldflags "-X main.version=1.4.0 -X main.commit=<sha>".
var (
	version = "dev"
	commit  = "unknown"
)

func versionInfo() map[string]any {
	wm := "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "go.mau.fi/whatsmeow" {
				wm = d.Version
			}
		}
	}
	return map[string]any{"protocol": protocolVersion, "version": version, "commit": commit, "whatsmeow": wm}
}

// Error codes (PROTOCOL.md).
const (
	codeBadRequest    = "bad_request"
	codeBadProtocol   = "bad_protocol"
	codeUnitNotOpen   = "unit_not_open"
	codeAlreadyPaired = "already_paired"
	codeNotPaired     = "not_paired"
	codeNotConnected  = "not_connected"
	codeNotOnWhatsApp = "not_on_whatsapp"
	codeStoreNewer    = "store_newer"
	codeStoreError    = "store_error"
	codePairFailed    = "pair_failed"
	codeSendFailed    = "send_failed"
	codeInternal      = "internal"
)

// codedError carries a protocol error code.
type codedError struct {
	code string
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

func coded(code string, err error) error { return &codedError{code: code, err: err} }

var (
	errBadRequest = coded(codeBadRequest, errors.New("invalid params"))
	errNotOpen    = coded(codeUnitNotOpen, errors.New("unit is not open"))
)

func codeOf(err error) (string, string) {
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code, ce.err.Error()
	}
	return codeInternal, err.Error()
}

// lineWriter writes one JSON object per line; stdout is shared by every goroutine.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("encode: %v", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(b, '\n'))
}

func (l *lineWriter) result(id uint64, v any) {
	l.write(map[string]any{"id": id, "result": v})
}

func (l *lineWriter) fail(id uint64, code, msg string) {
	l.write(map[string]any{"id": id, "error": map[string]string{"code": code, "message": msg}})
}

func (l *lineWriter) event(name, unit string, data any) {
	l.write(map[string]any{"event": name, "unit": unit, "data": data})
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
