package balancekeeper

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mr-tron/base58"
	"github.com/tidwall/gjson"
)

type broadcastEvent struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"eventId"`
	Name          string `json:"event"`
	Family        string `json:"family"`
	Network       string `json:"network"`
	Hash          string `json:"hash"`
	TxHash        string `json:"txHash,omitempty"`
}

func normalizeHash(family, h string) (string, error) {
	switch family {
	case "solana", "sui":
		b, e := base58.Decode(h)
		want := 32
		if family == "solana" {
			want = 64
		}
		if e == nil && len(b) == want {
			return h, nil
		}
	case "evm", "aptos", "tron", "bitcoin", "ton":
		if family == "ton" {
			if b, e := base64.StdEncoding.DecodeString(h); e == nil && len(b) == 32 {
				return hex.EncodeToString(b), nil
			}
		}
		b, e := hex.DecodeString(strings.TrimPrefix(h, "0x"))
		if e == nil && len(b) == 32 {
			s := hex.EncodeToString(b)
			if family == "evm" || family == "aptos" {
				s = "0x" + s
			}
			return s, nil
		}
	}
	return "", errors.New("invalid transaction identifier")
}
func (s *Service) networkFor(family, id string) string {
	for name, c := range s.cfg.Networks {
		if family == "evm" && chainIDs[name] > 0 && strconv.FormatInt(chainIDs[name], 10) == id {
			return name
		}
		key := name
		if family == "solana" && c.Cluster != "" {
			key = c.Cluster
		}
		if family == name && id == key {
			return name
		}
	}
	return ""
}
func parseBroadcast(o gjson.Result) (broadcastEvent, error) {
	var ev broadcastEvent
	if e := fields(o, "schemaVersion eventId event family network hash txHash"); e != nil {
		return ev, e
	}
	ver := o.Get("schemaVersion")
	ev = broadcastEvent{1, o.Get("eventId").Str, o.Get("event").Str, o.Get("family").Str, o.Get("network").Str, o.Get("hash").Str, o.Get("txHash").Str}
	if ver.Raw != "1" || ev.ID == "" || len(ev.ID) > 256 || ev.Network == "" || ev.Family == "" || ev.Name != "broadcast.accepted" && ev.Name != "broadcast.included" {
		return ev, bad("invalid broadcast event")
	}
	_, e := normalizeHash(ev.Family, ev.Hash)
	if e != nil {
		return ev, bad(e.Error())
	}
	if ev.Name == "broadcast.included" {
		tx, e := normalizeHash(ev.Family, ev.TxHash)
		candidate, _ := normalizeHash(ev.Family, ev.Hash)
		if e != nil || ev.Family != "ton" && candidate != tx {
			return ev, bad("invalid txHash")
		}
	} else if o.Get("txHash").Exists() {
		return ev, bad("unexpected txHash")
	}
	return ev, nil
}
func (s *Service) receiveBroadcasts(w http.ResponseWriter, r *http.Request) {
	secret := s.cfg.BroadcastSecret
	if secret == "" {
		http.NotFound(w, r)
		return
	}
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if e != nil {
		failure(w, bad("invalid request body"))
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(b)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-Broadcaster-Signature"))) {
		failure(w, &fault{"unauthorized", "invalid signature", 401, nil})
		return
	}
	batch, e := rawBatch(b)
	if e != nil {
		failure(w, e)
		return
	}
	e = s.atomic(r.Context(), func(tx *sql.Tx) error {
		for i, o := range batch {
			ev, e := parseBroadcast(o)
			if e == nil {
				e = inboxPut(r.Context(), tx, "broadcaster", ev.ID, s.networkFor(ev.Family, ev.Network), compact(ev))
			}
			if e != nil {
				var f *fault
				if errors.As(e, &f) {
					f.index = ptr(i)
				}
				return e
			}
		}
		return nil
	})
	if e != nil {
		failure(w, e)
		return
	}
	write(w, 204, nil)
}
