// Command wa-sender sends WhatsApp messages from a school's own number on behalf
// of a host program, speaking the line-delimited JSON protocol in PROTOCOL.md over
// stdin/stdout.
//
// # Copyright (C) 2026 Jaris Desktop contributors
//
// This program is free software: you can redistribute it and/or modify it under
// the terms of the GNU General Public License as published by the Free Software
// Foundation, either version 3 of the License, or (at your option) any later
// version. See LICENSE.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
)

// maxLine bounds one protocol line (PROTOCOL.md).
const maxLine = 1 << 20

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		_ = json.NewEncoder(os.Stdout).Encode(versionInfo())
		return
	}
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix("wa-sender: ")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The host stops us by closing stdin; a terminal Ctrl+C or a service manager's
	// SIGTERM does the same.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	s := newServer(ctx, os.Stdout)
	s.run(os.Stdin)
	s.closeAll()
}

// server reads requests, runs them and writes answers and events.
type server struct {
	ctx   context.Context
	out   *lineWriter
	units *unitSet
	hello bool
}

func newServer(ctx context.Context, w io.Writer) *server {
	out := &lineWriter{w: w}
	s := &server{ctx: ctx, out: out}
	s.units = newUnitSet(ctx, out.event)
	go refreshWAVersion(ctx)
	return s
}

type request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// run reads stdin until EOF, a shutdown request or cancellation.
func (s *server) run(in io.Reader) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		for sc.Scan() {
			lines <- append([]byte(nil), sc.Bytes()...)
		}
		if err := sc.Err(); err != nil {
			log.Printf("read: %v", err)
		}
	}()
	for {
		select {
		case <-s.ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			var req request
			if err := json.Unmarshal(line, &req); err != nil || req.ID == 0 || req.Method == "" {
				log.Printf("ignored a malformed line (%d bytes)", len(line))
				continue
			}
			if req.Method == "shutdown" {
				s.out.result(req.ID, struct{}{})
				return
			}
			if req.Method == "hello" {
				s.handleHello(req)
				continue
			}
			if !s.hello {
				s.out.fail(req.ID, codeBadProtocol, "hello must be the first request")
				continue
			}
			go s.dispatch(req)
		}
	}
}

func (s *server) handleHello(req request) {
	var p struct {
		Protocol int `json:"protocol"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Protocol != protocolVersion {
		s.out.fail(req.ID, codeBadProtocol, fmt.Sprintf("this wa-sender speaks protocol %d", protocolVersion))
		return
	}
	s.hello = true
	s.out.result(req.ID, versionInfo())
}

func (s *server) dispatch(req request) {
	res, err := s.call(req.Method, req.Params)
	if err != nil {
		code, msg := codeOf(err)
		s.out.fail(req.ID, code, msg)
		return
	}
	if res == nil {
		res = struct{}{}
	}
	s.out.result(req.ID, res)
}

func (s *server) call(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "open_unit":
		var p struct {
			Unit, StorePath, StoreKeyHex string
		}
		if err := decode(raw, &p); err != nil || p.Unit == "" || p.StorePath == "" {
			return nil, errBadRequest
		}
		return s.units.open(p.Unit, p.StorePath, p.StoreKeyHex)
	case "pair_code":
		var p struct{ Unit, Phone string }
		if err := decode(raw, &p); err != nil || !validPhone(p.Phone) {
			return nil, errBadRequest
		}
		u, err := s.units.get(p.Unit)
		if err != nil {
			return nil, err
		}
		code, err := u.pairCode(p.Phone)
		if err != nil {
			return nil, err
		}
		return map[string]string{"code": code}, nil
	case "pair_qr":
		u, err := s.unitOf(raw)
		if err != nil {
			return nil, err
		}
		return nil, u.pairQR()
	case "cancel_pair":
		u, err := s.unitOf(raw)
		if err != nil {
			return nil, err
		}
		u.cancelPair()
		return nil, nil
	case "status":
		u, err := s.unitOf(raw)
		if err != nil {
			return nil, err
		}
		return u.snapshot(), nil
	case "check_number":
		var p struct{ Unit, Phone string }
		if err := decode(raw, &p); err != nil || !validPhone(p.Phone) {
			return nil, errBadRequest
		}
		u, err := s.units.get(p.Unit)
		if err != nil {
			return nil, err
		}
		on, err := u.checkNumber(p.Phone)
		if err != nil {
			return nil, err
		}
		return map[string]bool{"onWhatsApp": on}, nil
	case "send":
		var p struct{ Unit, To, Text, MessageID string }
		if err := decode(raw, &p); err != nil || !validPhone(p.To) || p.Text == "" || p.MessageID == "" || len(p.MessageID) > 64 {
			return nil, errBadRequest
		}
		u, err := s.units.get(p.Unit)
		if err != nil {
			return nil, err
		}
		return u.send(p.To, p.Text, p.MessageID)
	case "logout":
		var p struct{ Unit string }
		if err := decode(raw, &p); err != nil {
			return nil, errBadRequest
		}
		return nil, s.units.logout(p.Unit)
	case "close_unit":
		var p struct{ Unit string }
		if err := decode(raw, &p); err != nil {
			return nil, errBadRequest
		}
		s.units.close(p.Unit)
		return nil, nil
	}
	return nil, errBadRequest
}

func (s *server) unitOf(raw json.RawMessage) (*unit, error) {
	var p struct{ Unit string }
	if err := decode(raw, &p); err != nil {
		return nil, errBadRequest
	}
	return s.units.get(p.Unit)
}

func (s *server) closeAll() { s.units.closeAll() }

// decode reads params strictly (camelCase keys, matched case-insensitively by
// encoding/json; unknown keys refused).
func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	d := json.NewDecoder(bytesReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// validPhone: international digits without "+" (PROTOCOL.md).
func validPhone(p string) bool {
	if len(p) < 8 || len(p) > 15 || p[0] == '0' {
		return false
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
