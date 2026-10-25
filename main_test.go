package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// harness drives a server over in-memory pipes, like the host does.
type harness struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan map[string]any
	nextID uint64
	done   chan struct{}
}

func start(t *testing.T) *harness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, in: inW, lines: make(chan map[string]any, 64), done: make(chan struct{})}
	s := newServer(ctx, outW)
	go func() {
		s.run(inR)
		s.closeAll()
		_ = outW.Close()
		close(h.done)
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("non-JSON line on stdout: %q", sc.Text())
				continue
			}
			h.lines <- m
		}
		close(h.lines)
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		cancel()
		<-h.done
	})
	return h
}

// call sends a request and waits for its answer, skipping events.
func (h *harness) call(method string, params any) map[string]any {
	h.t.Helper()
	h.nextID++
	id := h.nextID
	b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if _, err := h.in.Write(append(b, '\n')); err != nil {
		h.t.Fatalf("write: %v", err)
	}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case m, ok := <-h.lines:
			if !ok {
				h.t.Fatalf("%s: stdout closed", method)
			}
			if got, _ := m["id"].(float64); uint64(got) == id {
				return m
			}
		case <-deadline:
			h.t.Fatalf("%s: no answer", method)
		}
	}
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func result(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result, got %v", m)
	}
	return r
}

const testKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestHelloIsRequiredAndVersioned(t *testing.T) {
	h := start(t)
	if c := errCode(h.call("status", map[string]any{"unit": "u"})); c != codeBadProtocol {
		t.Fatalf("before hello: %q", c)
	}
	if c := errCode(h.call("hello", map[string]any{"protocol": 2})); c != codeBadProtocol {
		t.Fatalf("protocol 2: %q", c)
	}
	r := result(t, h.call("hello", map[string]any{"protocol": 1}))
	if r["protocol"].(float64) != 1 || r["version"] == "" || r["whatsmeow"] == "" {
		t.Fatalf("hello: %v", r)
	}
}

// An unlinked unit opens without touching the network, refuses sends with
// not_paired, and logout deletes its encrypted store.
func TestOpenUnpairedUnit(t *testing.T) {
	h := start(t)
	result(t, h.call("hello", map[string]any{"protocol": 1}))
	path := filepath.Join(t.TempDir(), "wa", "unit-a.db")
	r := result(t, h.call("open_unit", map[string]any{"unit": "a", "storePath": path, "storeKeyHex": testKey}))
	if r["state"] != stateUnpaired {
		t.Fatalf("open: %v", r)
	}
	if r := result(t, h.call("status", map[string]any{"unit": "a"})); r["state"] != stateUnpaired {
		t.Fatalf("status: %v", r)
	}
	if c := errCode(h.call("send", map[string]any{"unit": "a", "to": "6281234567890", "text": "hi", "messageId": "m1"})); c != codeNotPaired {
		t.Fatalf("send unpaired: %q", c)
	}
	if c := errCode(h.call("check_number", map[string]any{"unit": "a", "phone": "6281234567890"})); c != codeNotPaired {
		t.Fatalf("check unpaired: %q", c)
	}
	if c := errCode(h.call("status", map[string]any{"unit": "b"})); c != codeUnitNotOpen {
		t.Fatalf("unknown unit: %q", c)
	}
	head, err := os.ReadFile(path)
	if err != nil || strings.HasPrefix(string(head), "SQLite format 3") {
		t.Fatalf("store must exist and be encrypted (err %v)", err)
	}
	result(t, h.call("logout", map[string]any{"unit": "a"}))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("logout must delete the store: %v", err)
	}
}

func TestBadParams(t *testing.T) {
	h := start(t)
	result(t, h.call("hello", map[string]any{"protocol": 1}))
	path := filepath.Join(t.TempDir(), "u.db")
	for _, c := range []struct {
		method string
		params map[string]any
	}{
		{"open_unit", map[string]any{"unit": "a"}},
		{"open_unit", map[string]any{"unit": "a", "storePath": path, "storeKeyHex": "abc"}},
		{"open_unit", map[string]any{"unit": "a", "storePath": path, "extra": 1}},
		{"send", map[string]any{"unit": "a", "to": "081234567890", "text": "x", "messageId": "m"}},
		{"send", map[string]any{"unit": "a", "to": "6281234567890", "text": "", "messageId": "m"}},
		{"send", map[string]any{"unit": "a", "to": "6281234567890", "text": "x", "messageId": strings.Repeat("m", 65)}},
		{"pair_code", map[string]any{"unit": "a", "phone": "+6281234567890"}},
		{"nope", map[string]any{}},
	} {
		if code := errCode(h.call(c.method, c.params)); code != codeBadRequest {
			t.Fatalf("%s %v: %q", c.method, c.params, code)
		}
	}
}

