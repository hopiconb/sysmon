// Package ipc streams live samples from the daemon to attached clients
// over a unix socket, one JSON-encoded Sample per line.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"

	"github.com/hopiconb/sysmon/internal/collector"
)

// SocketPath returns the default socket location under XDG_RUNTIME_DIR,
// falling back to /tmp for sessions without one.
func SocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir + "/sysmon.sock"
	}
	return fmt.Sprintf("/tmp/sysmon-%d.sock", os.Getuid())
}

// Server broadcasts samples to every connected client. Clients that stop
// reading are dropped rather than blocking the daemon's sample loop.
type Server struct {
	ln      net.Listener
	mu      sync.Mutex
	clients map[net.Conn]chan []byte
}

func Listen(path string) (*Server, error) {
	// Remove a stale socket from a previous run — but only if nothing is
	// listening on it, so a second daemon can't hijack a live one.
	if _, err := os.Stat(path); err == nil {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil, fmt.Errorf("daemon already running on %s", path)
		}
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, clients: map[net.Conn]chan []byte{}}
	go s.accept()
	return s, nil
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, fs.ErrClosed) {
				return
			}
			continue
		}
		ch := make(chan []byte, 64)
		s.mu.Lock()
		s.clients[conn] = ch
		s.mu.Unlock()
		go s.writeLoop(conn, ch)
	}
}

func (s *Server) writeLoop(conn net.Conn, ch chan []byte) {
	defer s.drop(conn)
	for line := range ch {
		if _, err := conn.Write(line); err != nil {
			return
		}
	}
}

func (s *Server) drop(conn net.Conn) {
	s.mu.Lock()
	delete(s.clients, conn)
	s.mu.Unlock()
	conn.Close()
}

func (s *Server) Broadcast(sm collector.Sample) {
	line, err := json.Marshal(sm)
	if err != nil {
		return
	}
	line = append(line, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn, ch := range s.clients {
		select {
		case ch <- line:
		default:
			// Slow client: close its channel so writeLoop exits and drops it.
			delete(s.clients, conn)
			close(ch)
		}
	}
}

func (s *Server) Close() error {
	s.mu.Lock()
	for conn, ch := range s.clients {
		delete(s.clients, conn)
		close(ch)
		conn.Close()
	}
	s.mu.Unlock()
	return s.ln.Close()
}

// Dial connects to a running daemon and returns a channel of live samples.
// The channel closes when the daemon goes away.
func Dial(path string) (<-chan collector.Sample, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	ch := make(chan collector.Sample)
	go func() {
		defer close(ch)
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var sm collector.Sample
			if err := json.Unmarshal(sc.Bytes(), &sm); err != nil {
				continue
			}
			ch <- sm
		}
	}()
	return ch, nil
}
