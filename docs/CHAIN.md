# What goes on the chain, and why

Companion to [PROTOCOL.md](PROTOCOL.md). That document defines the protocol;
this one explains which parts of it Solana holds and what the program looks
like.

The rule the split follows:

> The chain holds what must be **ordered**, **owned**, or **paid**. Everything
> readable stays off it and is committed to by hash.

Readable things are large, are read far more often than they are written, and
gain nothing from consensus. Putting a claim body on chain would cost money per
byte to store something nobody needs the network to agree about. Putting its
hash there costs 32 bytes and makes it impossible to alter afterwards, which is
the only property that was actually wanted.


## The split

| On chain | Why it cannot be anywhere else |
|---|---|
| Colony registry — leader, payment mint, consensus policy | An objective's colony must be canonical, and the policy in force must be a record rather than a claim |
| Objective — vault, awards, deadlines, criteria hash, the policy it was published under, and a URI to fetch the document | Money, a commitment to what was asked, and rules that cannot move once agents are working. The URI is a hint rather than a commitment: whatever it serves is checked against the hash |
| Claim record — hash, agent, slot, the running totals of what validators returned, and what it settled to | **Priority.** The slot is the protocol's only total order. The totals are there so the settled figure can be an average the program computes rather than one a leader types |
| Validation — a structured finding, two numbers, and a hash of the rest | A vote that cannot be cast twice. Weight is recomputed by readers, never carried here (AUDIT A4) |
| Escrow — one token account per agent per objective | An awarded claim's money, out of the leader's reach and collectable without them |
| Token supply, the two edge rates and the colony whitelist | What the protocol issues for the discovery graph, which no colony's leader controls. One rate per end of an edge, and the citer's is bounded by the root's |
| Validation fee pool — one token account per objective | What this objective's claimants put up for its validators, held where neither the leader nor the claimant can take it back once it is earned |

| Off chain | Why the chain would add nothing |
|---|---|
| Objective statement and criteria text | Large, read constantly, committed by hash |
| Claim body — assertion, significance, evidence | The bulk of the data; hash-committed |
| Per-item validation findings and the rationale | A list as long as the claim's evidence. The chain holds their hash, so they cannot be altered without being noticed |
| Artifacts and evidence | Fetched point-to-point, referenced by hash |
| Colony membership and admission | A colony-signed token does this |
| Discovery and the marketplace | The website does this |
| **A statistical interval over the votes** | A way for a reader to weigh a result, not the rule. The program decides by the plain share of passes against the thresholds, which it tallies as the votes arrive; an interval is a pure function of the same votes, so anybody can compute one without the program |
| **Standing** | Derived from settled outcomes. A score that is a function of history does not need to be stored, and storing it would mean paying to maintain something recomputable |

The last two are the ones worth arguing about, so they are argued in full.

**The interval.** The program decides by the share of validators that passed
a claim, against the two thresholds, once at least the minimum voted. It keeps
no confidence level and computes no interval. Any node can run Wilson over the
same votes and get the same interval, as a measure of how much a result is
worth; running it on chain would burn compute units for a number that is
already determined by data the chain holds.

**Standing.** Standing is a fold over resolved claims and the validations
under them, which are on chain and permanent. Anyone can recompute it from history and reach the same number, so
it is verifiable without being stored. Storing it would mean an instruction to
update it, a decision about who may call that instruction, and a value that can
drift from the history it claims to summarise.


## Program layout

One program, `chain/programs/unloop`.

    src/
      lib.rs               entry points only — each is one line
      constants.rs         seeds and structural limits
      error.rs             every failure the program can report
      state/               the accounts, and the arithmetic over them
        config.rs          fee split
        colony.rs          trust domain and consensus policy
        objective.rs       awards, balances, the withdrawal rule, and what
                           an admitted claim is owed
        claim.rs           priority, roots, the range the validators
                           returned, and what the claim settled to
        validation.rs      one structured finding
        token.rs           the supply, the rates, the colony whitelist
      instructions/        handlers, grouped by subject

**Where the arithmetic lives.** Money rules are methods on the state types, not
code in the handlers. `Objective::withdrawable` is the only implementation of
"what may the leader take back", and every handler that touches the vault asks
it rather than working it out. `Award::due` is the only implementation of what
an admitted claim earns. `Config::split` is the only place a fee is divided,
and `TokenConfig::issue` the only place a token is created. A handler checks who is asking, asks the state
what is permitted, and writes the result.


## Accounts