// A store with the wrong key, or one a newer wa-sender upgraded, is refused with
// a code the host can act on.
func TestStoreErrors(t *testing.T) {
	h := start(t)
	result(t, h.call("hello", map[string]any{"protocol": 1}))
	dir := t.TempDir()
	path := filepath.Join(dir, "u.db")
	result(t, h.call("open_unit", map[string]any{"unit": "a", "storePath": path, "storeKeyHex": testKey}))
	result(t, h.call("close_unit", map[string]any{"unit": "a"}))
	wrong := strings.Repeat("ff", 32)
	if c := errCode(h.call("open_unit", map[string]any{"unit": "a", "storePath": path, "storeKeyHex": wrong})); c != codeStoreError {
		t.Fatalf("wrong key: %q", c)
	}

	plain := filepath.Join(dir, "plain.db")
	result(t, h.call("open_unit", map[string]any{"unit": "p", "storePath": plain, "storeKeyHex": ""}))
	result(t, h.call("close_unit", map[string]any{"unit": "p"}))
	db, err := sql.Open("sqlite3", storeDSN(plain, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE whatsmeow_version SET version = version + 100, compat = version + 100`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if c := errCode(h.call("open_unit", map[string]any{"unit": "p", "storePath": plain, "storeKeyHex": ""})); c != codeStoreNewer {
		t.Fatalf("newer store: %q", c)
	}
}

func TestShutdownAnswersThenExits(t *testing.T) {
	h := start(t)
	result(t, h.call("hello", map[string]any{"protocol": 1}))
	h.call("shutdown", map[string]any{})
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not exit after shutdown")
	}
}

func TestMessageIDIsStable(t *testing.T) {
	a, b := waMessageID("outbox-1"), waMessageID("outbox-1")
	if a != b || !strings.HasPrefix(string(a), "3EB0") || len(a) != 22 || waMessageID("outbox-2") == a {
		t.Fatalf("ids: %s %s", a, b)
	}
}

func TestStoreDSNWindowsPath(t *testing.T) {
	if got := storeDSN("C:/Users/Budi Santoso/AppData/wa/u.db", ""); !strings.HasPrefix(got, "file:C:/Users/Budi%20Santoso/") {
		t.Fatalf("windows dsn: %s", got)
	}
	if got := storeDSN("/home/x/wa/u.db", ""); !strings.HasPrefix(got, "file:///home/x/wa/u.db?") {
		t.Fatalf("unix dsn: %s", got)
	}
}

// Live check against WhatsApp (opt-in, WA_SENDER_LIVE=1): an unlinked unit asks
// for a pairing QR and gets one. Nothing is linked.
func TestLivePairingQR(t *testing.T) {
	if os.Getenv("WA_SENDER_LIVE") != "1" {
		t.Skip("set WA_SENDER_LIVE=1 to reach WhatsApp")
	}
	h := start(t)
	result(t, h.call("hello", map[string]any{"protocol": 1}))
	result(t, h.call("open_unit", map[string]any{"unit": "live", "storePath": filepath.Join(t.TempDir(), "l.db"), "storeKeyHex": testKey}))
	result(t, h.call("pair_qr", map[string]any{"unit": "live"}))
	deadline := time.After(40 * time.Second)
	for {
		select {
		case m := <-h.lines:
			if m["event"] == "qr" {
				data := m["data"].(map[string]any)
				if code, _ := data["code"].(string); len(code) < 50 {
					t.Fatalf("short qr: %v", data)
				}
				h.call("cancel_pair", map[string]any{"unit": "live"})
				return
			}
		case <-deadline:
			t.Fatal("no qr event")
		}
	}
}
