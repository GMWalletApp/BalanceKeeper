package balancekeeper

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type fault struct {
	code    string
	message string
	status  int
	index   *int
}

func (f *fault) Error() string { return f.message }
func bad(msg string) *fault    { return &fault{"invalid_request", msg, 400, nil} }
func fields(o gjson.Result, allowed string) error {
	if !o.IsObject() {
		return bad("expected object")
	}
	seen := map[string]bool{}
	var err error
	o.ForEach(func(k, v gjson.Result) bool {
		if seen[k.Str] || !strings.Contains(" "+allowed+" ", " "+k.Str+" ") || strings.Contains(k.Str, " ") || k.Str == "" {
			err = bad("unexpected or duplicate field: " + k.Str)
			return false
		}
		seen[k.Str] = true
		return true
	})
	return err
}

type target struct {
	network, address string
	assets           map[string]string
	tokens           []string
	limit            int
	before, from, to *int64
}

var routes = map[string]string{"/v1/watches": "assets", "/v1/watches/status": "", "/v1/balances/query": "tokens", "/v1/refreshes": "tokens", "/v1/snapshots/query": "tokens limit before from to", "/v1/watches/delete": ""}

func (s *Service) parseTarget(o gjson.Result, path string) (target, error) {
	t := target{limit: 50}
	if e := fields(o, "network address "+routes[path]); e != nil {
		return t, e
	}
	t.network = o.Get("network").Str
	if t.network == "" {
		return t, bad("network required")
	}
	if _, ok := s.cfg.Networks[t.network]; !ok {
		return t, &fault{"unsupported_network", "network not enabled", 422, nil}
	}
	a, e := canonical(t.network, o.Get("address").Str)
	if e != nil {
		return t, bad("invalid address")
	}
	t.address = a
	if path == "/v1/watches" {
		v := o.Get("assets")
		if !v.IsArray() {
			return t, bad("assets array required")
		}
		t.assets = map[string]string{}
		for _, asset := range v.Array() {
			if e := fields(asset, "name contract_address"); e != nil {
				return t, e
			}
			n := strings.TrimSpace(asset.Get("name").Str)
			c := asset.Get("contract_address")
			if n == "" || !c.Exists() {
				return t, bad("asset name and contract_address required")
			}
			key := "native"
			if c.Type != gjson.Null {
				key, e = tokenID(t.network, c.Str)
				if e != nil || key == "native" {
					return t, bad("invalid contract identifier")
				}
			}
			if _, ok := t.assets[key]; ok {
				return t, bad("duplicate asset")
			}
			t.assets[key] = n
		}
	}
	if ts := o.Get("tokens"); ts.Exists() {
		if !ts.IsArray() || len(ts.Array()) == 0 {
			return t, bad("nonempty tokens required")
		}
		seen := map[string]bool{}
		for _, v := range ts.Array() {
			key, e := tokenID(t.network, v.Str)
			if e != nil || seen[key] {
				return t, bad("invalid or duplicate token")
			}
			seen[key] = true
			t.tokens = append(t.tokens, key)
		}
	}
	if v := o.Get("limit"); v.Exists() {
		num, e := number(v, 10)
		if e != nil || v.Type != gjson.Number || num < 1 || num > 200 {
			return t, bad("limit must be 1..200")
		}
		t.limit = int(num)
	}
	if v := o.Get("before"); v.Exists() {
		i, e := strconv.ParseInt(v.Str, 10, 64)
		if e != nil || i <= 0 {
			return t, bad("invalid cursor")
		}
		t.before = &i
	}
	for _, k := range []string{"from", "to"} {
		if v := o.Get(k); v.Exists() {
			dt, e := time.Parse(time.RFC3339Nano, v.Str)
			_, offset := dt.Zone()
			ns := dt.UnixNano()
			if e != nil || offset != 0 || !time.Unix(0, ns).Equal(dt) {
				return t, bad("invalid UTC timestamp")
			}
			if k == "from" {
				t.from = &ns
			} else {
				t.to = &ns
			}
		}
	}
	if t.from != nil && t.to != nil && *t.from >= *t.to {
		return t, bad("from must precede to")
	}
	return t, nil
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}
func failure(w http.ResponseWriter, e error) {
	var f *fault
	if !errors.As(e, &f) {
		f = &fault{"storage_unavailable", "operation could not be committed", 503, nil}
	}
	obj := M{"code": f.code, "message": f.message}
	if f.index != nil {
		obj["index"] = *f.index
	}
	write(w, f.status, M{"error": obj})
}
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	for path := range routes {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.cfg.Token)) != 1 {
				failure(w, &fault{"unauthorized", "invalid token", 401, nil})
				return
			}
			batch, e := readBatch(w, r)
			if e != nil {
				failure(w, e)
				return
			}
			ts := make([]target, 0, len(batch))
			seen := map[[2]string]bool{}
			for i, o := range batch {
				t, e := s.parseTarget(o, path)
				key := [2]string{t.network, t.address}
				if e == nil && path == "/v1/watches" && seen[key] {
					e = bad("duplicate watch")
				}
				if e != nil {
					var f *fault
					if errors.As(e, &f) {
						f.index = ptr(i)
					}
					failure(w, e)
					return
				}
				seen[key] = true
				ts = append(ts, t)
			}
			status, res, e := s.execute(r.Context(), path, ts)
			if e != nil {
				failure(w, e)
				return
			}
			write(w, status, M{"results": res})
		})
	}
	mux.HandleFunc("POST /v1/notifications/broadcasts", s.receiveBroadcasts)
	return mux
}
func rawBatch(raw []byte) ([]gjson.Result, error) {
	if !gjson.ValidBytes(raw) {
		return nil, bad("invalid JSON")
	}
	r := gjson.ParseBytes(raw)
	if !r.IsArray() || len(r.Array()) == 0 {
		return nil, bad("expected nonempty array")
	}
	return r.Array(), nil
}
func readBatch(w http.ResponseWriter, r *http.Request) ([]gjson.Result, error) {
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if e != nil {
		return nil, bad("invalid or oversized body")
	}
	return rawBatch(b)
}
func response(t target) M { return M{"network": t.network, "address": t.address} }
func (s *Service) execute(ctx context.Context, path string, ts []target) (int, []M, error) {
	if path == "/v1/watches" || path == "/v1/watches/delete" {
		return s.mutate(ctx, path, ts)
	}
	if path == "/v1/refreshes" {
		return s.refreshBatch(ctx, ts)
	}
	out := make([]M, 0, len(ts))
	for _, t := range ts {
		r := response(t)
		var id int64
		var initial, status string
		e := s.db.QueryRowContext(ctx, "SELECT id,initial,status FROM watches WHERE network=? AND address=?", t.network, t.address).Scan(&id, &initial, &status)
		if errors.Is(e, sql.ErrNoRows) {
			r["error"] = M{"code": "watch_not_found", "message": "address not registered"}
		} else if e != nil {
			return 0, nil, e
		} else {
			switch path {
			case "/v1/watches/status":
				r["subscription"] = status
				r["initial_asset_count"] = len(gjson.Parse(initial).Map())
				r["pending_assets"], e = s.pending(ctx, id)
				if e == nil {
					var cursor, end int64
					ge := s.db.QueryRowContext(ctx, "SELECT cursor,end FROM gaps WHERE watch=?", id).Scan(&cursor, &end)
					if ge == nil {
						r["discovery_gap"] = M{"from_height": cursor, "to_height": end}
					} else if !errors.Is(ge, sql.ErrNoRows) {
						e = ge
					}
				}
			case "/v1/snapshots/query":
				e = s.history(ctx, id, t, r)
			default:
				e = s.current(ctx, id, t, r)
			}
			if e != nil {
				return 0, nil, e
			}
		}
		out = append(out, r)
	}
	return 200, out, nil
}
func (s *Service) mutate(ctx context.Context, path string, ts []target) (int, []M, error) {
	status := 200
	res := make([]M, 0, len(ts))
	e := s.atomic(ctx, func(tx *sql.Tx) error {
		for i, t := range ts {
			r := response(t)
			if path == "/v1/watches/delete" {
				// A quick delete/register pair must still make an upstream removal visible.
				_, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO removals(network,address)
                    SELECT network,address FROM watches WHERE network=? AND address=?`, t.network, t.address)
				if e != nil {
					return e
				}
				x, e := tx.ExecContext(ctx, "DELETE FROM watches WHERE network=? AND address=?", t.network, t.address)
				if e != nil {
					return e
				}
				n, _ := x.RowsAffected()
				r["status"] = "already_absent"
				if n != 0 {
					r["status"] = "deleted"
				}
			} else {
				initial := compact(t.assets)
				x, e := tx.ExecContext(ctx, "INSERT INTO watches(network,address,initial) VALUES(?,?,?) ON CONFLICT(network,address) DO NOTHING", t.network, t.address, initial)
				if e != nil {
					return e
				}
				n, _ := x.RowsAffected()
				var id int64
				var original, state string
				if e = tx.QueryRowContext(ctx, "SELECT id,initial,status FROM watches WHERE network=? AND address=?", t.network, t.address).Scan(&id, &original, &state); e != nil {
					return e
				}
				if original != initial {
					return &fault{"watch_already_initialized", "different initialization", 409, ptr(i)}
				}
				if n == 1 {
					status = 202
					for k, name := range t.assets {
						if _, e = tx.ExecContext(ctx, "INSERT INTO assets(watch,token,name) VALUES(?,?,?)", id, k, name); e != nil {
							return e
						}
						if e = queue(ctx, tx, id, k, "initial", "initial", 0, ""); e != nil {
							return e
						}
					}
				}
				r["initial_asset_count"] = len(t.assets)
				r["subscription"] = state
			}
			res = append(res, r)
		}
		return nil
	})
	if e == nil {
		s.signal()
	}
	return status, res, e
}
func tokenField(t string) any {
	if t == "native" {
		return nil
	}
	return t
}
func (s *Service) pending(ctx context.Context, id int64) ([]M, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT token,name,last_error FROM assets WHERE watch=? AND amount IS NULL ORDER BY token", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []M{}
	for rows.Next() {
		var t, n, er string
		if e := rows.Scan(&t, &n, &er); e != nil {
			return nil, e
		}
		r := M{"name": n, "contract_address": tokenField(t), "last_error": nil}
		if er != "" {
			r["last_error"] = M{"message": er, "code": "rpc_failed"}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type assetKey struct {
	id    int64
	token string
}
type outcome struct {
	amount string
	err    error
}

// Freeze every item's scope before the first RPC. A token discovered by an
// earlier item in this batch belongs to the next whole-address refresh.
func (s *Service) refreshBatch(ctx context.Context, ts []target) (int, []M, error) {
	ids := make([]int64, len(ts))
	err := s.atomic(ctx, func(tx *sql.Tx) error {
		for i, t := range ts {
			err := tx.QueryRowContext(ctx, "SELECT id FROM watches WHERE network=? AND address=?", t.network, t.address).Scan(&ids[i])
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if t.tokens != nil {
				continue
			}
			rows, err := tx.QueryContext(ctx, "SELECT token FROM assets WHERE watch=? AND (amount IS NULL OR amount<>'0') ORDER BY token", ids[i])
			if err != nil {
				return err
			}
			for rows.Next() {
				var token string
				if err = rows.Scan(&token); err != nil {
					rows.Close()
					return err
				}
				ts[i].tokens = append(ts[i].tokens, token)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	out := make([]M, len(ts))
	cache := map[assetKey]outcome{}
	for i, t := range ts {
		r := response(t)
		out[i] = r
		if ids[i] == 0 {
			r["error"] = M{"code": "watch_not_found", "message": "address not registered"}
			continue
		}
		balances := map[string]string{}
		failures := M{}
		for _, token := range t.tokens {
			k := assetKey{ids[i], token}
			result, ok := cache[k]
			if !ok {
				result.amount, result.err = s.observe(ctx, k.id, k.token, "manual", "", 0, "")
				cache[k] = result
			}
			if result.err == nil {
				balances[token] = result.amount
				continue
			}
			if !errors.Is(result.err, errRead) {
				return 0, nil, result.err
			}
			failures[token] = M{"code": "rpc_failed", "message": result.err.Error()}
		}
		r["balances"], r["status"] = balances, "succeeded"
		if len(failures) > 0 {
			r["errors"], r["status"] = failures, "partial"
			if len(balances) == 0 {
				r["status"] = "failed"
			}
		}
	}
	return 200, out, nil
}

func (s *Service) current(ctx context.Context, id int64, t target, r M) error {
	vals := map[string]*string{}
	r["balances"] = vals
	q := "SELECT token,amount FROM assets WHERE watch=?"
	args := []any{id}
	if len(t.tokens) == 0 {
		q += " AND amount<>'0'"
	} else {
		q += " AND token IN (" + strings.TrimSuffix(strings.Repeat("?,", len(t.tokens)), ",") + ")"
		for _, k := range t.tokens {
			args = append(args, k)
		}
	}
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var amount *string
		if e = rows.Scan(&k, &amount); e != nil {
			return e
		}
		vals[k] = amount
	}
	if e = rows.Err(); e != nil {
		return e
	}
	errs := M{}
	for _, k := range t.tokens {
		if _, ok := vals[k]; !ok {
			errs[k] = M{"code": "token_not_found", "message": "no observation"}
		}
	}
	if len(errs) > 0 {
		r["errors"] = errs
	}
	return nil
}
func (s *Service) history(ctx context.Context, id int64, t target, r M) error {
	q := "SELECT id,token,name,amount,decimals,height,block_time,observed,source,event FROM snapshots WHERE watch=?"
	args := []any{id}
	if len(t.tokens) > 0 {
		q += " AND token IN (" + strings.TrimSuffix(strings.Repeat("?,", len(t.tokens)), ",") + ")"
		for _, k := range t.tokens {
			args = append(args, k)
		}
	}
	for _, x := range []struct {
		col string
		v   *int64
	}{{"observed>=", t.from}, {"observed<", t.to}, {"id<", t.before}} {
		if x.v != nil {
			q += " AND " + x.col + "?"
			args = append(args, *x.v)
		}
	}
	rows, e := s.db.QueryContext(ctx, q+" ORDER BY id DESC LIMIT ?", append(args, t.limit+1)...)
	if e != nil {
		return e
	}
	defer rows.Close()
	list := []M{}
	var last string
	r["next_before"] = nil
	for rows.Next() {
		if len(list) == t.limit {
			r["next_before"] = last
			break
		}
		var serial, observed int64
		var k, name, amount, source string
		var dec, height *int64
		var bt, event *string
		if e = rows.Scan(&serial, &k, &name, &amount, &dec, &height, &bt, &observed, &source, &event); e != nil {
			return e
		}
		last = strconv.FormatInt(serial, 10)
		snap := M{"id": last, "observed_at": iso(time.Unix(0, observed)), "block_time": bt, "amount_raw": amount, "decimals": dec, "height": nil, "source": source, "event": nil}
		if height != nil {
			snap["height"] = fmt.Sprint(*height)
		}
		if event != nil {
			snap["event"] = json.RawMessage(*event)
		}
		list = append(list, M{"asset": M{"name": name, "contract_address": tokenField(k)}, "snapshot": snap})
	}
	r["items"] = list
	return rows.Err()
}
