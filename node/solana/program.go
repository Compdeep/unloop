package solana

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/envelope"
)

// Program is the Unloop program's accounts, read through an RPC: what
// unloop-node hands the bus as its Accounts.
//
// Each message names an account, and the address that may sign it is the one
// the account names (docs/PLATFORM.md section 6, the author rule):
//
//	colony.announce                      the colony's leader
//	objective.publish, objective.update  the leader of the objective's colony
//	claim.publish, claim.notice          the claim's agent: the claim's address
//	                                     is derived from it, so the account
//	                                     existing is the proof
//	claim.resolved                       the objective's settlement authority
//	validation.publish                   a validator with a commitment on the claim
//	amendment                            the claim's agent
//	cannot_verify, clarification,        the claim's agent or one of its validators
//	validators.selected
//	validator.announce                   an address in the colony's pool
//	claim.paid                           anybody, about a claim that exists
//
// Every account must belong to the topic's colony. An account not there is
// ErrNoAccount, which the bus waits on and then ignores: the node's RPC may
// simply be behind. An account that is there and names somebody else is
// ErrNotAuthor, and one in another colony is ErrChannel.
type Program struct {
	rpc *RPC
	id  Key
}

// NewProgram reads the program at id through rpc.
func NewProgram(rpc *RPC, id Key) *Program { return &Program{rpc: rpc, id: id} }

// Layouts: an 8-byte discriminator, then the fields the rule reads, each at a
// fixed offset because every field before it is fixed size
// (chain/programs/unloop/src/state; checked against the IDL by the tests).
const (
	colonyLeader         = 8  // Colony.leader
	colonySlug           = 40 // Colony.slug, a u32 length and the bytes
	objectiveColony      = 8  // Objective.colony
	objectiveSettlement  = 40 // Objective.settlement_authority
	objectivePaymentMint = 72 // Objective.payment_mint
	// Objective.validators_per_claim and the three deadlines after it, past
	// the consensus, index, payout and status, three hashes, the entry proof
	// count, three awards of 74 bytes, the award count, the pool and the
	// pending claims — every one fixed size.
	objectivePanel = 104 + 6 + 8 + 1 + 1 + 32*3 + 4 + 3*74 + 1 + 8 + 8
	claimObjective = 8  // Claim.objective
	claimAgent     = 40 // Claim.agent
	claimHash      = 72 // Claim.hash
	// Claim.status, past the award index and the slot; the eight root slots,
	// their count, and the confirmed and paid bits follow it.
	claimStatus    = 72 + 32 + 1 + 8
	claimRoots     = claimStatus + 1
	claimRootCount = claimRoots + 8*32
	// Claim.decided_by, the first Option: past the fee, the selection, the
	// counts, the sums and ranges, the figures and the bump. Claim.awarded
	// follows it, one byte later where it is None.
	claimDecidedBy  = claimRootCount + 3 + 8 + 8 + 4 + 3*2 + 2*4 + 2 + 4*2 + 2*2 + 1
	configTreasury  = 40 // Config.treasury
	tokenConfigMint = 40 // TokenConfig.mint
	// Colony.validation_fee, counted from the end of the slug: payment_mint,
	// consensus (three u16), admission (a one-byte enum), admission_authority,
	// entry_proof_required, bond, probation_validations, eligible and trusted;
	// objective_count follows it.
	colonyFeeAfterSlug = 32 + 6 + 1 + 32 + 1 + 8 + 4 + 4 + 4
)

func discriminator(account string) []byte {
	h := sha256.Sum256([]byte("account:" + account))
	return h[:8]
}

// Colonies is every colony's slug, for a node that follows them all.
func (p *Program) Colonies(ctx context.Context) ([]string, error) {
	accounts, err := p.rpc.ProgramAccounts(ctx, p.id, Filter{0, discriminator("Colony")})
	if err != nil {
		return nil, err
	}
	slugs := make([]string, 0, len(accounts))
	for _, a := range accounts {
		if len(a.Data) < colonySlug+4 {
			continue
		}
		n := int(binary.LittleEndian.Uint32(a.Data[colonySlug:]))
		if len(a.Data) < colonySlug+4+n {
			continue
		}
		slugs = append(slugs, string(a.Data[colonySlug+4:colonySlug+4+n]))
	}
	return slugs, nil
}