Every account is a PDA. Nothing is addressed by an id the caller chooses.

    Config        ["config"]
    Colony        ["colony", slug]
    Objective     ["objective", colony, index]
    Vault         ["vault", objective]              token account, objective-owned
    Escrow        ["escrow", objective, agent]      token account, its own authority
    Claim         ["claim", objective, claim_hash, agent]
    Validation    ["validation", claim, validator]
    Validator     ["validator", colony, address]
    Bond          ["bond", colony, address]        token account, its own authority
    TokenConfig   ["token"]
    Mintable      ["mintable", colony]
    Fees          ["fees", objective]               token account, its own authority
    Buyback       ["buyback", payment_mint]        token account, its own authority

Five of those seeds do work beyond addressing:

**A `Claim` account is never closed.** Its rent deposit — 0.003905 SOL for 433
bytes, which `chain/layout.py` derives from the Rust rather than anyone
retyping — is what a claim costs, and a cost that could be reclaimed would not
be one. There is no separate bond; permanence is the commitment.

**`Claim` is keyed by the claim hash and the agent**, so an agent cannot
file the same thing twice and nobody can file in another's place. It was the
hash alone, which made the hash the property of whoever filed it first, found
or not: where an objective's answers can be listed, anybody could file every
candidate before the work was done and lock the finder out for good — a leader
included, against its own prize, getting its validation fees back because
nothing was evaluated. Two agents filing the same discovery now both hold a
claim, and what was already built decides between them: a winner-take-all
prize goes to the earliest admitted, and a later copy is no novelty. The hash
is still taken over the *canonical form*, which is what makes the two copies
recognisably the same discovery. A claim's roots name the claims it cites by
account address, since a hash no longer names one claim.

**`Validation` is keyed by the validator**, so nobody votes twice, and one
identity cannot hold two commitments and open whichever turned out better.
Keying it by the claim as well makes the findings enumerable: one filtered request
returns every validation of a claim, which is how a reader recomputes what the
validators settled without trusting anybody's summary of it.

**`Escrow` is keyed by the agent**, so an award has exactly one place it can
arrive. There is no recipient field anywhere in the payment path: paying the
wrong party would need an address that does not exist. It is also why
collection needs no guard against happening twice — the balance goes to zero,
and a second attempt has nothing to move.

**`Fees` is keyed by the objective and not by the claim**, so the validation
fees of every claim in an objective sit in one account. Per claim would be
exact, and would add a token account's rent to every claim filed — a permanent
cost on filing, to hold a balance that is spent within days. What keeps the
claims separate without separate accounts is that each one stores the fee it
paid, so its share is arithmetic rather than a balance to look up.

**`Validator` is keyed by the address**, so a colony's eligible set is
enumerable and an identity has exactly one record in it — which is what
random selection selects from and what an attacker has to multiply.

**The treasury token account is bound to `config.treasury`** by an ownership
constraint. Without it, every collector could name their own account as the
treasury and keep the fee — which the first compiled version allowed.


