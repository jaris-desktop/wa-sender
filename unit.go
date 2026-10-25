package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// States (PROTOCOL.md).
const (
	stateUnpaired     = "unpaired"
	statePairing      = "pairing"
	stateConnecting   = "connecting"
	stateConnected    = "connected"
	stateDisconnected = "disconnected"
	stateLoggedOut    = "logged_out"
	stateReplaced     = "replaced"
	stateTempBan      = "temp_ban"
	stateOutdated     = "outdated"
)

const (
	pairTimeout = 3 * time.Minute
	sendTimeout = 60 * time.Second
	// pairDisplayName is what the phone lists under "Linked devices"; WhatsApp only
	// accepts "Browser (OS)" with common browsers.
	pairDisplayName = "Chrome (Linux)"
)

func init() {
	store.SetOSInfo("Jaris", [3]uint32{1, 0, 0})
}

type emitFunc func(event, unit string, data any)

// unitSet holds the open units.
type unitSet struct {
	ctx   context.Context
	emit  emitFunc
	mu    sync.Mutex
	units map[string]*unit
}

func newUnitSet(ctx context.Context, emit emitFunc) *unitSet {
	return &unitSet{ctx: ctx, emit: emit, units: map[string]*unit{}}
}

func (s *unitSet) get(id string) (*unit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u := s.units[id]; u != nil {
		return u, nil
	}
	return nil, errNotOpen
}

func (s *unitSet) open(id, path, keyHex string) (any, error) {
	if u, err := s.get(id); err == nil {
		return u.snapshot(), nil
	}
	if keyHex != "" {
		if b, err := hex.DecodeString(keyHex); err != nil || len(b) != 32 {
			return nil, errBadRequest
		}
	}
	u, err := openUnit(s.ctx, id, path, keyHex, s.emit)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if other := s.units[id]; other != nil { // opened concurrently
		s.mu.Unlock()
		u.close()
		return other.snapshot(), nil
	}
	s.units[id] = u
	s.mu.Unlock()
	u.start()
	return u.snapshot(), nil
}

func (s *unitSet) close(id string) {
	s.mu.Lock()
	u := s.units[id]
	delete(s.units, id)
	s.mu.Unlock()
	if u != nil {
		u.close()
	}
}

func (s *unitSet) logout(id string) error {
	u, err := s.get(id)
	if err != nil {
		return err
	}
	if err := u.logout(); err != nil {
		return err
	}
	s.close(id)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(u.path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return coded(codeStoreError, err)
		}
	}
	return nil
}

func (s *unitSet) closeAll() {
	s.mu.Lock()
	all := s.units
	s.units = map[string]*unit{}
	s.mu.Unlock()
	for _, u := range all {
		u.close()
	}
}

// unit is one school's WhatsApp account and store.
type unit struct {
	id   string
	path string
	ctx  context.Context
	emit emitFunc
	db   *sql.DB
	box  *sqlstore.Container

	mu         sync.Mutex
	cli        *whatsmeow.Client
	state      string
	expiresAt  time.Time
	pairCancel context.CancelFunc
}

func openUnit(ctx context.Context, id, path, keyHex string, emit emitFunc) (*unit, error) {
	waitWAVersion()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, coded(codeStoreError, err)
	}
	db, err := sql.Open("sqlite3", storeDSN(path, keyHex))
	if err != nil {
		return nil, coded(codeStoreError, err)
	}
	box := sqlstore.NewWithDB(db, "sqlite3", stderrLogger("store"))
	if err := box.Upgrade(ctx); err != nil {
		_ = db.Close()
		if errors.Is(err, dbutil.ErrUnsupportedDatabaseVersion) {
			return nil, coded(codeStoreNewer, err)
		}
		return nil, coded(codeStoreError, err)
	}
	dev, err := box.GetFirstDevice(ctx)
	if err != nil {
		_ = db.Close()
		return nil, coded(codeStoreError, err)
	}
	u := &unit{id: id, path: path, ctx: ctx, emit: emit, db: db, box: box, state: stateUnpaired}
	u.cli = u.newClient(dev)
	if dev.ID != nil {
		u.state = stateConnecting
	}
	return u, nil
}

// start connects a linked unit; whatsmeow reconnects by itself after that.
func (u *unit) start() {
	u.mu.Lock()
	cli, linked := u.cli, u.cli.Store.ID != nil
	u.mu.Unlock()
	if linked {
		go func() {
			if err := cli.Connect(); err != nil {
				log.Printf("unit %s: connect: %v", u.id, err)
				u.setState(stateDisconnected)
			}
		}()
	}
}

func (u *unit) newClient(dev *store.Device) *whatsmeow.Client {
	cli := whatsmeow.NewClient(dev, stderrLogger("client"))
	cli.AddEventHandler(u.onEvent)
	return cli
}

