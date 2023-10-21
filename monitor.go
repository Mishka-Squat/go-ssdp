package ssdp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/koron/go-ssdp/internal/multicast"
	"github.com/koron/go-ssdp/internal/ssdplog"
)

// Monitor monitors SSDP's alive and byebye messages.
type Monitor struct {
	OnAlive  AliveHandler
	OnBye    ByeHandler
	OnSearch SearchHandler

	Options []Option

	conn *multicast.Conn
	wg   sync.WaitGroup
}

// Start starts to monitor SSDP messages.
func (m *Monitor) Start() error {
	cfg, err := opts2config(m.Options)
	if err != nil {
		return err
	}
	conn, err := multicast.Listen(cfg.laddrResolver(), cfg.raddrResolver(), cfg.multicastConfig.options()...)
	if err != nil {
		return err
	}
	ssdplog.Printf("monitoring on %s", conn.LocalAddr().String())
	m.conn = conn
	m.wg.Add(1)
	go func() {
		m.serve()
		m.wg.Done()
	}()
	return nil
}

func (m *Monitor) Search(searchType string, waitSec int, opts ...Option) error {
	cfg, err := opts2config(opts)
	if err != nil {
		return err
	}
	// dial multicast UDP packet.
	conn, err := multicast.Listen(&multicast.AddrResolver{Addr: ""}, cfg.multicastConfig.options()...)
	if err != nil {
		return err
	}
	defer conn.Close()
	ssdplog.Printf("search on %s", conn.LocalAddr().String())

	// send request.
	addr, err := multicast.SendAddr()
	if err != nil {
		return err
	}
	msg, err := buildSearch(addr, searchType, waitSec)
	if err != nil {
		return err
	}
	if _, err := conn.WriteTo(multicast.BytesDataProvider(msg), addr); err != nil {
		return err
	}

	err = conn.ReadPackets(0, func(addr net.Addr, raw []byte) error {
		return m.handleSeqrchResponse(addr, raw)
	})

	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	return nil
}

func (m *Monitor) serve() error {
	// TODO: update listening interfaces of m.conn
	err := m.conn.ReadPackets(0, func(addr net.Addr, data []byte) error {
		//msg := make([]byte, len(data))
		//copy(msg, data)
		return m.handleRaw(addr, data)
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (m *Monitor) handleRaw(addr net.Addr, raw []byte) error {
	// Add newline to workaround buggy SSDP responses
	if !bytes.HasSuffix(raw, endOfHeader) {
		raw = bytes.Join([][]byte{raw, endOfHeader}, nil)
	}
	if bytes.HasPrefix(raw, []byte("M-SEARCH ")) {
		return m.handleSearch(addr, raw)
	}
	if bytes.HasPrefix(raw, []byte("NOTIFY ")) {
		return m.handleNotify(addr, raw)
	}
	n := bytes.Index(raw, []byte("\r\n"))
	ssdplog.Printf("unexpected method: %q", string(raw[:n]))
	return nil
}

func (m *Monitor) handleNotify(addr net.Addr, raw []byte) error {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return err
	}

	header := req.Header
	method := req.Method

	switch nts := header.Get("NTS"); nts {
	case "ssdp:alive":
		if method != "NOTIFY" {
			return fmt.Errorf("unexpected method for %q: %s", "ssdp:alive", method)
		}
		if h := m.OnAlive; h != nil {
			h(&AliveMessage{
				From:      addr,
				Type:      header.Get("NT"),
				USN:       header.Get("USN"),
				Location:  header.Get("LOCATION"),
				Server:    header.Get("SERVER"),
				rawHeader: header,
			})
		}
	case "ssdp:byebye":
		if method != "NOTIFY" {
			return fmt.Errorf("unexpected method for %q: %s", "ssdp:byebye", method)
		}
		if h := m.OnBye; h != nil {
			h(&ByeMessage{
				From:      addr,
				Type:      header.Get("NT"),
				USN:       header.Get("USN"),
				rawHeader: header,
			})
		}
	default:
		return fmt.Errorf("unknown NTS: %s", nts)
	}
	return nil
}

func (m *Monitor) handleSearch(addr net.Addr, raw []byte) error {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return err
	}
	man := req.Header.Get("MAN")
	if man != `"ssdp:discover"` {
		return fmt.Errorf("unexpected MAN: %s", man)
	}
	if h := m.OnSearch; h != nil {
		h(&SearchMessage{
			From:      addr,
			Type:      req.Header.Get("ST"),
			rawHeader: req.Header,
		})
	}
	return nil
}

func (m *Monitor) handleSeqrchResponse(addr net.Addr, raw []byte) error {
	req, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		return err
	}

	header := req.Header

	if h := m.OnAlive; h != nil {
		h(&AliveMessage{
			From:      addr,
			Type:      header.Get("ST"),
			USN:       header.Get("USN"),
			Location:  header.Get("LOCATION"),
			Server:    header.Get("SERVER"),
			rawHeader: header,
		})
	}

	return nil
}

// Close closes monitoring.
func (m *Monitor) Close() error {
	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
		m.wg.Wait()
	}
	return nil
}

// AliveMessage represents SSDP's ssdp:alive message.
type AliveMessage struct {
	// From is a sender of this message
	From net.Addr

	// Type is a property of "NT"
	Type string

	// USN is a property of "USN"
	USN string

	// Location is a property of "LOCATION"
	Location string

	// Server is a property of "SERVER"
	Server string

	rawHeader http.Header
	maxAge    *int
}

// Header returns all properties in alive message.
func (m *AliveMessage) Header() http.Header {
	return m.rawHeader
}

// MaxAge extracts "max-age" value from "CACHE-CONTROL" property.
func (m *AliveMessage) MaxAge() int {
	if m.maxAge == nil {
		m.maxAge = new(int)
		*m.maxAge = extractMaxAge(m.rawHeader.Get("CACHE-CONTROL"), -1)
	}
	return *m.maxAge
}

// AliveHandler is handler of Alive message.
type AliveHandler func(*AliveMessage)

// ByeMessage represents SSDP's ssdp:byebye message.
type ByeMessage struct {
	// From is a sender of this message
	From net.Addr

	// Type is a property of "NT"
	Type string

	// USN is a property of "USN"
	USN string

	rawHeader http.Header
}

// Header returns all properties in bye message.
func (m *ByeMessage) Header() http.Header {
	return m.rawHeader
}

// ByeHandler is handler of Bye message.
type ByeHandler func(*ByeMessage)

// SearchMessage represents SSDP's ssdp:discover message.
type SearchMessage struct {
	From net.Addr
	Type string

	rawHeader http.Header
}

// Header returns all properties in search message.
func (s *SearchMessage) Header() http.Header {
	return s.rawHeader
}

func (s *SearchMessage) Mx() int {
	mx, err := strconv.Atoi(s.rawHeader.Get("MX"))
	if err != nil {
		return 0
	}

	return mx
}

// SearchHandler is handler of Search message.
type SearchHandler func(*SearchMessage)
