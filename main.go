package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tidwall/gjson"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type M = map[string]any
type Network struct {
	RPC           string `json:"rpc"`
	Bearer        string `json:"bearer,omitempty"`
	Confirmations int64  `json:"confirmations,omitempty"`
	Index         string `json:"index,omitempty"`
	IndexBearer   string `json:"indexBearer,omitempty"`
	Cluster       string `json:"cluster,omitempty"`
}
type Config struct {
	Listen          string             `json:"listen"`
	Database        string             `json:"database"`
	Token           string             `json:"token"`
	Scanner         string             `json:"scanner"`
	ScannerToken    string             `json:"scannerToken"`
	BroadcastSecret string             `json:"broadcastSecret"`
	Networks        map[string]Network `json:"networks"`
}
type Service struct {
	cfg   Config
	db    *sql.DB
	http  *http.Client
	locks [128]sync.Mutex
	wake  chan struct{}
}

func openService(c Config) (*Service, error) {
	db, e := sql.Open("sqlite", c.Database)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if _, e = db.Exec(schema); e != nil {
		db.Close()
		return nil, e
	}
	return &Service{cfg: c, db: db, http: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, wake: make(chan struct{}, 1)}, nil
}
func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) atomic(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Service) lock(id int64, token string) func() {
	h := fnv.New32a()
	fmt.Fprint(h, id, "/", token)
	l := &s.locks[h.Sum32()%uint32(len(s.locks))]
	l.Lock()
	return l.Unlock
}
func compact(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return string(b)
}
func nowStamp() int64        { return time.Now().UTC().UnixNano() }
func iso(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (s *Service) request(ctx context.Context, method, endpoint, bearer string, body any) (gjson.Result, int, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewBufferString(compact(body))
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, r)
	if e != nil {
		return gjson.Result{}, 0, errors.New("invalid service URL")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := s.http.Do(req)
	if e != nil {
		return gjson.Result{}, 0, errors.New("upstream request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return gjson.Result{}, resp.StatusCode, fmt.Errorf("upstream HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode == 204 {
		return gjson.Result{}, 204, nil
	}
	const limit = 32 << 20
	b, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if e != nil || len(b) > limit || !gjson.ValidBytes(b) {
		return gjson.Result{}, resp.StatusCode, errors.New("invalid upstream JSON")
	}
	return gjson.ParseBytes(b), resp.StatusCode, nil
}
func run() error {
	path := flag.String("config", "config.json", "configuration path")
	flag.Parse()
	b, e := os.ReadFile(*path)
	if e != nil {
		return e
	}
	var c Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return e
	}
	c.Scanner = os.ExpandEnv(c.Scanner)
	c.Token = os.ExpandEnv(c.Token)
	c.ScannerToken = os.ExpandEnv(c.ScannerToken)
	broadcastEnabled := c.BroadcastSecret != ""
	c.BroadcastSecret = os.ExpandEnv(c.BroadcastSecret)
	if len(c.Token) < 24 || len(c.ScannerToken) < 24 || len(c.Networks) == 0 || c.Database == "" || !validURL(c.Scanner) {
		return errors.New("configure database, scanner, networks and credentials (24 bytes minimum)")
	}
	if broadcastEnabled && (len(c.BroadcastSecret) < 24 || c.BroadcastSecret == c.Token || c.BroadcastSecret == c.ScannerToken) {
		return errors.New("use a dedicated broadcast secret of at least 24 bytes")
	}
	for name, n := range c.Networks {
		n.RPC = os.ExpandEnv(n.RPC)
		n.Index = os.ExpandEnv(n.Index)
		n.IndexBearer = os.ExpandEnv(n.IndexBearer)
		if !supported(name) || !validURL(n.RPC) || n.Confirmations < 0 || n.Index != "" && !validURL(n.Index) {
			return fmt.Errorf("invalid network configuration: %s", name)
		}
		n.Bearer = os.ExpandEnv(n.Bearer)
		c.Networks[name] = n
	}
	c.Scanner = strings.TrimRight(c.Scanner, "/")
	if c.Listen == "" {
		c.Listen = "127.0.0.1:9082"
	}
	s, e := openService(c)
	if e != nil {
		return e
	}
	defer s.db.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	for network := range c.Networks {
		if _, e := s.head(ctx, network); e != nil {
			return fmt.Errorf("verify RPC for %s: %w", network, e)
		}
	}
	srv := http.Server{Addr: c.Listen, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 25 * time.Second, IdleTimeout: time.Minute}
	workers := s.start(ctx)
	defer func() { cancel(); workers.Wait() }()
	go func() {
		<-ctx.Done()
		stop, end := context.WithTimeout(context.Background(), 15*time.Second)
		defer end()
		if srv.Shutdown(stop) != nil {
			srv.Close()
		}
	}()
	slog.Info("starting BalanceKeeper", "listen", c.Listen)
	if e = srv.ListenAndServe(); errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
func validURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil
}
func main() {
	if err := run(); err != nil {
		slog.Error("BalanceKeeper stopped", "error", err)
		os.Exit(1)
	}
}