// Authorize is bus.Accounts.
func (p *Program) Authorize(ctx context.Context, m *envelope.Message) error {
	author, err := ParseKey(m.Author)
	if err != nil {
		return fmt.Errorf("%w: %v", bus.ErrNotAuthor, err)
	}
	colony, err := FindAddress(p.id, []byte("colony"), []byte(m.Colony))
	if err != nil {
		return err
	}
	if m.Body == nil && m.Type != "validator.announce" {
		// The account it names is in a body this node has not fetched.
		return errors.New("the body is carried by reference, and the rule needs it")
	}
	body := fields(m.Body)

	switch m.Type {
	case "colony.announce":
		return p.leads(ctx, colony, author)

	case "objective.publish", "objective.update":
		if _, err := p.objective(ctx, colony, body.key("id")); err != nil {
			return err
		}
		return p.leads(ctx, colony, author)

	case "claim.publish", "claim.notice":
		objective, err := ParseKey(body.text("objective"))
		if err != nil {
			return fmt.Errorf("%w: %v", bus.ErrNotAuthor, err)
		}
		if _, err := p.objective(ctx, colony, objective); err != nil {
			return err
		}
		hash, err := hex.DecodeString(body.text("claimHash"))
		if err != nil {
			return fmt.Errorf("%w: %v", bus.ErrNotAuthor, err)
		}
		claim, err := FindAddress(p.id, []byte("claim"), objective[:], hash, author[:])
		if err != nil {
			return err
		}
		return p.exists(ctx, claim, "Claim")

	case "claim.resolved":
		settlement, err := p.objective(ctx, colony, body.key("objective"))
		if err != nil {
			return err
		}
		if settlement != author {
			return fmt.Errorf("%w: the objective's settlement authority is %s", bus.ErrNotAuthor, settlement)
		}
		return nil

	case "claim.paid":
		_, err := p.claims(ctx, colony, body.text("claimHash"))
		return err

	case "validation.publish":
		claims, err := p.claims(ctx, colony, body.text("claimHash"))
		if err != nil {
			return err
		}
		return p.validates(ctx, claims, author)

	case "amendment":
		claims, err := p.claims(ctx, colony, body.text("claimHash"))
		if err != nil {
			return err
		}
		if agentOf(claims, author) {
			return nil
		}
		return fmt.Errorf("%w: not the claim's agent", bus.ErrNotAuthor)

	case "cannot_verify", "clarification", "validators.selected":
		hash := body.text("claimHash")
		if m.Type == "validators.selected" {
			hash = body.text("claim")
		}
		claims, err := p.claims(ctx, colony, hash)
		if err != nil {
			return err
		}
		if agentOf(claims, author) {
			return nil
		}
		return p.validates(ctx, claims, author)

	case "validator.announce":
		record, err := FindAddress(p.id, []byte("validator"), colony[:], author[:])
		if err != nil {
			return err
		}
		return p.exists(ctx, record, "Validator")
	}
	return fmt.Errorf("%w: no rule for %s", bus.ErrChannel, m.Type)
}

// ID is the program's address.
func (p *Program) ID() Key { return p.id }

// Address is the program-derived address for seeds.
func (p *Program) Address(seeds ...[]byte) Key {
	k, err := FindAddress(p.id, seeds...)
	if err != nil {
		panic(err) // a bump off the curve exists for every seed the program uses
	}
	return k
}

// Objective is what the agent's transactions read of an objective.
type Objective struct {
	Address, Colony, SettlementAuthority, PaymentMint Key
	// ValidatorsPerClaim is the panel each claim pays: the fee is this many
	// times the colony's validation fee.
	ValidatorsPerClaim uint8
	// The deadlines, in unix seconds: claims, commitments, validation.
	ClaimDeadline, CommitDeadline, ValidationDeadline int64
}

