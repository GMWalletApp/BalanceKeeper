package balancekeeper

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

var errRead = errors.New("balance unavailable")

func queue(ctx context.Context, tx *sql.Tx, id int64, t, source, obs string, minHeight int64, event string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO jobs(watch,token,source,observation,min_height,event) VALUES(?,?,?,?,?,NULLIF(?,''))
 ON CONFLICT(watch,token,observation) DO UPDATE SET min_height=max(min_height,excluded.min_height)`, id, t, source, obs, minHeight, event)
	return e
}
func (s *Service) observe(ctx context.Context, id int64, t, source, key string, minHeight int64, event string) (string, error) {
	unlock := s.lock(id, t)
	defer unlock()
	var n, a string
	var start int64
	e := s.db.QueryRowContext(ctx, "SELECT network,address,start FROM watches WHERE id=?", id).Scan(&n, &a, &start)
	if errors.Is(e, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: registration deleted", errRead)
	}
	if e != nil {
		return "", e
	}
	if key != "" {
		var amount string
		e = s.db.QueryRowContext(ctx, "SELECT amount FROM snapshots WHERE watch=? AND token=? AND observation=?", id, t, key).Scan(&amount)
		if e == nil {
			_, e = s.db.ExecContext(ctx, "DELETE FROM jobs WHERE watch=? AND token=? AND observation=?", id, t, key)
			return amount, e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return "", e
		}
	}
	var prior sql.NullInt64
	e = s.db.QueryRowContext(ctx, "SELECT max(height) FROM assets WHERE watch=? AND token=?", id, t).Scan(&prior)
	if e != nil {
		return "", e
	}
	minimum := max(minHeight, start-1, prior.Int64)
	type observation struct {
		reading
		orphan bool
	}
	val, re := readNodes(ctx, s, n, func(ctx context.Context) (observation, error) {
		v, err := s.balance(ctx, n, a, t, minimum)
		result := observation{reading: v}
		if err != nil || !strings.HasPrefix(key, "tx/") {
			return result, err
		}
		var hash string
		switch {
		case n == "bitcoin":
			hash, err = s.bitcoinHashAt(ctx, minHeight)
		case chainIDs[n] != 0:
			var p point
			p, err = s.evmBlock(ctx, n, minHeight)
			hash = p.Block
		default:
			return result, nil
		}
		result.orphan = err == nil && !strings.HasSuffix(key, "/"+strings.ToLower(hash))
		return result, err
	})
	if re == nil && val.orphan {
		_, err := s.db.ExecContext(ctx, "DELETE FROM jobs WHERE watch=? AND token=? AND observation=?", id, t, key)
		return "", err
	}
	observed := nowStamp()
	e = s.atomic(ctx, func(tx *sql.Tx) error {
		var found bool
		if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM watches WHERE id=?)", id).Scan(&found); e != nil {
			return e
		}
		if !found {
			return fmt.Errorf("%w: registration deleted", errRead)
		}
		if _, e := tx.ExecContext(ctx, "INSERT INTO assets(watch,token,name) VALUES(?,?,?) ON CONFLICT DO NOTHING", id, t, t); e != nil {
			return e
		}
		if re != nil {
			if _, err := tx.ExecContext(ctx, "UPDATE assets SET last_error=? WHERE watch=? AND token=?", re.Error(), id, t); err != nil {
				return err
			}
			// A synchronous manual read has no existing job. Persist this failed
			// observation alongside last_error so the worker can finish it.
			if source == "manual" && key == "" {
				_, err := tx.ExecContext(ctx, `INSERT INTO jobs(watch,token,source,observation,min_height,due,attempts)
					VALUES(?,?,'manual','manual/'||lower(hex(randomblob(16))),?,?,1)`, id, t, minimum, backoff(0))
				return err
			}
			return nil
		}
		if val.Name == "" && t == "native" {
			val.Name = nativeSymbols[n]
		}
		if val.Name != "" {
			if _, err := tx.ExecContext(ctx, "UPDATE assets SET name=? WHERE watch=? AND token=? AND name=token", val.Name, id, t); err != nil {
				return err
			}
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO snapshots(watch,token,name,amount,decimals,height,block_time,observed,source,event,observation)
 SELECT watch,token,name,?,?,?,?,?,?,NULLIF(?,''),NULLIF(?,'') FROM assets WHERE watch=? AND token=?`,
			val.Amount, val.Decimals, val.Height, val.Time, observed, source, event, key, id, t)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `UPDATE assets SET amount=?,height=coalesce(?,height),last_error='',derived=CASE WHEN ?='' THEN derived ELSE ? END WHERE watch=? AND token=?`, val.Amount, val.Height, val.Derived, val.Derived, id, t)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "DELETE FROM jobs WHERE watch=? AND token=? AND observation=?", id, t, key)
		return e
	})
	if e != nil {
		return "", e
	}
	if re != nil {
		return "", fmt.Errorf("%w: %v", errRead, re)
	}
	if val.Derived != "" {
		s.signal()
	}
	return val.Amount, nil
}

