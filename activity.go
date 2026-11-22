package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

type candidates map[string]map[string]bool

const transferTopic = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

func (c candidates) add(n, a, t string) {
	a, e := canonical(n, a)
	if e != nil {
		return
	}
	t, e = tokenID(n, t)
	if e != nil {
		return
	}
	if c[a] == nil {
		c[a] = map[string]bool{}
	}
	c[a][t] = true
}
func (c candidates) touch(n, a string) {
	a, e := canonical(n, a)
	if e == nil && c[a] == nil {
		c[a] = map[string]bool{}
	}
}
func topicAddr(v gjson.Result) string {
	h := strings.TrimPrefix(v.Str, "0x")
	if len(h) != 64 || !strings.HasPrefix(h, strings.Repeat("0", 24)) {
		return ""
	}
	return "0x" + h[24:]
}
func addLogs(c candidates, n string, logs gjson.Result) {
	for _, l := range logs.Array() {
		topics := l.Get("topics").Array()
		if len(topics) != 3 || strings.TrimPrefix(strings.ToLower(topics[0].Str), "0x") != transferTopic || len(strings.TrimPrefix(l.Get("data").Str, "0x")) != 64 {
			continue
		}
		for _, v := range topics[1:] {
			c.add(n, topicAddr(v), l.Get("address").Str)
		}
	}
}
func parseSolana(n string, v gjson.Result) candidates {
	c := candidates{}
	keys := v.Get("transaction.message.accountKeys").Array()
	for _, key := range []string{"writable", "readonly"} {
		keys = append(keys, v.Get("meta.loadedAddresses."+key).Array()...)
	}
	pre, post := v.Get("meta.preBalances").Array(), v.Get("meta.postBalances").Array()
	for i, k := range keys {
		a := k.Str
		if k.IsObject() {
			a = k.Get("pubkey").Str
		}
		c.touch(n, a)
		if i >= len(pre) || i >= len(post) || pre[i].Raw != post[i].Raw {
			c.add(n, a, "native")
		}
	}
	type holding struct{ a, t, raw string }
	before := map[int64]holding{}
	for _, b := range v.Get("meta.preTokenBalances").Array() {
		before[b.Get("accountIndex").Int()] = holding{b.Get("owner").Str, b.Get("mint").Str, b.Get("uiTokenAmount.amount").Str}
	}
	for _, b := range v.Get("meta.postTokenBalances").Array() {
		i := b.Get("accountIndex").Int()
		old := before[i]
		cur := holding{b.Get("owner").Str, b.Get("mint").Str, b.Get("uiTokenAmount.amount").Str}
		if old != cur {
			c.add(n, old.a, old.t)
			c.add(n, cur.a, cur.t)
		}
		delete(before, i)
	}
	for _, old := range before {
		c.add(n, old.a, old.t)
	}
	return c
}
func parseOther(n string, v gjson.Result) candidates {
	c := candidates{}
	switch n {
	case "tron":
		for _, r := range v.Get("transaction.raw_data.contract").Array() {
			p := r.Get("parameter.value")
			c.add(n, p.Get("owner_address").Str, "native")
			c.add(n, p.Get("to_address").Str, "native")
		}
		addLogs(c, n, v.Get("receipt.log"))
		for _, r := range v.Get("receipt.internal_transactions").Array() {
			c.add(n, r.Get("caller_address").Str, "native")
			c.add(n, r.Get("transferTo_address").Str, "native")
		}
	case "sui":
		for _, field := range []string{"balance_changes", "balanceChanges"} {
			for _, b := range v.Get(field).Array() {
				t := b.Get("coin_type").Str
				if t == "" {
					t = b.Get("coinType").Str
				}
				c.add(n, b.Get("address").Str, t)
			}
		}
		c.add(n, v.Get("transaction.gas_payment.owner").Str, "native")
		c.add(n, v.Get("transaction.gasPayment.owner").Str, "native")
		c.touch(n, v.Get("transaction.sender").Str)
		for _, o := range v.Get("effects.changed_objects").Array() {
			c.touch(n, o.Get("input_owner.address").Str)
			c.touch(n, o.Get("output_owner.address").Str)
		}
	case "aptos":
		c.add(n, v.Get("sender").Str, "native")
		c.add(n, v.Get("signature.fee_payer_address").Str, "native")
		owners := map[string]string{}
		for _, r := range v.Get("changes").Array() {
			if strings.HasSuffix(r.Get("data.type").Str, "::object::ObjectCore") {
				owners[r.Get("address").Str] = r.Get("data.data.owner").Str
			}
		}
		for _, r := range v.Get("changes").Array() {
			typ := r.Get("data.type").Str
			if r.Get("type").Str == "delete_resource" {
				typ = r.Get("resource").Str
			}
			a := r.Get("address").Str
			if strings.HasPrefix(typ, "0x1::coin::CoinStore<") && strings.HasSuffix(typ, ">") {
				c.add(n, a, typ[len("0x1::coin::CoinStore<"):len(typ)-1])
			}
			if strings.HasSuffix(typ, "::fungible_asset::FungibleStore") {
				c.add(n, owners[a], r.Get("data.data.metadata.inner").Str)
			}
		}
		for _, ev := range v.Get("events").Array() {
			typ := ev.Get("type").Str
			if i := strings.Index(typ, "<"); i >= 0 && strings.HasSuffix(typ, ">") {
				for _, k := range []string{"account", "sender", "receiver"} {
					c.add(n, ev.Get("data."+k).Str, typ[i+1:len(typ)-1])
				}
			}
		}
	case "ton":
		c.add(n, v.Get("account").Str, "native")
		c.add(n, v.Get("Account").Str, "native")
		for _, m := range append(v.Get("out_msgs").Array(), v.Get("in_msg")) {
			c.add(n, m.Get("source").Str, "native")
			c.add(n, m.Get("destination").Str, "native")
		}
	}
	return c
}