func (u *unit) onEvent(evt any) {
	switch e := evt.(type) {
	case *events.Connected:
		u.setState(stateConnected)
	case *events.Disconnected:
		u.mu.Lock()
		cur := u.state
		u.mu.Unlock()
		if cur == stateConnected || cur == stateConnecting {
			u.setState(stateDisconnected)
		}
	case *events.LoggedOut:
		u.setState(stateLoggedOut)
	case *events.StreamReplaced:
		u.setState(stateReplaced)
	case *events.TemporaryBan:
		u.mu.Lock()
		u.expiresAt = time.Now().Add(e.Expire).UTC()
		u.mu.Unlock()
		u.setState(stateTempBan)
	case *events.ClientOutdated:
		u.setState(stateOutdated)
		go u.retryAfterVersionRefresh()
	case *events.PairSuccess:
		u.setState(stateConnecting)
	case *events.PairError:
		u.emit("pair_error", u.id, map[string]string{"message": fmt.Sprint(e.Error)})
	}
}

// retryAfterVersionRefresh reconnects once a newer WhatsApp Web version was found.
func (u *unit) retryAfterVersionRefresh() {
	if !refreshWAVersion(u.ctx) {
		return
	}
	u.mu.Lock()
	cli := u.cli
	u.mu.Unlock()
	cli.Disconnect()
	u.setState(stateConnecting)
	if err := cli.Connect(); err != nil {
		log.Printf("unit %s: reconnect after version refresh: %v", u.id, err)
	}
}

func (u *unit) setState(s string) {
	u.mu.Lock()
	u.state = s
	if s != stateTempBan {
		u.expiresAt = time.Time{}
	}
	u.mu.Unlock()
	u.emit("state", u.id, u.snapshot())
}

func (u *unit) snapshot() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string]any{"state": u.state}
	if id := u.cli.Store.ID; id != nil {
		out["jid"] = id.User
		if name := u.cli.Store.PushName; name != "" {
			out["pushName"] = name
		}
	}
	if !u.expiresAt.IsZero() {
		out["expiresAt"] = u.expiresAt.Format(time.RFC3339)
	}
	return out
}

// pairClient prepares a fresh, unlinked client and starts the QR channel.
func (u *unit) pairClient() (*whatsmeow.Client, <-chan whatsmeow.QRChannelItem, context.Context, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cli.Store.ID != nil {
		return nil, nil, nil, coded(codeAlreadyPaired, errors.New("an account is already linked"))
	}
	if u.pairCancel != nil {
		u.pairCancel()
	}
	u.cli.Disconnect()
	if u.cli.Store.Deleted { // logged out earlier: its device rows are gone
		u.cli = u.newClient(u.box.NewDevice())
	}
	ctx, cancel := context.WithTimeout(u.ctx, pairTimeout)
	ch, err := u.cli.GetQRChannel(ctx)
	if err != nil {
		cancel()
		return nil, nil, nil, coded(codePairFailed, err)
	}
	if err := u.cli.Connect(); err != nil {
		cancel()
		return nil, nil, nil, coded(codePairFailed, err)
	}
	u.pairCancel = cancel
	u.state = statePairing
	return u.cli, ch, ctx, nil
}

func (u *unit) pairQR() error {
	_, ch, _, err := u.pairClient()
	if err != nil {
		return err
	}
	u.emit("state", u.id, u.snapshot())
	go u.drainPairing(ch, true)
	return nil
}

func (u *unit) pairCode(phone string) (string, error) {
	cli, ch, ctx, err := u.pairClient()
	if err != nil {
		return "", err
	}
	u.emit("state", u.id, u.snapshot())
	// The phone code may only be requested once the socket is up: the QR channel's
	// first code says so.
	select {
	case item, ok := <-ch:
		if !ok || item.Event != "code" {
			u.endPairing()
			return "", coded(codePairFailed, fmt.Errorf("pairing did not start (%s)", item.Event))
		}
	case <-ctx.Done():
		u.endPairing()
		return "", coded(codePairFailed, ctx.Err())
	}
	code, err := cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, pairDisplayName)
	if err != nil {
		u.endPairing()
		return "", coded(codePairFailed, err)
	}
	go u.drainPairing(ch, false)
	return code, nil
}

// drainPairing follows the QR channel until the pairing ends.
func (u *unit) drainPairing(ch <-chan whatsmeow.QRChannelItem, showQR bool) {
	for item := range ch {
		switch {
		case item.Event == "code":
			if showQR {
				u.emit("qr", u.id, map[string]any{"code": item.Code, "timeoutSeconds": int(item.Timeout.Seconds())})
			}
		case item == whatsmeow.QRChannelSuccess:
			u.mu.Lock()
			u.pairCancel = nil
			u.mu.Unlock()
			return // PairSuccess / Connected set the state
		case item.Error != nil:
			u.emit("pair_error", u.id, map[string]string{"message": item.Error.Error()})
		case item.Event != "":
			u.emit("pair_error", u.id, map[string]string{"message": item.Event})
		}
	}
	u.endPairing()
}

