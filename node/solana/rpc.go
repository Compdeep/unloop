package solana

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mr-tron/base58"
)

// RPC is a Solana JSON-RPC endpoint, read at the confirmed commitment: the
// reads the bus needs and nothing that sends.
type RPC struct {
	URL  string
	HTTP *http.Client
}

// NewRPC reads from url.
func NewRPC(url string) *RPC {
	return &RPC{URL: url, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Account is an account's address and its data.
type Account struct {
	Address Key
	Owner   Key
	Data    []byte
}

// Filter narrows getProgramAccounts to accounts whose data holds Bytes at
// Offset.
type Filter struct {
	Offset int
	Bytes  []byte
}

// GenesisHash is the cluster's genesis hash, base58.
func (r *RPC) GenesisHash(ctx context.Context) (string, error) {
	var out string
	return out, r.call(ctx, "getGenesisHash", []any{}, &out)
}

// Account reads one account; nil, with no error, when there is none.
func (r *RPC) Account(ctx context.Context, address Key) (*Account, error) {
	var out struct {
		Value *rawAccount `json:"value"`
	}
	params := []any{address.String(), map[string]any{"encoding": "base64", "commitment": "confirmed"}}
	if err := r.call(ctx, "getAccountInfo", params, &out); err != nil {
		return nil, err
	}
	if out.Value == nil {
		return nil, nil
	}
	return out.Value.account(address)
}

// ProgramAccounts reads every account a program owns that passes every filter.
func (r *RPC) ProgramAccounts(ctx context.Context, program Key, filters ...Filter) ([]Account, error) {
	fs := make([]any, len(filters))
	for i, f := range filters {
		fs[i] = map[string]any{"memcmp": map[string]any{"offset": f.Offset, "bytes": base58.Encode(f.Bytes)}}
	}
	params := []any{program.String(), map[string]any{"encoding": "base64", "commitment": "confirmed", "filters": fs}}
	var out []struct {
		Pubkey  string     `json:"pubkey"`
		Account rawAccount `json:"account"`
	}
	if err := r.call(ctx, "getProgramAccounts", params, &out); err != nil {
		return nil, err
	}
	accounts := make([]Account, 0, len(out))
	for _, o := range out {
		k, err := ParseKey(o.Pubkey)
		if err != nil {
			return nil, err
		}
		a, err := o.Account.account(k)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, *a)
	}
	return accounts, nil
}

type rawAccount struct {
	Owner string    `json:"owner"`
	Data  [2]string `json:"data"` // [base64, "base64"]
}

func (a rawAccount) account(address Key) (*Account, error) {
	owner, err := ParseKey(a.Owner)
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(a.Data[0])
	if err != nil {
		return nil, err
	}
	return &Account{Address: address, Owner: owner, Data: data}, nil
}

// busyWaits is how long call waits before each retry of a request the RPC
// turned away as too many (429): a public endpoint limits each address, and
// an agent that gave up at the first refusal would fail mid-round.
// Resending is safe: a read reads again, and a signed transaction sent twice
// is the same transaction.
var busyWaits = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second}

func (r *RPC) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	var res *http.Response
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if res, err = r.HTTP.Do(req); err != nil {
			return err
		}
		if res.StatusCode != http.StatusTooManyRequests || attempt == len(busyWaits) {
			break
		}
		res.Body.Close()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(busyWaits[attempt]):
		}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: the RPC answered %s", method, res.Status)
	}
	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&reply); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if reply.Error != nil {
		return fmt.Errorf("%s: %s (%d)", method, reply.Error.Message, reply.Error.Code)
	}
	if len(reply.Result) == 0 {
		return errors.New(method + ": no result")
	}
	return json.Unmarshal(reply.Result, out)
}

// LatestBlockhash is the blockhash a transaction is built on, and the last
// block height at which it can still land.
func (r *RPC) LatestBlockhash(ctx context.Context) (Key, uint64, error) {
	var out struct {
		Value struct {
			Blockhash            string `json:"blockhash"`
			LastValidBlockHeight uint64 `json:"lastValidBlockHeight"`
		} `json:"value"`
	}
	if err := r.call(ctx, "getLatestBlockhash", []any{map[string]any{"commitment": "confirmed"}}, &out); err != nil {
		return Key{}, 0, err
	}
	k, err := ParseKey(out.Value.Blockhash)
	return k, out.Value.LastValidBlockHeight, err
}

