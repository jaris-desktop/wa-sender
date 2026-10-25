package main

import (
	"context"
	"log"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

var (
	versionOnce  sync.Once
	versionReady = make(chan struct{})
	versionMu    sync.Mutex
)

// refreshWAVersion asks web.whatsapp.com for the current WhatsApp Web version and
// adopts it when newer than the built-in one, so a plain version bump on
// WhatsApp's side needs no new wa-sender release. It reports whether it changed.
// The first call also releases waitWAVersion.
func refreshWAVersion(ctx context.Context) bool {
	defer versionOnce.Do(func() { close(versionReady) })
	versionMu.Lock()
	defer versionMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	latest, err := whatsmeow.GetLatestVersion(ctx, nil)
	if err != nil {
		log.Printf("WhatsApp version check: %v (using %s)", err, store.GetWAVersion())
		return false
	}
	if cur := store.GetWAVersion(); cur.LessThan(*latest) {
		store.SetWAVersion(*latest)
		log.Printf("WhatsApp Web version %s → %s", cur, latest)
		return true
	}
	return false
}

// waitWAVersion waits (briefly) for the startup version check before the first
// connection.
func waitWAVersion() {
	select {
	case <-versionReady:
	case <-time.After(12 * time.Second):
	}
}
