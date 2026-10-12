package chain

import (
	"context"
	"fmt"

	"github.com/Compdeep/unloop/node/solana"
)

// Client builds the agent's instructions from the program's accounts. Each
// action is chain/client's of the same name: the same accounts in the same
// order, the same data.
type Client struct {
	program *solana.Program
	rpc     *solana.RPC
}

// NewClient reads the program through rpc.
func NewClient(rpc *solana.RPC, program *solana.Program) *Client {
	return &Client{program: program, rpc: rpc}
}

func (c *Client) seed(seeds ...[]byte) solana.Key { return c.program.Address(seeds...) }

func (c *Client) objective(ctx context.Context, address solana.Key) (*solana.Objective, error) {
	o, err := c.program.Objective(ctx, address)
	if err == nil && o == nil {
		err = fmt.Errorf("no objective at %s", address)
	}
	return o, err
}

func (c *Client) claim(ctx context.Context, address solana.Key) (*solana.Claim, *solana.Objective, error) {
	cl, err := c.program.Claim(ctx, address)
	if err == nil && cl == nil {
		err = fmt.Errorf("no claim at %s", address)
	}
	if err != nil {
		return nil, nil, err
	}
	o, err := c.objective(ctx, cl.Objective)
	return cl, o, err
}

// File records a claim (record_claim): its hash, the award it is for, the
// claims it builds on, and the most the agent will pay validators. The fee
// accounts ride along only where the colony charges, so a free colony needs
// no protocol token.
func (c *Client) File(ctx context.Context, agent, objective solana.Key, hash [32]byte, awardIndex uint8, roots []solana.Key, maxFee uint64) ([]Instruction, error) {
	o, err := c.objective(ctx, objective)
	if err != nil {
		return nil, err
	}
	fee, err := c.program.ValidationFee(ctx, o.Colony)
	if err != nil {
		return nil, err
	}
	// An optional account left out is named by the program's own address.
	none := r(c.program.ID())
	tokenConfig, mint, source, fees, tokenProgram := none, none, none, none, none
	if fee > 0 {
		t, err := c.program.ProtocolToken(ctx)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, fmt.Errorf("this colony charges a validation fee, and the protocol token does not exist on this chain")
		}
		tokenConfig, mint, tokenProgram = r(t.Config), w(t.Mint), r(t.Program)
		source, fees = w(ata(agent, t.Mint, t.Program)), w(c.seed([]byte("fees"), objective[:]))
	}
	claim := c.seed([]byte("claim"), objective[:], hash[:], agent[:])
	data := call("record_claim").bytes(hash[:]).u8(awardIndex).u32(uint32(len(roots)))
	for _, root := range roots {
		data = data.bytes(root[:])
	}
	accounts := []Meta{ws(agent), w(objective), w(claim), r(o.Colony), tokenConfig, mint, source, fees, tokenProgram, r(SystemProgram)}
	for _, root := range roots {
		accounts = append(accounts, r(root))
	}
	return []Instruction{{Program: c.program.ID(), Accounts: accounts, Data: data.u64(maxFee)}}, nil
}

// Seed fixes who validates a claim (select_validators), from the block
// that seeds it. Anybody may send it, and pays its fee.
func (c *Client) Seed(ctx context.Context, caller, claim solana.Key) ([]Instruction, error) {
	cl, o, err := c.claim(ctx, claim)
	if err != nil {
		return nil, err
	}
	return []Instruction{{Program: c.program.ID(), Data: call("select_validators"), Accounts: []Meta{
		rs(caller), w(claim), r(cl.Objective), r(o.Colony), r(SlotHashes),
	}}}, nil
}

// validation is the accounts commit and reveal share.
func (c *Client) validation(ctx context.Context, claim, validator solana.Key) (colony, objective, record, validation solana.Key, err error) {
	cl, o, err := c.claim(ctx, claim)
	if err != nil {
		return
	}
	return o.Colony, cl.Objective, c.seed([]byte("validator"), o.Colony[:], validator[:]), c.seed([]byte("validation"), claim[:], validator[:]), nil
}

// Commit seals a validator's finding (commit_validation).
func (c *Client) Commit(ctx context.Context, claim, validator solana.Key, commitment [32]byte) ([]Instruction, error) {
	colony, objective, record, validation, err := c.validation(ctx, claim, validator)
	if err != nil {
		return nil, err
	}
	return []Instruction{{Program: c.program.ID(), Data: call("commit_validation").bytes(commitment[:]), Accounts: []Meta{
		ws(validator), r(colony), w(record), r(objective), w(claim), w(validation), r(SlotHashes), r(SystemProgram),
	}}}, nil
}