type evidence struct {
	hash       string
	height     int64
	block      string
	data       gjson.Result
	trace      gjson.Result
	traceRetry error
}

func (s *Service) tonIndex(ctx context.Context, path string) (gjson.Result, error) {
	c := s.cfg.Networks["ton"]
	bearer := c.IndexBearer
	base := c.Index
	if base == "" {
		base = c.RPC
		bearer = c.Bearer
	}
	v, _, e := s.request(ctx, "GET", strings.TrimRight(base, "/")+"/api/v3/"+path, bearer, nil)
	return v, e
}
func (s *Service) lookup(ctx context.Context, n, hash string) (gjson.Result, error) {
	v, _, e := s.request(ctx, "GET", s.cfg.Scanner+"/v1/transactions/"+url.PathEscape(n)+"/"+url.PathEscape(hash), s.cfg.ScannerToken, nil)
	if e != nil {
		return v, e
	}
	if v.Get("network").Str != n || v.Get("hash").Str != hash || !v.Get("result").IsObject() {
		return v, invalidEvidence
	}
	return v.Get("result"), nil
}

// Read the chain's own transaction format by hash. ChainScanner's separate
// transaction lookup is a fallback for an unavailable local history endpoint.
func (s *Service) fullTransaction(ctx context.Context, n, hash string) (gjson.Result, error) {
	var v gjson.Result
	var err error
	switch n {
	case "solana":
		v, err = s.rpc(ctx, n, "getTransaction", hash, M{"commitment": "finalized", "encoding": "json", "maxSupportedTransactionVersion": 1})
	case "aptos":
		v, err = s.rest(ctx, n, "/v1/transactions/by_hash/"+url.PathEscape(hash), nil)
	case "sui":
		v, err = s.suiTransaction(ctx, hash)
	case "tron":
		tx, te := s.rest(ctx, n, "/walletsolidity/gettransactionbyid", M{"value": hash})
		receipt, re := s.rest(ctx, n, "/walletsolidity/gettransactioninfobyid", M{"value": hash})
		if te == nil && re == nil && tx.IsObject() && receipt.IsObject() && receipt.Get("id").Exists() {
			v = gjson.Parse(compact(M{"transaction": json.RawMessage(tx.Raw), "receipt": json.RawMessage(receipt.Raw)}))
		}
	}
	var includesEffects bool
	switch n {
	case "solana":
		includesEffects = v.Get("transaction.signatures.0").Str == hash && v.Get("slot").Exists() && v.Get("meta.preBalances").IsArray() && v.Get("meta.postBalances").IsArray()
	case "aptos":
		includesEffects = strings.EqualFold(v.Get("hash").Str, hash) && v.Get("changes").IsArray()
	case "sui":
		includesEffects = v.Get("digest").Str == hash && v.Get("checkpoint").Exists()
	case "tron":
		includesEffects = strings.EqualFold(v.Get("receipt.id").Str, hash) && strings.EqualFold(v.Get("transaction.txID").Str, hash)
	}
	if err != nil || !includesEffects {
		return s.lookup(ctx, n, hash)
	}
	return v, nil
}
func (s *Service) evidence(ctx context.Context, n, source string, p gjson.Result) (evidence, error) {
	var ev evidence
	var e error
	family := n
	if chainIDs[n] > 0 {
		family = "evm"
	}
	scanner := source == "chainscanner"
	if scanner {
		a := p.Get("activity")
		ev.hash, e = normalizeHash(family, a.Get("tx_id").Str)
		if e != nil {
			return ev, e
		}
		ev.height, e = number(a.Get("height"), 10)
		if e != nil {
			return ev, e
		}
		ev.data = a.Get("data")
	} else {
		ev.hash, e = normalizeHash(family, p.Get("hash").Str)
		if e != nil {
			return ev, e
		}
		if n == "ton" {
			v, err := s.tonIndex(ctx, "transactionsByMessage?direction=in&limit=100&msg_hash="+url.QueryEscape(ev.hash))
			if err != nil {
				return ev, err
			}
			var matched bool
			for _, row := range v.Get("transactions").Array() {
				msg, err := normalizeHash("ton", row.Get("in_msg.hash_norm").Str)
				if err != nil || msg != ev.hash {
					continue
				}
				tx, err := normalizeHash("ton", row.Get("hash").Str)
				if err != nil {
					continue
				}
				if p.Get("txHash").Exists() {
					expected, _ := normalizeHash("ton", p.Get("txHash").Str)
					if expected != tx {
						continue
					}
				}
				ev.hash = tx
				ev.data = row
				matched = true
				break
			}
			if !matched {
				return ev, invalidEvidence
			}
		}
	}
	if n == "bitcoin" {
		return s.bitcoinEvidence(ctx, ev, scanner)
	}
	if family == "evm" {
		return s.evmEvidence(ctx, n, ev, scanner)
	}
	// Fetch full per-transaction data. Scanner deliveries may contain a
	// projection with well-formed but incomplete arrays of changes.
	if n == "ton" && scanner {
		rows, err := s.tonIndex(ctx, "transactions?limit=1&hash="+url.QueryEscape(ev.hash))
		if err != nil {
			return ev, err
		}
		row := rows.Get("transactions.0")
		hash, err := normalizeHash("ton", row.Get("hash").Str)
		if err != nil || hash != ev.hash || row.Get("account").Str == "" || row.Get("mc_block_seqno").Int() != ev.height {
			return ev, invalidEvidence
		}
		ev.data = row
	} else if n != "ton" {
		ev.data, e = s.fullTransaction(ctx, n, ev.hash)
		if e != nil {
			return ev, e
		}
	}
	v := ev.data
	deliveredHeight := ev.height
	if n != "ton" || !scanner {
		switch n {
		case "solana":
			ev.height, e = number(v.Get("slot"), 10)
			if v.Get("transaction.signatures.0").Str != ev.hash || !v.Get("meta.preBalances").IsArray() || !v.Get("meta.postBalances").IsArray() || !v.Get("transaction.message.accountKeys").IsArray() {
				return ev, invalidEvidence
			}
		case "sui":
			if v.Get("digest").Str != ev.hash {
				return ev, invalidEvidence
			}
			ev.height, e = number(v.Get("checkpoint"), 10)
		case "aptos":
			ev.height, e = number(v.Get("version"), 10)
			if !strings.EqualFold(v.Get("hash").Str, ev.hash) || v.Get("type").Str == "pending_transaction" || !v.Get("changes").IsArray() {
				return ev, invalidEvidence
			}
		case "tron":
			if !strings.EqualFold(v.Get("transaction.txID").Str, ev.hash) || !strings.EqualFold(v.Get("receipt.id").Str, ev.hash) {
				return ev, invalidEvidence
			}
			ev.height, e = number(v.Get("receipt.blockNumber"), 10)
		case "ton":
			ev.height, e = number(v.Get("mc_block_seqno"), 10)
		}
		if e != nil {
			return ev, e
		}
	}
	if scanner && ev.height != deliveredHeight {
		return ev, invalidEvidence
	}
	if ev.block == "" {
		ev.block = strconv.FormatInt(ev.height, 10)
	}
	return ev, nil
}