type watch struct {
	Network string `json:"network"`
	Address string `json:"address"`
}
type registration struct {
	id, start int64
	watch
}

func (s *Service) syncWatches(ctx context.Context) error {
	retired := map[watch]int64{}
	barriers, err := s.db.QueryContext(ctx, "SELECT network,address,generation FROM removals")
	if err != nil {
		return err
	}
	for barriers.Next() {
		var w watch
		var gen int64
		if err = barriers.Scan(&w.Network, &w.Address, &gen); err != nil {
			barriers.Close()
			return err
		}
		retired[w] = gen
	}
	err = barriers.Err()
	barriers.Close()
	if err != nil {
		return err
	}
	rows, e := s.db.QueryContext(ctx, `SELECT id,network,address,upstream_start FROM watches UNION ALL
 SELECT w.id,w.network,a.derived,w.upstream_start FROM assets a JOIN watches w ON w.id=a.watch WHERE a.derived<>'' ORDER BY 2,3,1`)
	if e != nil {
		return e
	}
	var regs []registration
	wanted := []watch{}
	seen := map[watch]bool{}
	for rows.Next() {
		var r registration
		if e = rows.Scan(&r.id, &r.Network, &r.Address, &r.start); e != nil {
			rows.Close()
			return e
		}
		if _, paused := retired[r.watch]; paused {
			continue
		}
		regs = append(regs, r)
		if !seen[r.watch] {
			wanted = append(wanted, r.watch)
			seen[r.watch] = true
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	v, _, e := s.request(ctx, "PUT", s.cfg.Scanner+"/v1/subscriptions", s.cfg.ScannerToken, wanted)
	if e != nil {
		_, dbErr := s.db.ExecContext(ctx, "UPDATE watches SET status='retrying'")
		return errors.Join(e, dbErr)
	}
	bounds := map[watch]int64{}
	for _, r := range v.Get("registrations").Array() {
		h, e := number(r.Get("from_height"), 10)
		if e != nil {
			return e
		}
		bounds[watch{r.Get("network").Str, r.Get("address").Str}] = h
	}
	if len(bounds) != len(wanted) {
		return invalidEvidence
	}
	return s.atomic(ctx, func(tx *sql.Tx) error {
		for _, gen := range retired {
			if _, e := tx.ExecContext(ctx, "DELETE FROM removals WHERE generation=?", gen); e != nil {
				return e
			}
		}
		if len(retired) > 0 {
			s.signal()
		}
		for _, r := range regs {
			h, ok := bounds[r.watch]
			if !ok {
				return invalidEvidence
			}
			if r.start >= 0 && r.start != h {
				if h > r.start {
					_, err := tx.ExecContext(ctx, `INSERT INTO gaps(watch,cursor,end) SELECT id,?,? FROM watches WHERE id=? AND address=?
                    ON CONFLICT(watch) DO UPDATE SET cursor=min(cursor,excluded.cursor),end=max(end,excluded.end),due=0`, r.start, h-1, r.id, r.Address)
					if err != nil {
						return err
					}
				}
				_, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO jobs(watch,token,source,observation,min_height)
 SELECT watch,token,'reconcile',?,? FROM assets WHERE watch=? AND EXISTS(SELECT 1 FROM watches WHERE id=? AND address=?)`, fmt.Sprintf("reconcile-%d", h), h-1, r.id, r.id, r.Address)
				if e != nil {
					return e
				}
			}
			_, e := tx.ExecContext(ctx, "UPDATE watches SET status='active', start=CASE WHEN start<0 THEN ? ELSE start END,upstream_start=? WHERE id=? AND address=?", h, h, r.id, r.Address)
			if e != nil {
				return e
			}
		}
		return nil
	})
}
func (s *Service) start(ctx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			e := s.syncWatches(ctx)
			delay := 30 * time.Second
			if e != nil {
				delay = 3 * time.Second
				if ctx.Err() == nil {
					slog.Warn("subscriptions", "error", e)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-time.After(delay):
			}
		}
	})
	for network := range s.cfg.Networks {
		wg.Go(func() {
			for ctx.Err() == nil {
				if e := s.poll(ctx, network); e != nil {
					if ctx.Err() == nil {
						slog.Warn("scanner feed", "network", network, "error", e)
					}
					wait(ctx, 2*time.Second)
				}
			}
		})
	}
	wg.Go(func() { s.loop(ctx, s.nextInbox) })
	wg.Go(func() { s.loop(ctx, s.nextGap) })
	for range 4 {
		wg.Go(func() { s.loop(ctx, s.nextProbe) })
	}
	for n := range s.cfg.Networks {
		if s.ownerInventoryEnabled(n) {
			wg.Go(func() { s.loop(ctx, func(ctx context.Context) (bool, error) { return s.nextInventory(ctx, n) }) })
		}
	}
	if _, enabled := s.cfg.Networks["ton"]; enabled {
		wg.Go(func() { s.loop(ctx, s.nextJettonOwner) })
	}
	for range 4 {
		wg.Go(func() { s.loop(ctx, s.nextJob) })
	}
	return &wg
}
func (s *Service) loop(ctx context.Context, next func(context.Context) (bool, error)) {
	for ctx.Err() == nil {
		work, e := next(ctx)
		if e != nil && ctx.Err() == nil {
			slog.Warn("background work deferred", "error", e)
		}
		if !work || e != nil {
			wait(ctx, 300*time.Millisecond)
		}
	}
}
func (s *Service) poll(ctx context.Context, n string) error {
	v, status, e := s.request(ctx, "GET", s.cfg.Scanner+"/v1/events/next?network="+url.QueryEscape(n)+"&wait=15", s.cfg.ScannerToken, nil)
	if e != nil || status == 204 {
		return e
	}
	id, e := number(v.Get("id"), 10)
	if e != nil || id <= 0 || v.Get("activity.network").Str != n || !v.Get("activity.addresses").IsArray() || v.Get("activity.tx_id").Str == "" {
		return invalidEvidence
	}
	body := v.Raw
	block := v.Get("activity.data.transaction.blockHash").Str
	if n == "bitcoin" {
		block = v.Get("activity.data.block_hash").Str
	}
	identity := fmt.Sprintf("%s/%d/%s/%s/%s", n, id, v.Get("activity.height").Raw, v.Get("activity.tx_id").Str, block)
	e = s.atomic(ctx, func(tx *sql.Tx) error { return inboxPut(ctx, tx, "chainscanner", identity, n, body) })
	if e != nil {
		return e
	}
	_, _, e = s.request(ctx, "POST", fmt.Sprintf("%s/v1/events/%d/ack", s.cfg.Scanner, id), s.cfg.ScannerToken, nil)
	return e
}
func inboxPut(ctx context.Context, tx *sql.Tx, source, key, n, body string) error {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	var existing string
	e := tx.QueryRowContext(ctx, "SELECT digest FROM inbox WHERE source=? AND identity=?", source, key).Scan(&existing)
	if e == nil {
		if existing != digest {
			return bad("conflicting event ID")
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO inbox(source,identity,digest,network,payload,ceiling)
 SELECT ?,?,?,?,?,coalesce(max(id),0) FROM watches WHERE network=?`, source, key, digest, n, body, n)
	return e
}
func backoff(attempt int) int64 {
	return time.Now().Add(time.Second * time.Duration(1<<min(attempt, 9))).UnixMilli()
}
func (s *Service) nextJob(ctx context.Context) (bool, error) {
	var jid, id, minH int64
	var t, source, key, event string
	var tries int
	e := s.db.QueryRowContext(ctx, `UPDATE jobs SET due=? WHERE id=(SELECT j.id FROM jobs j JOIN watches w ON w.id=j.watch
 WHERE j.due<=? AND w.start>=0 ORDER BY j.due,j.id LIMIT 1)
 RETURNING id,watch,token,source,observation,min_height,coalesce(event,''),attempts`, time.Now().Add(3*time.Minute).UnixMilli(), time.Now().UnixMilli()).Scan(&jid, &id, &t, &source, &key, &minH, &event, &tries)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	_, e = s.observe(ctx, id, t, source, key, minH, event)
	if e != nil {
		_, de := s.db.ExecContext(ctx, "UPDATE jobs SET attempts=attempts+1,due=? WHERE id=?", backoff(tries), jid)
		return true, errors.Join(e, de)
	}
	return true, nil
}
func (s *Service) nextInbox(ctx context.Context) (bool, error) {
	var id, ceiling int64
	var source, n, body string
	var attempts int
	e := s.db.QueryRowContext(ctx, "SELECT id,source,network,payload,ceiling,attempts FROM inbox WHERE done=0 AND due<=? ORDER BY due,id LIMIT 1", time.Now().UnixMilli()).Scan(&id, &source, &n, &body, &ceiling, &attempts)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	e = s.routeEvent(ctx, id, ceiling, source, n, gjson.Parse(body))
	if e != nil {
		_, de := s.db.ExecContext(ctx, "UPDATE inbox SET due=?,attempts=attempts+1 WHERE id=?", backoff(attempts), id)
		return true, errors.Join(e, de)
	}
	return true, nil
}
func finishInbox(ctx context.Context, tx *sql.Tx, id int64) error {
	_, e := tx.ExecContext(ctx, "UPDATE inbox SET done=1,payload='' WHERE id=?", id)
	return e
}
func (s *Service) knownTokens(ctx context.Context, id int64) ([]string, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT token FROM assets WHERE watch=?", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if e = rows.Scan(&k); e != nil {
			return nil, e
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