// Reveal opens it (reveal_validation) with the finding and nonce it was made from.
func (c *Client) Reveal(ctx context.Context, claim, validator solana.Key, f Finding, nonce [32]byte) ([]Instruction, error) {
	e, err := f.fields()
	if err != nil {
		return nil, err
	}
	colony, objective, record, validation, err := c.validation(ctx, claim, validator)
	if err != nil {
		return nil, err
	}
	data := call("reveal_validation").u8(e[0]).u8(e[1]).u8(e[2]).u16(f.NoveltyBps).u16(f.ScoreBps).u8(e[3]).bytes(f.Finding[:]).bytes(nonce[:])
	return []Instruction{{Program: c.program.ID(), Data: data, Accounts: []Meta{
		rs(validator), w(record), r(colony), r(objective), w(claim), w(validation),
	}}}, nil
}

// Collect pays an agent what its escrow on an objective holds, less the
// protocol's share. Both destinations are opened if they are not there: the
// agent's own, and the treasury's, which a first collection in a new mint
// finds missing.
func (c *Client) Collect(ctx context.Context, agent, objective solana.Key) ([]Instruction, error) {
	o, err := c.objective(ctx, objective)
	if err != nil {
		return nil, err
	}
	treasury, err := c.program.Treasury(ctx)
	if err != nil {
		return nil, err
	}
	tokenProgram, err := c.program.Owner(ctx, o.PaymentMint)
	if err != nil {
		return nil, err
	}
	mine, openMine := openATA(agent, agent, o.PaymentMint, tokenProgram)
	theirs, openTheirs := openATA(agent, treasury, o.PaymentMint, tokenProgram)
	return []Instruction{openMine, openTheirs, {Program: c.program.ID(), Data: call("collect"), Accounts: []Meta{
		ws(agent), r(c.seed([]byte("config"))), r(objective), w(c.seed([]byte("escrow"), objective[:], agent[:])),
		r(o.PaymentMint), w(mine), w(theirs), w(c.seed([]byte("buyback"), o.PaymentMint[:])), r(tokenProgram), r(SystemProgram),
	}}}, nil
}

// CollectFee pays a validator its share of a claim's fee
// (collect_validation_fee), into an account opened if it is not there.
func (c *Client) CollectFee(ctx context.Context, validator, claim solana.Key) ([]Instruction, error) {
	cl, o, err := c.claim(ctx, claim)
	if err != nil {
		return nil, err
	}
	t, err := c.program.ProtocolToken(ctx)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("the protocol token does not exist on this chain, so no fee was paid")
	}
	mine, open := openATA(validator, validator, t.Mint, t.Program)
	return []Instruction{open, {Program: c.program.ID(), Data: call("collect_validation_fee"), Accounts: []Meta{
		ws(validator), r(cl.Objective), r(claim), w(c.seed([]byte("validation"), claim[:], validator[:])),
		w(c.seed([]byte("validator"), o.Colony[:], validator[:])), r(t.Config), r(t.Mint),
		w(c.seed([]byte("fees"), cl.Objective[:])), w(mine), r(t.Program),
	}}}, nil
}

// Fund adds to an objective's pool (top_up_objective) from the payer's own
// account for its payment mint.
func (c *Client) Fund(ctx context.Context, objective, payer solana.Key, amount uint64) ([]Instruction, error) {
	o, err := c.objective(ctx, objective)
	if err != nil {
		return nil, err
	}
	tokenProgram, err := c.program.Owner(ctx, o.PaymentMint)
	if err != nil {
		return nil, err
	}
	return []Instruction{{Program: c.program.ID(), Data: call("top_up_objective").u64(amount), Accounts: []Meta{
		ws(payer), w(objective), w(c.seed([]byte("vault"), objective[:])), r(o.PaymentMint),
		w(ata(payer, o.PaymentMint, tokenProgram)), r(tokenProgram),
	}}}, nil
}

