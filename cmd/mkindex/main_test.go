package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mkindex-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "mkindex")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func run(t *testing.T, seed string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"WA_EXT_SIGNING_KEY=" + seed}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func key(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), base64.RawURLEncoding.EncodeToString(seed)
}

func readIndex(t *testing.T, dir string, pub ed25519.PublicKey) index {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	sigB64, err := os.ReadFile(filepath.Join(dir, "index.json.sig"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil || !ed25519.Verify(pub, body, sig) {
		t.Fatalf("signature does not verify (err %v)", err)
	}
	var idx index
	if err := json.Unmarshal(body, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

// The feed lists every built platform with its URL, size and SHA-256, is signed
// over its exact bytes, and keeps the previous releases (newest first, no duplicate).
func TestWritesASignedFeed(t *testing.T) {
	pub, seed := key(t)
	dist := t.TempDir()
	linux := []byte("linux binary")
	win := []byte("windows binary!")
	_ = os.WriteFile(filepath.Join(dist, "wa-sender-linux-amd64"), linux, 0o755)
	_ = os.WriteFile(filepath.Join(dist, "wa-sender-windows-amd64.exe"), win, 0o755)
	prev := filepath.Join(t.TempDir(), "prev.json")
	old, _ := json.Marshal(index{Name: "wa-sender", Releases: []release{{Version: "1.3.0", Protocol: 1}, {Version: "1.4.0", Protocol: 1}}})
	_ = os.WriteFile(prev, old, 0o644)

	out := t.TempDir()
	if msg, err := run(t, seed, "-version", "1.4.0", "-min-jaris", "1.2.0", "-dist", dist,
		"-base-url", "https://example.com/v1.4.0", "-source", "https://example.com/tree/v1.4.0", "-prev", prev, "-out", out); err != nil {
		t.Fatalf("mkindex: %v\n%s", err, msg)
	}
	idx := readIndex(t, out, pub)
	if len(idx.Releases) != 2 || idx.Releases[0].Version != "1.4.0" || idx.Releases[1].Version != "1.3.0" {
		t.Fatalf("releases: %+v", idx.Releases)
	}
	r := idx.Releases[0]
	if r.MinJaris != "1.2.0" || r.Protocol != 1 || len(r.Files) != 2 {
		t.Fatalf("release: %+v", r)
	}
	sum := sha256.Sum256(linux)
	f := r.Files["linux-x64"]
	if f.URL != "https://example.com/v1.4.0/wa-sender-linux-amd64" || f.Size != int64(len(linux)) || f.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("linux file: %+v", f)
	}
	if r.Files["win32-x64"].Size != int64(len(win)) {
		t.Fatalf("windows file: %+v", r.Files["win32-x64"])
	}
}

func TestRefusesBadInput(t *testing.T) {
	_, seed := key(t)
	dist := t.TempDir()
	_ = os.WriteFile(filepath.Join(dist, "wa-sender-linux-amd64"), []byte("x"), 0o755)
	base := []string{"-min-jaris", "1.0.0", "-dist", dist, "-base-url", "https://e", "-source", "https://s", "-out", t.TempDir()}
	for name, c := range map[string]struct {
		seed string
		args []string
	}{
		"no key":          {"", append([]string{"-version", "1.0.0"}, base...)},
		"bad version":     {seed, append([]string{"-version", "1.0"}, base...)},
		"no binaries":     {seed, []string{"-version", "1.0.0", "-min-jaris", "1.0.0", "-dist", t.TempDir(), "-base-url", "https://e", "-source", "https://s", "-out", t.TempDir()}},
		"missing sources": {seed, []string{"-version", "1.0.0", "-dist", dist, "-out", t.TempDir()}},
	} {
		if out, err := run(t, c.seed, c.args...); err == nil {
			t.Fatalf("%s: accepted\n%s", name, out)
		}
	}
}

// -genkey prints a pair whose seed signs what the public key verifies.
func TestGenkey(t *testing.T) {
	out, err := run(t, "", "-genkey")
	if err != nil {
		t.Fatal(err)
	}
	var pubB64, seedB64 string
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "public (compile into Jaris): "); ok {
			pubB64 = v
		}
		if v, ok := strings.CutPrefix(l, "private seed (WA_EXT_SIGNING_KEY secret): "); ok {
			seedB64 = v
		}
	}
	pub, err1 := base64.RawURLEncoding.DecodeString(pubB64)
	seed, err2 := base64.RawURLEncoding.DecodeString(seedB64)
	if err1 != nil || err2 != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("genkey output: %q", out)
	}
	if !ed25519.Verify(pub, []byte("x"), ed25519.Sign(ed25519.NewKeyFromSeed(seed), []byte("x"))) {
		t.Fatal("pair does not match")
	}
}
