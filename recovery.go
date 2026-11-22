package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	pb "github.com/block-vision/sui-go-sdk/pb/sui/rpc/v2"
)

const splProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
const token2022 = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"

func (s *Service) discoverGap(ctx context.Context, n, a string, lo, hi int64) ([]string, int64, error) {
	required := hi
	if chainIDs[n] != 0 {
		required = min(hi, lo+127)
	} else if n == "tron" {
		required = lo
	}
	head, e := s.head(ctx, n)
	if e != nil {
		return nil, lo, e
	}
	if head.Height < required {
		return nil, lo, errors.New("RPC behind discovery window")
	}
	tokens := []string{"native"}
	switch {
	case chainIDs[n] != 0:
		end := min(hi, lo+127)
		logs, e := s.rpc(ctx, n, "eth_getLogs", M{"fromBlock": evmHeight(lo), "toBlock": evmHeight(end), "topics": []any{"0x" + transferTopic, nil, "0x" + strings.Repeat("0", 24) + a[2:]}})
		if e != nil || !logs.IsArray() {
			return nil, lo, invalidEvidence
		}
		c := candidates{}
		addLogs(c, n, logs)
		for t := range c[a] {
			tokens = append(tokens, t)
		}
		return tokens, end + 1, nil
	case n == "tron":
		v, e := s.rest(ctx, n, "/walletsolidity/gettransactioninfobyblocknum", M{"num": lo})
		if e != nil || !v.IsArray() {
			return nil, lo, invalidEvidence
		}
		c := candidates{}
		for _, r := range v.Array() {
			addLogs(c, n, r.Get("log"))
		}
		for t := range c[a] {
			tokens = append(tokens, t)
		}
		return tokens, lo + 1, nil
	case n == "solana":
		for _, program := range []string{splProgram, token2022} {
			v, e := s.rpc(ctx, n, "getTokenAccountsByOwner", a, M{"programId": program}, M{"commitment": "finalized", "encoding": "jsonParsed", "minContextSlot": hi})
			if e != nil || !v.Get("value").IsArray() || v.Get("context.slot").Int() < hi {
				return nil, lo, invalidEvidence
			}
			for _, row := range v.Get("value").Array() {
				tokens = append(tokens, row.Get("account.data.parsed.info.mint").Str)
			}
		}
	case n == "ton":
		t, e := s.jettons(ctx, a)
		if e != nil {
			return nil, lo, e
		}
		tokens = append(tokens, t...)
	case n == "sui":
		ctx, c, close, e := s.suiConn(ctx)
		if e != nil {
			return nil, lo, e
		}
		defer close()
		req := &pb.ListBalancesRequest{Owner: &a}
		for {
			r, e := pb.NewStateServiceClient(c).ListBalances(ctx, req)
			if e != nil {
				return nil, lo, e
			}
			for _, coin := range r.GetBalances() {
				tokens = append(tokens, coin.GetCoinType())
			}
			if len(r.GetNextPageToken()) == 0 {
				break
			}
			req.PageToken = r.GetNextPageToken()
		}
	case n == "aptos":
		// Replay bounded ledger pages; this also works without an external indexer.
		v, e := s.rest(ctx, n, fmt.Sprintf("/v1/transactions?start=%d&limit=%d", lo, min(25, hi-lo+1)), nil)
		if e != nil || !v.IsArray() || len(v.Array()) == 0 {
			return nil, lo, invalidEvidence
		}
		for _, tx := range v.Array() {
			h, e := number(tx.Get("version"), 10)
			if e != nil || h != lo {
				return nil, lo, invalidEvidence
			}
			c := parseOther(n, tx)
			if e = s.aptosOwners(ctx, evidence{height: h, data: tx}, c); e != nil {
				return nil, lo, e
			}
			for t := range c[a] {
				tokens = append(tokens, t)
			}
			lo++
		}
		return tokens, lo, nil
	}
	return tokens, hi + 1, nil
}
func (s *Service) nextGap(ctx context.Context) (bool, error) {
	var id, lo, hi int64
	var n, a string
	var attempts int
	e := s.db.QueryRowContext(ctx, `SELECT w.id,w.network,w.address,g.cursor,g.end,g.attempts FROM gaps g JOIN watches w ON w.id=g.watch WHERE g.due<=? ORDER BY g.due,w.id LIMIT 1`, time.Now().UnixMilli()).Scan(&id, &n, &a, &lo, &hi, &attempts)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	tokens, next, e := s.discoverGap(ctx, n, a, lo, hi)
	if e != nil {
		_, de := s.db.ExecContext(ctx, "UPDATE gaps SET attempts=attempts+1,due=?,last_error=? WHERE watch=?", backoff(attempts), e.Error(), id)
		return true, errors.Join(e, de)
	}
	return true, s.atomic(ctx, func(tx *sql.Tx) error {
		var end int64
		e := tx.QueryRowContext(ctx, "SELECT end FROM gaps WHERE watch=? AND cursor=?", id, lo).Scan(&end)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		for _, raw := range tokens {
			t, e := tokenID(n, raw)
			if e != nil {
				return e
			}
			if e = queue(ctx, tx, id, t, "reconcile", fmt.Sprintf("reconcile-%d", hi+1), hi, ""); e != nil {
				return e
			}
		}
		if next > end {
			_, e = tx.ExecContext(ctx, "DELETE FROM gaps WHERE watch=?", id)
		} else {
			_, e = tx.ExecContext(ctx, "UPDATE gaps SET cursor=?,attempts=0,due=0,last_error='' WHERE watch=?", next, id)
		}
		return e
	})
}

// A first Jetton transfer may only touch the recipient's derived wallet.
// Use TON's owner index to discover these wallets before they have a subscription.
func (s *Service) nextJettonOwner(ctx context.Context) (bool, error) {
	var id, start int64
	var a string
	e := s.db.QueryRowContext(ctx, "SELECT id,address,start FROM watches WHERE network='ton' AND start>=0 AND discovery_due<=? ORDER BY discovery_due,id LIMIT 1", time.Now().UnixMilli()).Scan(&id, &a, &start)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	tokens, e := s.jettons(ctx, a)
	if e != nil {
		_, de := s.db.ExecContext(ctx, "UPDATE watches SET discovery_due=? WHERE id=?", time.Now().Add(time.Minute).UnixMilli(), id)
		return true, errors.Join(e, de)
	}
	return true, s.atomic(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE watches SET discovery_due=? WHERE id=?", time.Now().Add(5*time.Minute).UnixMilli(), id)
		if err != nil {
			return err
		}
		if count, _ := res.RowsAffected(); count == 0 {
			return nil
		}
		for _, t := range tokens {
			res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO assets(watch,token,name) VALUES(?,?,?)", id, t, t)
			if err != nil {
				return err
			}
			if count, _ := res.RowsAffected(); count == 0 {
				continue
			}
			if err := queue(ctx, tx, id, t, "reconcile", "owner-discovery", start-1, ""); err != nil {
				return err
			}
		}
		return nil
	})
}