## Instructions

    initialize_config       set the fee split and treasury. only the
                            program's upgrade authority may call it
    update_config           change it without redeploying

    create_colony           leader, payment mint, consensus policy, and who
                            may validate here: admission, its authority, the
                            bond, what one validation costs, what entry
                            costs, and how many answers a new validator
                            gives before they count. refused where the
                            policy cannot decide, or a public colony asks
                            for no bond
    update_colony           change the policy, under the same checks
    set_validation_fee      reprice validation. cannot reach a claim already
                            filed, because that claim stores what it paid
    set_bond                reprice the bond, for validators joining from
                            now on. the leader only, and never to zero. each
                            validator keeps, and gets back, what it posted

    submit_entry_proof      answer one of an objective's entry proofs and let
                            yourself in. no authority signs it: the program
                            checks the answer. public colonies only, and
                            refused where the colony has closed that door
    set_trusted             the admission authority marks a validator already
                            in the queue trusted, or unmarks it. trusted
                            validators are picked for every claim; the
                            random pick is drawn from the rest
    revoke_qualification    a private colony's admission authority removes a
                            validator, for any reason. refused in keyed and
                            public colonies, where only a proof removes one.
                            not a slash — the bond is still the validator's
    record_absence          record that a validator was selected for a claim
                            and never committed. permissionless; three in a
                            row suspend it, bond and trust intact, and a
                            commitment resets the count
    rejoin                  a suspended validator back in the queue, sent by
                            itself: no new test, no new bond
    qualify                 the admission authority records that an address
                            passed this colony's test. the chain does not
                            judge the test — it is domain work, checked off
                            chain, and this is the attestation
    post_bond               put the colony's bond at risk. a fixed deposit,
                            not a stake: every bonded validator counts once
    withdraw_bond           take it back and stop being eligible. not while a
                            commitment is sealed. the record stays, because
                            the history on it is what standing is computed
                            from
    slash_bond              burn the bond of a validator that committed and
                            never opened it, and remove it from the colony
                            for good. permissionless, because nothing
                            about it is a decision, and burned rather than
                            paid to anybody, because a slash that enriched
                            someone would give them a reason to manufacture
                            faults
    select_validators       fix which validators a claim selects, from the
                            hash of the tenth block produced after the claim's
                            own. counted in blocks, so a skipped slot is not
                            one; and there is no substitute for that block, so
                            the answer does not move with when the call is
                            sent. permissionless, and the claimant is the
                            one with a reason: it cannot announce the list
                            until the seed exists. a validator's first
                            commit fixes it too, so nothing waits

    create_objective        fund the vault, fix the awards and deadlines,
                            and say where its document can be fetched
    set_objective_uri       repoint that. safe to change because it was
                            never trusted: the commitments are the hashes
                            and they do not move
    top_up_objective        a bounty may always grow, from anybody
    withdraw_unencumbered   take back only what no claim could win
    close_objective         stop taking claims; pending ones still settle

    record_claim            take your place in line (rent is the cost);
                            every declared root is passed as an account
                            and checked: same objective, admitted, earlier.
                            carries the most its author agreed to pay for
                            validation, so a repricing cannot be dropped in
                            front of a filing already signed. an ongoing
                            objective refuses a claim its unreserved funds
                            cannot cover
    commit_validation       the hash of a finding, and nothing else. reading
                            a claim costs something and copying the validator
                            before you costs nothing, so during the commit
                            window there is nothing to copy
    reveal_validation       open it. the claim and the validator are in the
                            hashed input, so a commitment lifted from somebody
                            else cannot be opened by whoever copied it. only
                            here do the two numbers fold into the range
    resolve_claim           settle a claim. waits for every commitment to be
                            opened or for the window to close, because an
                            unopened one could still move the numbers.
                            supplies no score: where enough validators
                            answered it is their average, and the verdict is
                            the share of them that passed it against the
                            thresholds the objective published. the
                            settlement authority only, until the validation
                            deadline — after that anybody, where the
                            validators settled it, and a day later anybody,
                            where they did not, as a rejection. who decided
                            is worked out, not passed. only the authority
                            confirms roots. confirmed roots are a bit
                            per declared root
    expire_claim            reject a claim whose seed was never fixed while
                            the chain still held the block that seeds it, so
                            no validator can ever be selected for it.
                            permissionless, and it decides nothing: the
                            condition is a fact about the chain's own history
                            and the only outcome is rejection. refused while
                            the claim can still be seeded, or once it has
                            been. the fee comes back through
                            refund_validation_fee; the rent does not, because
                            the rent is what filing cost

    award_claim             pay one admitted claim into its agent's escrow.
                            anybody may send it. takes no amount — the rate
                            is the award's allocation against the claim's own
                            settled figures — and no recipient, because the
                            escrow's address derives from the claim's agent.
                            so the agent can pay itself and waits on nobody.
                            a winner-take-all prize pays only its earliest
                            admitted claim, by filing slot, once nothing that
                            might be earlier is still pending
    collect                 drain your own escrow, fee deducted here and
                            split the way the two shares were declared:
                            the treasury's, and the buyback's own account

    initialize_token        the supply cap, fixed here and never raised.
                            only the program's upgrade authority may call it
    set_edge_rates          what one confirmed edge pays at each end. the
                            citer's share is bounded by the root's, and the
                            program enforces the ordering
    set_colony_mintable     whether a colony's work can mint at all
    mint_edge_reward        pay both ends of one confirmed edge: the root's
                            agent for being worth building on, the citer for
                            saying so. anybody may call it, and the caller
                            chooses nothing

    collect_validation_fee  a validator takes its share of what the claimant
                            put up. an equal split among the validations that
                            counted — not minted, moved
    refund_validation_fee   the claimant takes back a fee no validation
                            earned

There is no dispute instruction, because the protocol has no dispute process.
There is no `pay` instruction, because paying is not something anyone performs
on somebody else's behalf: every payment in this list is pulled by the party
it belongs to.


## Events

Every state transition emits one, declared together in `events.rs` rather than
beside the handler that fires it, so the whole surface an indexer has to handle
can be read in one place. Twenty handlers, twenty emit sites.

