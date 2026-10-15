// Package replication provides module-level functionality for replication.
// input: a target TCP address
// output: faultProxy, an in-test TCP proxy that forwards, black-holes (like SIGSTOP on a proxy: connections are accepted and kept, nothing is forwarded either way) or kills (every connection closed, new ones refused) the source
// pos: test helper for the source liveness e2e tests (ADR 0009 P2-4, #383 QA)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type proxyMode int

const (
	proxyForward proxyMode = iota
	proxyBlackhole
	proxyKilled
)

type faultProxy struct {
	t      *testing.T
	target string
	addr   string
	mu     sync.Mutex
	mode   proxyMode
	ln     net.Listener
	conns  map[net.Conn]bool
	resume chan struct{}
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	t.Helper()
	p := &faultProxy{t: t, target: target, conns: map[net.Conn]bool{}, resume: make(chan struct{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.ln = ln
	go p.accept(ln)
	t.Cleanup(p.close)
	return p
}

func (p *faultProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *faultProxy) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conns[c] = true
		p.mu.Unlock()
		go p.serve(c)
	}
}

// gate blocks while the proxy is black-holed and reports false once killed.
func (p *faultProxy) gate() bool {
	for {
		p.mu.Lock()
		mode, resume := p.mode, p.resume
		p.mu.Unlock()
		switch mode {
		case proxyForward:
			return true
		case proxyKilled:
			return false
		}
		<-resume
	}
}

func (p *faultProxy) serve(c net.Conn) {
	if !p.gate() {
		c.Close()
		return
	}
	s, err := net.DialTimeout("tcp", p.target, 2*time.Second)
	if err != nil {
		c.Close()
		return
	}
	p.mu.Lock()
	p.conns[s] = true
	p.mu.Unlock()
	go p.pipe(s, c)
	p.pipe(c, s)
}

func (p *faultProxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if !p.gate() {
				return
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				return
			}
			return
		}
	}
}

// blackhole stops all forwarding; connections stay open and new ones are
// accepted but never answered.
func (p *faultProxy) blackhole() {
	p.mu.Lock()
	p.mode = proxyBlackhole
	p.mu.Unlock()
}

// kill closes every connection and refuses new ones.
func (p *faultProxy) kill() {
	p.mu.Lock()
	p.mode = proxyKilled
	close(p.resume)
	p.resume = make(chan struct{})
	p.ln.Close()
	for c := range p.conns {
		c.Close()
	}
	p.conns = map[net.Conn]bool{}
	p.mu.Unlock()
}

// restore forwards again (after a kill, on the same port).
func (p *faultProxy) restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode == proxyKilled {
		ln, err := net.Listen("tcp", p.addr)
		if err != nil {
			p.t.Errorf("relisten %s: %v", p.addr, err)
			return
		}
		p.ln = ln
		go p.accept(ln)
	}
	p.mode = proxyForward
	close(p.resume)
	p.resume = make(chan struct{})
}

func (p *faultProxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = proxyKilled
	p.ln.Close()
	for c := range p.conns {
		c.Close()
	}
	close(p.resume)
	p.resume = make(chan struct{})
}
