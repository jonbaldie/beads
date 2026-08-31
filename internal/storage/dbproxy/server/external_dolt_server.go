package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/util"
)

type ExternalDoltServer struct {
	externalDoltEndpoint
	ExternalDoltLifecycle
	keepAlivePeriod time.Duration
}

type externalDoltEndpoint struct {
	id     string
	host   string
	port   int
	socket string
}

type ExternalDoltLifecycle struct {
	started atomic.Bool
}

func (e *externalDoltEndpoint) ID(_ context.Context) string {
	return e.id
}

func (e *externalDoltEndpoint) DSN(_ context.Context, database, user, password string) string {
	dsn := util.DoltServerDSN{
		User:     user,
		Password: password,
		Database: database,
	}
	if e.socket != "" {
		dsn.Socket = e.socket
	} else {
		dsn.Host = e.host
		dsn.Port = e.port
	}
	return dsn.String()
}

func (e *ExternalDoltLifecycle) Start(_ context.Context) error {
	if !e.started.CompareAndSwap(false, true) {
		return errors.New("server: ExternalDoltServer.Start: server already started")
	}
	return nil
}

func (e *ExternalDoltLifecycle) Stop(_ context.Context) error {
	e.started.Store(false)
	return nil
}

func (e *ExternalDoltLifecycle) Running(_ context.Context) bool {
	return e.started.Load()
}

var _ DatabaseServer = (*ExternalDoltServer)(nil)

func NewExternalDoltServer(cfg configfile.ExternalDoltConfig) (*ExternalDoltServer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	keepAlive := cfg.KeepAlivePeriod
	if keepAlive == 0 {
		keepAlive = defaultKeepAlivePeriod
	}
	return &ExternalDoltServer{
		externalDoltEndpoint: externalDoltEndpoint{
			id:     ExternalDoltServerID(cfg),
			host:   cfg.Host,
			port:   cfg.Port,
			socket: cfg.Socket,
		},
		keepAlivePeriod: keepAlive,
	}, nil
}

func ExternalDoltServerID(cfg configfile.ExternalDoltConfig) string {
	sum := sha256.Sum256([]byte(externalDoltServerTarget(cfg)))
	return hex.EncodeToString(sum[:])
}

func externalDoltServerTarget(cfg configfile.ExternalDoltConfig) string {
	if cfg.Socket != "" {
		return "unix://" + cfg.Socket
	}
	return "tcp://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
}

func (s *ExternalDoltServer) Dial(ctx context.Context) (net.Conn, error) {
	network, addr := "tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port))
	if s.socket != "" {
		network, addr = "unix", s.socket
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("server: ExternalDoltServer.Dial: %w", err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(s.keepAlivePeriod)
	}
	return conn, nil
}