Each event names the account it concerns rather than inlining it. A Solana log
is bounded and paid for by the transaction, and the account is already readable
at that address — so an event carries the key plus the fields an indexer cannot
cheaply derive, and nothing more. Two carry a little extra for a reason:

- `ObjectiveWithdrawn` reports the remaining **encumbered** figure as well as
  the amount taken, because what matters to a watching agent is not what left
  but what is still beyond reach.
- `ClaimRecorded` reports `claim_slot`. That is the protocol's only total
  order, so an indexer reading these in log order is reading priority order.

- `ClaimAwarded` reports the score and the novelty the amount was computed
  from, so an indexer can check the arithmetic without fetching the claim back.

Not every event carries enough yet. `ColonyCreated` omits the consensus policy
altogether and `ObjectiveCreated` omits the two hashes it commits to, so an
index built by reading the log has to stop and fetch. Nothing is lost — both
sit in the accounts and in the instruction data — but the events an index is
built on are part of the interface rather than a convenience on top of it.


## Reading the graph

**Every question is answerable directly from the chain. The database is a
cache and nothing depends on it.**

All state lives in accounts, and accounts persist forever — so nothing needs a
log stream, a retention window, or an indexer to be correct. An RPC filter on a
known byte offset answers each of these:

    claims for an objective     filter `objective`   at offset   8
    claims by an agent          filter `agent`       at offset  40
    one claim by its hash       derive its address — no filter needed
    which claims cite P1        filter each root slot at offsets
                                114, 146, 178, 210, 242, 274, 306, 338
    every validation of a claim filter `claim`       at offset   8
    what an agent is owed       derive its escrow — no filter needed
    every validation by a node  filter `validator`   at offset  40

The last one is why `roots` is a fixed `[[u8; 32]; 8]` and not a `Vec`. A Vec's
entries sit wherever the preceding ones left off, so no fixed offset holds "a
root" and the reverse question degrades to fetching every claim and checking
each. Fixed slots put root N at a known byte, so the same question is eight
ordinary filtered queries. It is also three bytes smaller.

An indexer is still worth running — it makes these fast, sorts and aggregates
them, and serves a website. But it is an accelerator over queries anyone can
make themselves, not a component the protocol needs. If it disagrees with the
chain it is simply wrong, and if it disappears every agent keeps working.

Standings are the same: a fold over resolved `Claim` accounts and the
`Validation` accounts under them, all of which persist, so they are
recomputable by anyone at any time from state alone.

One caveat, and it is in the account layout rather than in the idea. `Claim`
ends with two `Option` fields, and borsh writes `None` as one byte and `Some`
as more — so everything after an `Option` moves when it is filled in. Both are
last for that reason, which leaves every fixed-width field above them,
`roots` included, where a byte-offset filter expects it.


## The two rules the program exists to enforce

**Encumbrance.** `Objective::encumbered` is the sum of two things: what an
open objective has promised, and what a claim that is owed something could win
— one still under judgement, or judged good and not yet paid. The leader may
withdraw the balance minus that, and nothing else.

There used to be a third term, for money written into a settlement and not yet
collected. It is gone because that money is no longer in the vault: awarding a
claim moves the tokens into the agent's escrow, so there is nothing left in the
vault to hold back for it, and closing the objective cannot reach it.

A delay cannot do this job. If withdrawal took six hours, a leader who wanted
to avoid paying for a published result would start the clock and wait six
hours, and an honest leader correcting a funding mistake would also wait six
hours. The two are separated by whether the money is already spoken for, not by
how long anybody waits — and the encumbrance rule also returns an honest
leader's surplus immediately, which a delay does not.

**Pull, not push.** `award_claim` moves an admitted claim's money into that
agent's escrow; `collect` moves it out, and the agent signs it. A push to many
winners hits transaction size and compute limits, and an earlier design made
that worse by writing payments in batches of twelve — one claim into one
escrow has no list to bound at all. Each agent pays its own fee, and the leader
is out of the payment path from the moment the claim is paid rather than from
the moment somebody gets round to collecting.


## Status

Compiled with Anchor 1.2.0 on platform-tools v1.54, IDL generated, and run
under LiteSVM 1.4 by a 65-test attack suite that passes; seventeen money-carrying
fixes are mutation-checked. See [AUDIT.md](AUDIT.md) for what was found and
changed between the first compiled version and this one. Not deployed.

    cd chain && ./build.sh      # SBPF v3; fails where cargo build-sbf does not
    ./build-idl.sh && npx vitest run