// Objective reads one; nil when there is none.
func (p *Program) Objective(ctx context.Context, address Key) (*Objective, error) {
	data, err := p.read(ctx, address, "Objective")
	if err != nil || data == nil {
		return nil, err
	}
	if len(data) < objectivePanel+25 {
		return nil, fmt.Errorf("objective %s is shorter than its layout", address)
	}
	i64 := func(at int) int64 { return int64(binary.LittleEndian.Uint64(data[at:])) }
	return &Objective{Address: address, Colony: keyAt(data, objectiveColony),
		SettlementAuthority: keyAt(data, objectiveSettlement), PaymentMint: keyAt(data, objectivePaymentMint),
		ValidatorsPerClaim: data[objectivePanel], ClaimDeadline: i64(objectivePanel + 1),
		CommitDeadline: i64(objectivePanel + 9), ValidationDeadline: i64(objectivePanel + 17)}, nil
}

// Claim is what the agent's transactions read of a claim.
type Claim struct {
	Address, Objective, Agent Key
	Hash                      [32]byte
	// Status is pending, admitted or rejected.
	Status string
	// Roots are the claims it declares it builds on; bit i of RootsConfirmed
	// and RootsPaid is Roots[i]'s edge, confirmed at resolution and paid.
	Roots                     []Key
	RootsConfirmed, RootsPaid uint8
	// DecidedBy is consensus, leader, deadline or unseeded once it is
	// resolved, and empty before.
	DecidedBy string
	// Awarded is whether award_claim has paid it.
	Awarded bool
}

// Payable is whether root i's edge is confirmed and not yet paid
// (Claim::edge_is_payable).
func (c *Claim) Payable(i int) bool {
	return i < len(c.Roots) && c.RootsConfirmed&(1<<i) != 0 && c.RootsPaid&(1<<i) == 0
}

// Claim reads one; nil when there is none.
func (p *Program) Claim(ctx context.Context, address Key) (*Claim, error) {
	data, err := p.read(ctx, address, "Claim")
	if err != nil || data == nil {
		return nil, err
	}
	if len(data) < claimDecidedBy+2 {
		return nil, fmt.Errorf("claim %s is shorter than its layout", address)
	}
	c := &Claim{Address: address, Objective: keyAt(data, claimObjective), Agent: keyAt(data, claimAgent),
		RootsConfirmed: data[claimRootCount+1], RootsPaid: data[claimRootCount+2]}
	copy(c.Hash[:], data[claimHash:claimHash+32])
	statuses := []string{"pending", "admitted", "rejected"}
	if int(data[claimStatus]) >= len(statuses) || data[claimRootCount] > 8 {
		return nil, fmt.Errorf("claim %s does not read as a claim", address)
	}
	c.Status = statuses[data[claimStatus]]
	for i := 0; i < int(data[claimRootCount]); i++ {
		c.Roots = append(c.Roots, keyAt(data, claimRoots+32*i))
	}
	awarded := claimDecidedBy + 1
	if data[claimDecidedBy] == 1 {
		deciders := []string{"consensus", "leader", "deadline", "unseeded"}
		if int(data[awarded]) >= len(deciders) {
			return nil, fmt.Errorf("claim %s does not read as a claim", address)
		}
		c.DecidedBy = deciders[data[awarded]]
		awarded++
	}
	c.Awarded = len(data) > awarded && data[awarded] == 1
	return c, nil
}

// Colony is what the agent reads of a colony.
type Colony struct {
	Address, Leader, PaymentMint Key
	Slug                         string
	// MinVotes is the consensus policy's: the votes a claim needs to be decided.
	MinVotes uint16
	// Admission is public, keyed or private.
	Admission string
	// ValidationFee is what the colony charges per validator on a claim.
	ValidationFee uint64
	// ObjectiveCount numbers the next objective: its address is derived from it.
	ObjectiveCount uint64
}