// Withdraw takes back what an objective's pool holds that no claim is owed
// (withdraw_unencumbered), into the leader's own account for its payment
// mint, opened first if it is not there. How much may go is the program's
// arithmetic, which refuses more.
func (c *Client) Withdraw(ctx context.Context, objective, leader solana.Key, amount uint64) ([]Instruction, error) {
	o, err := c.objective(ctx, objective)
	if err != nil {
		return nil, err
	}
	tokenProgram, err := c.program.Owner(ctx, o.PaymentMint)
	if err != nil {
		return nil, err
	}
	mine, open := openATA(leader, leader, o.PaymentMint, tokenProgram)
	return []Instruction{open, {Program: c.program.ID(), Data: call("withdraw_unencumbered").u64(amount), Accounts: []Meta{
		rs(leader), r(o.Colony), w(objective), w(c.seed([]byte("vault"), objective[:])), r(o.PaymentMint),
		w(mine), r(tokenProgram),
	}}}, nil
}

// Settlement is what resolve_claim is given. Only the settlement authority's
// counts, and only where the validators did not decide; anybody else's is
// dropped by the program, which then settles as the validators decided or,
// after the grace, rejects.
type Settlement struct {
	Admit                bool
	NoveltyBps, ScoreBps uint16
	// RootsConfirmed is a bit per declared root the claim was built on; only
	// the settlement authority confirms one.
	RootsConfirmed uint8
}

// Resolve settles a pending claim (resolve_claim). Who may, and when, is the
// program's: the settlement authority at any time once every commitment is
// open, anybody once the validation window has closed.
func (c *Client) Resolve(ctx context.Context, caller, claim solana.Key, s Settlement) ([]Instruction, error) {
	cl, err := c.program.Claim(ctx, claim)
	if err == nil && cl == nil {
		err = fmt.Errorf("no claim at %s", claim)
	}
	if err != nil {
		return nil, err
	}
	admit := uint8(0)
	if s.Admit {
		admit = 1
	}
	data := call("resolve_claim").u8(admit).u16(s.NoveltyBps).u16(s.ScoreBps).u8(s.RootsConfirmed)
	return []Instruction{{Program: c.program.ID(), Data: data, Accounts: []Meta{rs(caller), w(cl.Objective), w(claim)}}}, nil
}

// Award pays an admitted claim into its agent's escrow (award_claim). The
// program computes the amount; anybody may send it.
func (c *Client) Award(ctx context.Context, caller, claim solana.Key) ([]Instruction, error) {
	cl, o, err := c.claim(ctx, claim)
	if err != nil {
		return nil, err
	}
	tokenProgram, err := c.program.Owner(ctx, o.PaymentMint)
	if err != nil {
		return nil, err
	}
	return []Instruction{{Program: c.program.ID(), Data: call("award_claim"), Accounts: []Meta{
		ws(caller), w(cl.Objective), w(claim), w(c.seed([]byte("escrow"), cl.Objective[:], cl.Agent[:])),
		w(c.seed([]byte("vault"), cl.Objective[:])), r(o.PaymentMint), r(tokenProgram), r(SystemProgram),
	}}}, nil
}

// Edge mints the protocol token at both ends of a confirmed edge
// (mint_edge_reward): to root rootIndex's agent and to the claim's. Either
// account is opened if it is not there; anybody may send it.
func (c *Client) Edge(ctx context.Context, caller, claim solana.Key, rootIndex uint8) ([]Instruction, error) {
	cl, o, err := c.claim(ctx, claim)
	if err != nil {
		return nil, err
	}
	if int(rootIndex) >= len(cl.Roots) {
		return nil, fmt.Errorf("this claim declares %d roots; there is no root %d", len(cl.Roots), rootIndex)
	}
	root := cl.Roots[rootIndex]
	rc, err := c.program.Claim(ctx, root)
	if err == nil && rc == nil {
		err = fmt.Errorf("no claim at %s", root)
	}
	if err != nil {
		return nil, err
	}
	t, err := c.program.ProtocolToken(ctx)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("the protocol token does not exist on this chain, so nothing can be minted")
	}
	rootAccount, openRoot := openATA(caller, rc.Agent, t.Mint, t.Program)
	citerAccount, openCiter := openATA(caller, cl.Agent, t.Mint, t.Program)
	return []Instruction{openRoot, openCiter, {Program: c.program.ID(), Data: call("mint_edge_reward").u8(rootIndex), Accounts: []Meta{
		ws(caller), w(t.Config), w(t.Mint), r(cl.Objective), r(c.seed([]byte("mintable"), o.Colony[:])),
		w(claim), r(root), w(rootAccount), w(citerAccount), r(t.Program),
	}}}, nil
}