// Some Aptos writes update a primary store without rewriting its ObjectCore.
func (s *Service) aptosOwners(ctx context.Context, ev evidence, c candidates) error {
	// Concurrent-balance updates can emit Deposit/Withdraw with only the store
	// address. Resolve the store and its owner at the event's ledger version.
	stores := map[string]string{}
	owners := map[string]string{}
	deleted := map[string]bool{}
	for _, r := range ev.data.Get("changes").Array() {
		store, e := canonical("aptos", r.Get("address").Str)
		if e != nil {
			continue
		}
		if r.Get("type").Str == "delete_resource" && r.Get("resource").Str == "0x1::fungible_asset::FungibleStore" {
			stores[store] = ""
			deleted[store] = true
		}
		switch r.Get("data.type").Str {
		case "0x1::fungible_asset::FungibleStore":
			stores[store] = r.Get("data.data.metadata.inner").Str
		case "0x1::object::ObjectCore":
			owners[store] = r.Get("data.data.owner").Str
		}
	}
	for _, r := range ev.data.Get("events").Array() {
		switch r.Get("type").Str {
		case "0x1::fungible_asset::Deposit", "0x1::fungible_asset::Withdraw":
			store, e := canonical("aptos", r.Get("data.store").Str)
			if e != nil {
				return invalidEvidence
			}
			if _, ok := stores[store]; !ok {
				stores[store] = ""
			}
		}
	}
	resource := func(store, typ string) (gjson.Result, error) {
		height := ev.height
		if deleted[store] && height > 0 {
			height--
		}
		return s.rest(ctx, "aptos", "/v1/accounts/"+store+"/resource/0x1::"+typ+"?ledger_version="+strconv.FormatInt(height, 10), nil)
	}
	for store, token := range stores {
		if token == "" {
			v, e := resource(store, "fungible_asset::FungibleStore")
			if e != nil {
				return e
			}
			token = v.Get("data.metadata.inner").Str
		}
		owner := owners[store]
		if owner == "" {
			v, e := resource(store, "object::ObjectCore")
			if e != nil {
				return e
			}
			owner = v.Get("data.owner").Str
		}
		if _, e := canonical("aptos", owner); e != nil {
			return e
		}
		if _, e := tokenID("aptos", token); e != nil {
			return e
		}
		c.add("aptos", owner, token)
	}
	return nil
}
func (s *Service) jettons(ctx context.Context, a string) ([]string, error) {
	out := []string{}
	for offset := 0; ; offset += 100 {
		v, e := s.tonIndex(ctx, fmt.Sprintf("jetton/wallets?owner_address=%s&limit=100&offset=%d", url.QueryEscape(a), offset))
		if e != nil {
			return nil, e
		}
		rows := v.Get("jetton_wallets")
		if !rows.IsArray() {
			return nil, invalidEvidence
		}
		for _, r := range rows.Array() {
			owner, e := canonical("ton", r.Get("owner").Str)
			if e != nil || owner != a {
				return nil, invalidEvidence
			}
			master, e := canonical("ton", r.Get("jetton").Str)
			if e != nil {
				return nil, e
			}
			out = append(out, master)
		}
		if len(rows.Array()) < 100 {
			return out, nil
		}
	}
}
func (s *Service) routeEvent(ctx context.Context, id, ceiling int64, source, n string, p gjson.Result) error {
	var count, pending int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(start<0),0) FROM watches WHERE network=? AND id<=?", n, ceiling).Scan(&count, &pending); err != nil {
		return err
	}
	if count == 0 {
		return s.atomic(ctx, func(tx *sql.Tx) error { return finishInbox(ctx, tx, id) })
	}
	if pending != 0 {
		return errors.New("awaiting scanner boundary")
	}
	ev, e := s.evidence(ctx, n, source, p)
	if e != nil {
		return e
	}
	contracts := map[string]bool{}
	var c candidates
	switch {
	case chainIDs[n] != 0:
		c = parseEVM(n, ev, contracts)
	case n == "bitcoin":
		changes, err := bitcoinChanges(ev.data)
		if err != nil {
			return err
		}
		c = candidates{}
		for addr := range changes {
			c.add(n, addr, "native")
		}
	case n == "solana":
		c = parseSolana(n, ev.data)
	default:
		c = parseOther(n, ev.data)
	}
	if n == "aptos" {
		if e := s.aptosOwners(ctx, ev, c); e != nil {
			return e
		}
	}
	// If the scanner matched an address but the evidence doesn't identify its
	// asset, refresh its known holdings as well as the native currency.
	for _, a := range p.Get("activity.addresses").Array() {
		addr, e := canonical(n, a.Str)
		if e == nil && c[addr] == nil {
			c.add(n, addr, "native")
		}
	}
	// Large multi-calls can contain thousands of candidate recipients. Match
	// them against this network's indexed registrations in one SQLite read.
	if chainIDs[n] != 0 {
		addrs := make([]string, 0, len(c))
		for addr := range c {
			addrs = append(addrs, addr)
		}
		rows, e := s.db.QueryContext(ctx, `SELECT w.address FROM json_each(?) a JOIN watches w ON w.network=? AND w.address=a.value WHERE w.id<=? AND w.start BETWEEN 0 AND ?`, compact(addrs), n, ceiling, ev.height)
		if e != nil {
			return e
		}
		matches := candidates{}
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				rows.Close()
				return err
			}
			matches[a] = c[a]
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		c = matches
	}
	event := M{"height": strconv.FormatInt(ev.height, 10), "tx_id": ev.hash}
	if source == "chainscanner" {
		event["delivery_id"] = p.Get("id").String()
	} else {
		event["event_id"] = p.Get("eventId").Str
		event["broadcast_hash"] = p.Get("hash").Str
	}
	type pair struct {
		id int64
		t  string
	}
	targets := map[pair]bool{}
	inventories := map[int64]bool{}
	matched := map[int64]bool{}
	for addr, tokens := range c {
		var wid int64
		e := s.db.QueryRowContext(ctx, "SELECT id FROM watches WHERE network=? AND address=? AND id<=? AND start BETWEEN 0 AND ?", n, addr, ceiling, ev.height).Scan(&wid)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e == nil {
			matched[wid] = true
			for t := range tokens {
				targets[pair{wid, t}] = true
			}
			// Another visible effect (including a fee) cannot exclude an
			// unlogged change to a second, previously held token.
			known, e := s.knownTokens(ctx, wid)
			if e != nil {
				return e
			}
			for _, t := range known {
				targets[pair{wid, t}] = true
			}
			if s.ownerInventoryEnabled(n) {
				inventories[wid] = true
			}
			if n == "ton" {
				found, e := s.jettons(ctx, addr)
				if e != nil {
					return e
				}
				for _, t := range found {
					targets[pair{wid, t}] = true
				}
			}
		}
		if n == "ton" {
			rows, e := s.db.QueryContext(ctx, `SELECT a.watch,a.token FROM assets a JOIN watches w ON w.id=a.watch WHERE w.network='ton' AND a.derived=? AND w.id<=? AND w.start BETWEEN 0 AND ?`, addr, ceiling, ev.height)
			if e != nil {
				return e
			}
			for rows.Next() {
				var p pair
				if e = rows.Scan(&p.id, &p.t); e != nil {
					rows.Close()
					return e
				}
				targets[p] = true
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
		}
	}
	obs := "tx/" + ev.hash + "/" + ev.block
	meta := compact(event)
	e = s.atomic(ctx, func(tx *sql.Tx) error {
		for id := range inventories {
			if _, err := tx.ExecContext(ctx, "UPDATE watches SET discovery_due=min(discovery_due,?) WHERE id=?", time.Now().Add(time.Second).UnixMilli(), id); err != nil {
				return err
			}
		}
		for k := range targets {
			var live bool
			if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM watches WHERE id=?)", k.id).Scan(&live); e != nil {
				return e
			}
			if !live {
				continue
			}
			if _, e := tx.ExecContext(ctx, "INSERT INTO assets(watch,token,name) VALUES(?,?,?) ON CONFLICT DO NOTHING", k.id, k.t, k.t); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO sources VALUES(?,?,?,?,?)", k.id, k.t, obs, source, meta); e != nil {
				return e
			}
			if e := queue(ctx, tx, k.id, k.t, source, obs, ev.height, meta); e != nil {
				return e
			}
		}
		for wid := range matched {
			for token := range contracts {
				if targets[pair{wid, token}] {
					continue
				}
				_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO evm_probes(watch,token,observation,height,source,event)
                  SELECT id,?,?,?,?,? FROM watches WHERE id=? AND NOT EXISTS(SELECT 1 FROM assets WHERE watch=? AND token=?)`, token, obs, ev.height, source, meta, wid, wid, token)
				if err != nil {
					return err
				}
			}
		}
		if ev.traceRetry != nil {
			return nil
		}
		return finishInbox(ctx, tx, id)
	})
	return errors.Join(e, ev.traceRetry)
}