// Colony reads one; nil when there is none.
func (p *Program) Colony(ctx context.Context, address Key) (*Colony, error) {
	data, err := p.read(ctx, address, "Colony")
	if err != nil || data == nil {
		return nil, err
	}
	n := int(binary.LittleEndian.Uint32(data[colonySlug:]))
	after := colonySlug + 4 + n
	if len(data) < after+colonyFeeAfterSlug+16 {
		return nil, fmt.Errorf("colony %s is shorter than its layout", address)
	}
	admission := "unknown"
	if a := int(data[after+32+6]); a < len(admissions) {
		admission = admissions[a]
	}
	return &Colony{Address: address, Leader: keyAt(data, colonyLeader), Slug: string(data[colonySlug+4 : after]),
		PaymentMint: keyAt(data, after), MinVotes: binary.LittleEndian.Uint16(data[after+32:]), Admission: admission,
		ValidationFee:  binary.LittleEndian.Uint64(data[after+colonyFeeAfterSlug:]),
		ObjectiveCount: binary.LittleEndian.Uint64(data[after+colonyFeeAfterSlug+8:])}, nil
}

// admissions is the Admission enum, in the program's order.
var admissions = []string{"public", "keyed", "private"}

// ValidationFee is what a colony charges per validator on a claim.
func (p *Program) ValidationFee(ctx context.Context, colony Key) (uint64, error) {
	c, err := p.Colony(ctx, colony)
	if err == nil && c == nil {
		err = fmt.Errorf("no colony at %s", colony)
	}
	if err != nil {
		return 0, err
	}
	return c.ValidationFee, nil
}

// Treasury is where the protocol's share of a collection goes (Config.treasury).
func (p *Program) Treasury(ctx context.Context) (Key, error) {
	config := p.Address([]byte("config"))
	data, err := p.read(ctx, config, "Config")
	if err != nil {
		return Key{}, err
	}
	if data == nil {
		return Key{}, errors.New("the program has no config on this chain")
	}
	return keyAt(data, configTreasury), nil
}

// Token is the protocol token: its config, its mint, and the token program
// that owns the mint.
type Token struct{ Config, Mint, Program Key }

// ProtocolToken reads it; nil where nobody has initialised it.
func (p *Program) ProtocolToken(ctx context.Context) (*Token, error) {
	config := p.Address([]byte("token"))
	data, err := p.read(ctx, config, "TokenConfig")
	if err != nil || data == nil {
		return nil, err
	}
	mint := keyAt(data, tokenConfigMint)
	program, err := p.Owner(ctx, mint)
	if err != nil {
		return nil, err
	}
	return &Token{Config: config, Mint: mint, Program: program}, nil
}

// Owner is the program that owns an account: for a mint, Token or Token-2022.
func (p *Program) Owner(ctx context.Context, address Key) (Key, error) {
	a, err := p.rpc.Account(ctx, address)
	if err != nil {
		return Key{}, err
	}
	if a == nil {
		return Key{}, fmt.Errorf("%s is not on this chain", address)
	}
	return a.Owner, nil
}

// read is a program account of a kind, or nil when there is none.
func (p *Program) read(ctx context.Context, address Key, kind string) ([]byte, error) {
	a, err := p.rpc.Account(ctx, address)
	if err != nil || a == nil {
		return nil, err
	}
	if a.Owner != p.id || !bytes.HasPrefix(a.Data, discriminator(kind)) {
		return nil, fmt.Errorf("%w: %s is not a %s", bus.ErrNotAuthor, address, kind)
	}
	return a.Data, nil
}

func (p *Program) exists(ctx context.Context, address Key, kind string) error {
	data, err := p.read(ctx, address, kind)
	if err != nil {
		return err
	}
	if data == nil {
		return fmt.Errorf("%w: no %s at %s", bus.ErrNoAccount, kind, address)
	}
	return nil
}

