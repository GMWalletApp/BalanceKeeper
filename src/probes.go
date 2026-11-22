package balancekeeper

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Distinguish contracts that lack an ABI method from a temporarily unusable
// node. A timeout or missing historical state must not discard a candidate.
func (s *Service) callWord(ctx context.Context, n, t, input, at string) (gjson.Result, bool, error) {
	v, err := s.rpc(ctx, n, "eth_call", M{"to": t, "data": input}, at)
	if err != nil {
		msg := strings.ToLower(v.Get("error.message").Str)
		if v.Get("error.code").Int() == 3 || strings.Contains(msg, "execution reverted") || strings.Contains(msg, "invalid opcode") {
			return v, false, nil
		}
		return v, false, err
	}
	if !strings.HasPrefix(v.Str, "0x") || len(v.Str) != 66 {
		return v, false, nil
	}
	_, err = integer(v, 16)
	return v, err == nil, err
}

func (s *Service) probeToken(ctx context.Context, n, a, t string, height int64, obs string) (bool, error) {
	h, err := s.head(ctx, n)
	if err != nil {
		return false, err
	}
	if h.Height < height {
		return false, errors.New("RPC behind token probe")
	}
	eventBlock, err := s.evmBlock(ctx, n, height)
	if err != nil {
		return false, err
	}
	if !strings.HasSuffix(obs, "/"+strings.ToLower(eventBlock.Block)) {
		return false, nil
	}
	owner := strings.Repeat("0", 24) + a[2:]
	at := evmHeight(height)
	balance, ok, err := s.callWord(ctx, n, t, "0x70a08231"+owner, at)
	if err != nil || !ok {
		return false, err
	}
	amount, _ := integer(balance, 16)
	if amount.Sign() == 0 {
		return false, nil
	}
	// ERC-721 has balanceOf too. ERC-20 requires allowance and totalSupply.
	_, ok, err = s.callWord(ctx, n, t, "0xdd62ed3e"+owner+owner, at)
	if err != nil || !ok {
		return false, err
	}
	_, ok, err = s.callWord(ctx, n, t, "0x18160ddd", at)
	if err != nil || !ok {
		return false, err
	}
	iface, ok, err := s.callWord(ctx, n, t, "0x01ffc9a780ac58cd"+strings.Repeat("0", 56), at)
	if err != nil {
		return false, err
	}
	return !(ok && strings.TrimLeft(iface.Str[2:], "0") == "1"), nil
}

func (s *Service) nextProbe(ctx context.Context) (bool, error) {
	var id, wid, height int64
	var attempt int
	var n, a, t, obs, source, event string
	e := s.db.QueryRowContext(ctx, `UPDATE evm_probes SET due=? WHERE id=(SELECT id FROM evm_probes WHERE due<=? ORDER BY due,id LIMIT 1)
    RETURNING id,watch,(SELECT network FROM watches WHERE id=evm_probes.watch),(SELECT address FROM watches WHERE id=evm_probes.watch),token,observation,height,source,event,attempts`, time.Now().Add(4*time.Minute).UnixMilli(), time.Now().UnixMilli()).Scan(&id, &wid, &n, &a, &t, &obs, &height, &source, &event, &attempt)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	yes, e := readNodes(ctx, s, n, func(ctx context.Context) (bool, error) { return s.probeToken(ctx, n, a, t, height, obs) })
	if e != nil {
		_, dbErr := s.db.ExecContext(ctx, "UPDATE evm_probes SET due=?,attempts=attempts+1 WHERE id=? AND watch=?", backoff(attempt), id, wid)
		return true, errors.Join(e, dbErr)
	}
	return true, s.atomic(ctx, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(ctx, "DELETE FROM evm_probes WHERE id=? AND watch=?", id, wid)
		if e != nil {
			return e
		}
		count, e := res.RowsAffected()
		if e != nil || count == 0 || !yes {
			return e
		}
		if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO assets(watch,token,name) VALUES(?,?,?)", wid, t, t); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO sources VALUES(?,?,?,?,?)", wid, t, obs, source, event); e != nil {
			return e
		}
		return queue(ctx, tx, wid, t, source, obs, height, event)
	})
}
