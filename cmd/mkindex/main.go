// Command mkindex writes the signed release feed (index.json + index.json.sig)
// that Jaris Desktop installs and updates the extension from.
//
//	mkindex -genkey                      # print a new Ed25519 keypair
//	WA_EXT_SIGNING_KEY=<seed> mkindex -version 1.4.0 -min-jaris 1.2.0 \
//	    -dist dist -base-url https://github.com/jaris-desktop/wa-sender/releases/download/v1.4.0 \
//	    -source https://github.com/jaris-desktop/wa-sender/tree/v1.4.0 -prev old-index.json -out dist
//
// The feed lists every release (newest first, at most 30) so a host can stay on the
// newest one its own version supports.
//
// This program is part of wa-sender and licensed under the GNU GPL v3 (see LICENSE).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type file struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type release struct {
	Version  string          `json:"version"`
	Protocol int             `json:"protocol"`
	MinJaris string          `json:"minJaris"`
	Released string          `json:"released"`
	Source   string          `json:"source"`
	Files    map[string]file `json:"files"`
}

type index struct {
	Name     string    `json:"name"`
	Releases []release `json:"releases"`
}

// platforms maps a release asset to the host's platform key (Node's
// process.platform-process.arch).
var platforms = map[string]string{
	"wa-sender-windows-amd64.exe": "win32-x64",
	"wa-sender-windows-arm64.exe": "win32-arm64",
	"wa-sender-linux-amd64":       "linux-x64",
	"wa-sender-linux-arm64":       "linux-arm64",
	"wa-sender-darwin-amd64":      "darwin-x64",
	"wa-sender-darwin-arm64":      "darwin-arm64",
}

var semver = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func main() {
	genkey := flag.Bool("genkey", false, "print a new Ed25519 keypair and exit")
	ver := flag.String("version", "", "release version (x.y.z)")
	proto := flag.Int("protocol", 1, "protocol version")
	minJaris := flag.String("min-jaris", "1.0.0", "oldest Jaris Desktop version this release works with")
	dist := flag.String("dist", "dist", "directory with the release binaries")
	baseURL := flag.String("base-url", "", "download URL of the release's assets")
	source := flag.String("source", "", "URL of this release's source code")
	prev := flag.String("prev", "", "the previous index.json (its releases are kept)")
	out := flag.String("out", ".", "where to write index.json and index.json.sig")
	flag.Parse()

	if *genkey {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fail(err)
		}
		fmt.Printf("public (compile into Jaris): %s\nprivate seed (WA_EXT_SIGNING_KEY secret): %s\n",
			base64.RawURLEncoding.EncodeToString(pub), base64.RawURLEncoding.EncodeToString(priv.Seed()))
		return
	}
	seed, err := base64.RawURLEncoding.DecodeString(os.Getenv("WA_EXT_SIGNING_KEY"))
	if err != nil || len(seed) != ed25519.SeedSize {
		fail(fmt.Errorf("WA_EXT_SIGNING_KEY must be a base64url Ed25519 seed"))
	}
	if !semver.MatchString(*ver) || !semver.MatchString(*minJaris) || *baseURL == "" || *source == "" {
		fail(fmt.Errorf("-version, -min-jaris (x.y.z), -base-url and -source are required"))
	}

	rel := release{Version: *ver, Protocol: *proto, MinJaris: *minJaris,
		Released: time.Now().UTC().Format("2006-01-02"), Source: *source, Files: map[string]file{}}
	for name, platform := range platforms {
		b, err := os.ReadFile(filepath.Join(*dist, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			fail(err)
		}
		sum := sha256.Sum256(b)
		rel.Files[platform] = file{URL: *baseURL + "/" + name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))}
	}
	if len(rel.Files) == 0 {
		fail(fmt.Errorf("no release binaries in %s", *dist))
	}

	idx := index{Name: "wa-sender", Releases: []release{rel}}
	if *prev != "" {
		var old index
		b, err := os.ReadFile(*prev)
		if err != nil {
			fail(err)
		}
		if err := json.Unmarshal(b, &old); err != nil {
			fail(err)
		}
		for _, r := range old.Releases {
			if r.Version != rel.Version && len(idx.Releases) < 30 {
				idx.Releases = append(idx.Releases, r)
			}
		}
	}
	body, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		fail(err)
	}
	body = append(body, '\n')
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), body)
	if err := os.WriteFile(filepath.Join(*out, "index.json"), body, 0o644); err != nil {
		fail(err)
	}
	if err := os.WriteFile(filepath.Join(*out, "index.json.sig"), []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("index.json: %d release(s), %s has %d platform(s)\n", len(idx.Releases), rel.Version, len(rel.Files))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mkindex:", err)
	os.Exit(1)
}