// BlockHeight is the chain's height at the confirmed commitment.
func (r *RPC) BlockHeight(ctx context.Context) (uint64, error) {
	var out uint64
	return out, r.call(ctx, "getBlockHeight", []any{map[string]any{"commitment": "confirmed"}}, &out)
}

// Simulate runs a signed transaction without sending it: its error, if any,
// and its logs.
func (r *RPC) Simulate(ctx context.Context, tx []byte) (failure json.RawMessage, logs []string, err error) {
	var out struct {
		Value struct {
			Err  json.RawMessage `json:"err"`
			Logs []string        `json:"logs"`
		} `json:"value"`
	}
	params := []any{base64.StdEncoding.EncodeToString(tx), map[string]any{"encoding": "base64", "commitment": "confirmed"}}
	if err := r.call(ctx, "simulateTransaction", params, &out); err != nil {
		return nil, nil, err
	}
	if string(out.Value.Err) == "null" {
		out.Value.Err = nil
	}
	return out.Value.Err, out.Value.Logs, nil
}

// Send submits a signed transaction, already simulated, and returns its signature.
func (r *RPC) Send(ctx context.Context, tx []byte) (string, error) {
	var sig string
	params := []any{base64.StdEncoding.EncodeToString(tx), map[string]any{"encoding": "base64", "skipPreflight": true}}
	return sig, r.call(ctx, "sendTransaction", params, &sig)
}

// Status is whether a transaction has landed: confirmed or finalized, and
// in which slot; its error if it failed; or neither while it has not been seen.
func (r *RPC) Status(ctx context.Context, signature string) (confirmed bool, slot uint64, failure json.RawMessage, err error) {
	var out struct {
		Value []*struct {
			Slot               uint64          `json:"slot"`
			Err                json.RawMessage `json:"err"`
			ConfirmationStatus string          `json:"confirmationStatus"`
		} `json:"value"`
	}
	if err := r.call(ctx, "getSignatureStatuses", []any{[]string{signature}}, &out); err != nil {
		return false, 0, nil, err
	}
	if len(out.Value) == 0 || out.Value[0] == nil {
		return false, 0, nil, nil
	}
	s := out.Value[0]
	if string(s.Err) == "null" {
		s.Err = nil
	}
	return s.ConfirmationStatus == "confirmed" || s.ConfirmationStatus == "finalized", s.Slot, s.Err, nil
}

// Balance is an address's lamports.
func (r *RPC) Balance(ctx context.Context, address Key) (uint64, error) {
	var out struct {
		Value uint64 `json:"value"`
	}
	return out.Value, r.call(ctx, "getBalance", []any{address.String(), map[string]any{"commitment": "confirmed"}}, &out)
}

// TokenBalance is what an owner holds of one mint, in its smallest unit.
type TokenBalance struct {
	Mint     string `json:"mint"`
	Amount   string `json:"amount"`
	Decimals int    `json:"decimals"`
}

// TokenBalances is every token an owner holds, under one token program.
func (r *RPC) TokenBalances(ctx context.Context, owner, tokenProgram Key) ([]TokenBalance, error) {
	var out struct {
		Value []struct {
			Account struct {
				Data struct {
					Parsed struct {
						Info struct {
							Mint        string `json:"mint"`
							TokenAmount struct {
								Amount   string `json:"amount"`
								Decimals int    `json:"decimals"`
							} `json:"tokenAmount"`
						} `json:"info"`
					} `json:"parsed"`
				} `json:"data"`
			} `json:"account"`
		} `json:"value"`
	}
	params := []any{owner.String(), map[string]any{"programId": tokenProgram.String()}, map[string]any{"encoding": "jsonParsed", "commitment": "confirmed"}}
	if err := r.call(ctx, "getTokenAccountsByOwner", params, &out); err != nil {
		return nil, err
	}
	var balances []TokenBalance
	for _, v := range out.Value {
		i := v.Account.Data.Parsed.Info
		balances = append(balances, TokenBalance{Mint: i.Mint, Amount: i.TokenAmount.Amount, Decimals: i.TokenAmount.Decimals})
	}
	return balances, nil
}