// endPairing drops an unfinished pairing back to unpaired.
func (u *unit) endPairing() {
	u.mu.Lock()
	if u.pairCancel != nil {
		u.pairCancel()
		u.pairCancel = nil
	}
	cli := u.cli
	linked := cli.Store.ID != nil
	u.mu.Unlock()
	if !linked {
		cli.Disconnect()
		u.setState(stateUnpaired)
	}
}

func (u *unit) cancelPair() { u.endPairing() }

func (u *unit) ready() (*whatsmeow.Client, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cli.Store.ID == nil {
		return nil, coded(codeNotPaired, errors.New("no account linked"))
	}
	if u.state != stateConnected || !u.cli.IsConnected() {
		return nil, coded(codeNotConnected, fmt.Errorf("unit is %s", u.state))
	}
	return u.cli, nil
}

func (u *unit) checkNumber(phone string) (bool, error) {
	cli, err := u.ready()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(u.ctx, sendTimeout)
	defer cancel()
	res, err := cli.IsOnWhatsApp(ctx, []string{"+" + phone})
	if err != nil {
		return false, coded(codeSendFailed, err)
	}
	return len(res) > 0 && res[0].IsIn, nil
}

func (u *unit) send(to, text, messageID string) (any, error) {
	cli, err := u.ready()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(u.ctx, sendTimeout)
	defer cancel()
	jid := types.NewJID(to, types.DefaultUserServer)
	resp, err := cli.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(text)},
		whatsmeow.SendRequestExtra{ID: waMessageID(messageID)})
	switch {
	case errors.Is(err, whatsmeow.ErrNotConnected):
		return nil, coded(codeNotConnected, err)
	case errors.Is(err, whatsmeow.ErrNotLoggedIn):
		return nil, coded(codeNotPaired, err)
	case err != nil:
		return nil, coded(codeSendFailed, err)
	}
	return map[string]string{"messageId": string(resp.ID), "timestamp": resp.Timestamp.UTC().Format(time.RFC3339)}, nil
}

// waMessageID derives a WhatsApp Web-style message id from the host's id, so a
// resend after a crash carries the same id.
func waMessageID(hostID string) types.MessageID {
	sum := sha256.Sum256([]byte("wa-sender-message:" + hostID))
	return types.MessageID(whatsmeow.WebMessageIDPrefix + strings.ToUpper(hex.EncodeToString(sum[:9])))
}

func (u *unit) logout() error {
	u.mu.Lock()
	cli := u.cli
	if u.pairCancel != nil {
		u.pairCancel()
		u.pairCancel = nil
	}
	u.mu.Unlock()
	if cli.Store.ID == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(u.ctx, sendTimeout)
	defer cancel()
	if err := cli.Logout(ctx); err != nil {
		// Offline or already unlinked on the phone: forget the session locally.
		log.Printf("unit %s: logout: %v (deleting the local session)", u.id, err)
		if derr := cli.Store.Delete(ctx); derr != nil {
			return coded(codeStoreError, derr)
		}
	}
	return nil
}

func (u *unit) close() {
	u.mu.Lock()
	if u.pairCancel != nil {
		u.pairCancel()
		u.pairCancel = nil
	}
	cli := u.cli
	u.mu.Unlock()
	cli.Disconnect()
	_ = u.db.Close()
}

// storeDSN opens the store with the same SQLite settings as Jaris' own database.
func storeDSN(path, keyHex string) string {
	q := url.Values{}
	if keyHex != "" {
		q.Set("vfs", "adiantum")
		q.Set("hexkey", keyHex)
	}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "foreign_keys(ON)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	p := filepath.ToSlash(path)
	u := url.URL{Scheme: "file", RawQuery: q.Encode()}
	if len(p) >= 2 && p[1] == ':' { // Windows drive path: "file:C:/…", no authority
		u.Opaque = (&url.URL{Path: p}).EscapedPath()
	} else {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		u.Path = p
	}
	return u.String()
}

// stderrLogger keeps whatsmeow's logs off stdout (the protocol stream).
func stderrLogger(module string) waLog.Logger { return &stderrLog{module: module} }

type stderrLog struct{ module string }

func (l *stderrLog) Errorf(msg string, args ...any) {
	log.Printf("[%s] ERROR "+msg, append([]any{l.module}, args...)...)
}
func (l *stderrLog) Warnf(msg string, args ...any) {
	log.Printf("[%s] WARN "+msg, append([]any{l.module}, args...)...)
}
func (l *stderrLog) Infof(string, ...any)  {}
func (l *stderrLog) Debugf(string, ...any) {}
func (l *stderrLog) Sub(module string) waLog.Logger {
	return &stderrLog{module: l.module + "/" + module}
}