// leads is whether author is the colony's leader.
func (p *Program) leads(ctx context.Context, colony, author Key) error {
	data, err := p.read(ctx, colony, "Colony")
	if err != nil {
		return err
	}
	if data == nil {
		return fmt.Errorf("%w: no colony at %s", bus.ErrNoAccount, colony)
	}
	if leader := keyAt(data, colonyLeader); leader != author {
		return fmt.Errorf("%w: the colony's leader is %s", bus.ErrNotAuthor, leader)
	}
	return nil
}

// objective reads an objective in the colony, and returns its settlement
// authority.
func (p *Program) objective(ctx context.Context, colony Key, address Key) (Key, error) {
	o, err := p.Objective(ctx, address)
	if err != nil {
		return Key{}, err
	}
	if o == nil {
		return Key{}, fmt.Errorf("%w: no objective at %s", bus.ErrNoAccount, address)
	}
	if o.Colony != colony {
		return Key{}, fmt.Errorf("%w: objective %s is another colony's", bus.ErrChannel, address)
	}
	return o.SettlementAuthority, nil
}

type claim struct{ address, agent Key }

// claims are the colony's claims with this hash: one, nearly always, but the
// hash alone does not name a claim, since its address includes the objective
// and the agent.
func (p *Program) claims(ctx context.Context, colony Key, hashHex string) ([]claim, error) {
	hash, err := hex.DecodeString(hashHex)
	if err != nil || len(hash) != 32 {
		return nil, fmt.Errorf("%w: %q is not a claim hash", bus.ErrNotAuthor, hashHex)
	}
	accounts, err := p.rpc.ProgramAccounts(ctx, p.id, Filter{0, discriminator("Claim")}, Filter{claimHash, hash})
	if err != nil {
		return nil, err
	}
	var out []claim
	elsewhere := false
	for _, a := range accounts {
		if len(a.Data) < claimHash+32 {
			continue
		}
		if _, err := p.objective(ctx, colony, keyAt(a.Data, claimObjective)); err != nil {
			if errors.Is(err, bus.ErrChannel) {
				elsewhere = true
				continue
			}
			return nil, err
		}
		out = append(out, claim{address: a.Address, agent: keyAt(a.Data, claimAgent)})
	}
	if len(out) == 0 {
		if elsewhere {
			return nil, fmt.Errorf("%w: the claim is another colony's", bus.ErrChannel)
		}
		return nil, fmt.Errorf("%w: no claim with hash %s", bus.ErrNoAccount, hashHex)
	}
	return out, nil
}

// validates is whether author holds a commitment on one of the claims.
func (p *Program) validates(ctx context.Context, claims []claim, author Key) error {
	for _, c := range claims {
		v, err := FindAddress(p.id, []byte("validation"), c.address[:], author[:])
		if err != nil {
			return err
		}
		err = p.exists(ctx, v, "Validation")
		if err == nil || !errors.Is(err, bus.ErrNoAccount) {
			return err
		}
	}
	// The claim is there and no commitment by this author is: the commitment
	// is made before anything is published, so this is not a node behind.
	return fmt.Errorf("%w: no commitment on the claim", bus.ErrNotAuthor)
}

func agentOf(claims []claim, author Key) bool {
	for _, c := range claims {
		if c.agent == author {
			return true
		}
	}
	return false
}

func keyAt(data []byte, offset int) Key {
	var k Key
	if len(data) >= offset+32 {
		copy(k[:], data[offset:offset+32])
	}
	return k
}

// fields reads a body's string fields; the schemas have already said they
// are there and well formed.
type fields map[string]any

func (f fields) text(name string) string { s, _ := f[name].(string); return s }

func (f fields) key(name string) Key { k, _ := ParseKey(f.text(name)); return k }

// MintDecimals is how many decimal places a mint's amounts carry: byte 44 of
// an SPL mint, the same under Token and Token-2022.
func (p *Program) MintDecimals(ctx context.Context, mint Key) (int, error) {
	a, err := p.rpc.Account(ctx, mint)
	if err != nil {
		return 0, err
	}
	if a == nil || len(a.Data) < 45 {
		return 0, fmt.Errorf("%s is not a mint", mint)
	}
	return int(a.Data[44]), nil
}
