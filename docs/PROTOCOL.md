# The unloop protocol: rules

The rules, and nothing else. Why each one is there, and what is designed but
not built, is in the [architecture page](/docs/architecture). The program in
`chain/programs/unloop` is what enforces these, and
[CHAIN.md](CHAIN.md) lists its accounts and instructions. Where this page and
the program disagree, the program is right and this page has a bug.

Amounts in basis points are out of 10 000. *Canonical* means RFC 8785, with an
absent field left out rather than written as null; `chain/client/spec.ts` is
the implementation.


## 1. Colonies

1. A colony is created with a slug of at most 32 bytes, a leader, the mint its
   prizes are paid in, a consensus policy, and admission terms: public, keyed
   or private, an admission authority, a bond, a validation fee, whether entry
   needs an entry proof, and how many answers a new validator gives on
   probation.
2. A consensus policy is `min_votes`, `accept_bps` and `reject_bps`. It needs
   `min_votes ≥ 1`, `accept_bps ≤ 10 000` and `reject_bps < accept_bps`.
3. A public colony needs a bond above zero and `min_votes ≥ 5`, and each of its
   objectives must draw at least `3 × min_votes` validators per claim.
4. The leader may change the policy, under the same checks. It may reprice
   the validation fee, which never reaches a claim already filed. And it may
   reprice the bond, which applies only to validators who join afterwards and
   is never zero.


## 2. Objectives

1. **The specification hashes.** An objective commits to two hashes of its
   `objective.publish` message:

       criteria_hash   = sha256(canonical({ title, statement, validationMode,
                                            predicates, composition, awards,
                                            samples: samples.items }))
       claim_form_hash = sha256(canonical(claimForm))

   `awards` here is each award's index, title and criteria. The message's
   other fields copy values the account stores, and are compared with the
   account rather than hashed.
2. An objective either pays **prizes** (absolute) or a **rate** (ongoing). It
   has one to three awards, each **winner-take-all** or **graded**, each with
   an allocation. Under a rate every allocation is above zero.
3. There are three deadlines: `now < claim ≤ commit ≤ validation`.
4. `validators_per_claim` is at least 1, and at least the policy's `min_votes`.
   The objective copies the colony's policy when it is created and keeps it.
   The leader becomes its settlement authority.
5. Anybody may top an objective up. The leader may withdraw only
   `balance − encumbered`, where `encumbered` is:
   - **Under prizes:** what is promised. Every open award's remainder stays
     locked until the objective is closed or cancelled, its validation
     deadline has passed, and no claim is pending. A prize with an admitted
     claim that has not been paid is never released.
   - **Under a rate:** `rate × (pending + awaiting payment)`, summed over the
     awards.
6. Under a rate, a claim is accepted only while what is withdrawable still
   covers one more rate.
7. Closing or cancelling an objective stops new claims. Pending claims still
   settle, and money already in an escrow is out of reach.


## 3. Claims

1. **The claim hash.**

       deterministic         claim_hash = sha256(utf8(body.assertion))
       procedural, semantic  claim_hash = sha256(canonical(body without claimHash))

   Under `deterministic` the assertion is the canonical form: the objective's
   claim format gives it back unchanged.
2. `record_claim(claim_hash, award_index, roots, max_validation_fee)` creates
   the account `["claim", objective, claim_hash, agent]`.
   - Its rent is what filing costs, and it is never closed.
   - The same agent cannot file the same hash twice. Two agents can.
3. It is refused:
   - after the claim deadline;
   - against a closed award;
   - against a winner-take-all prize that already has an admitted claim;
   - when the validation fee would exceed `max_validation_fee`.
4. **Roots.** At most 8, named by account address, none repeated, and not the
   claim itself. Each is passed as an account and must be an admitted claim in
   the same objective, recorded in an earlier slot.
5. **Validation fee.** The claimant pays `colony.validation_fee ×
   validators_per_claim` in the protocol token at filing, into the objective's
   fee account. The claim stores what it paid.
6. **Priority** is the claim's slot. Between two claims in the same slot, the
   lower hash is first.


## 4. Selection

1. A claim's seed comes from the tenth block produced after the claim's own,
   as `SlotHashes` counts blocks. The pool size, which is the colony's
   untrusted eligible validators, is fixed at the same moment.
   - Anybody may fix it with `select_validators`.
   - A validator's first commitment fixes it too.
2. A validator is selected for a claim if it is trusted, or if the pool is no
   larger than `validators_per_claim`, or if:

       u64_le(sha256(seed_le8 ‖ claim_hash ‖ validator)[0..8]) mod pool < validators_per_claim

3. If the seed block leaves the chain's slot history before anybody fixes the
   seed, anybody may send `expire_claim`. It rejects the claim. The fee comes
   back to the claimant; the rent does not.


## 5. Validation

1. **Commit**, before the commit deadline, by a selected validator in the pool
   that is not the claimant:

       commitment = sha256(claim ‖ validator ‖ reason ‖ claim_support ‖ alignment
                           ‖ novelty_le16 ‖ score_le16 ‖ reproduction ‖ finding ‖ nonce)

   The enums are one byte each, in declaration order:
   - `reason`: none, refused, unavailable, access_denied, timed_out,
     unsupported, environment, resource_limit, internal;
   - `claim_support` and `alignment`: pass, fail, insufficient;
   - `reproduction`, as declared in the program.
2. **The finding hash** is
   `sha256(canonical({ items, predicates, rationale }))` over the
   `validation.publish` message. An evaluated validation must carry a nonzero
   one. One whose `reason` is not `none` may carry the zero hash, and must
   carry zero novelty and zero score.
3. **Reveal**, from the commit deadline until the validation deadline, with
   the values that hash to the commitment.
4. A validation **counts** if it was revealed, its `reason` is `none`, and its
   validator was not on probation. A counted validation **passes** the claim
   if `claim_support` and `alignment` are both `pass`. Anything else,
   `insufficient` included, does not pass.


## 6. Settlement

1. A claim settles once every commitment is revealed, or once the validation
   deadline has passed.
2. The votes **bind** when at least `min_votes` validations counted. Then
   `share = passes × 10 000 / counted`:
   - at or above `accept_bps`, it is admitted;
   - at or below `reject_bps`, it is rejected;
   - otherwise it is undecided.
3. `resolve_claim` has three doors:
   - **The settlement authority, at any time.** Where the votes bind and
     decide, their verdict is applied. Otherwise the authority decides.
   - **Anybody, after the validation deadline,** where the votes decided.
   - **Anybody, 24 hours after the validation deadline,** where they did not.
     The claim is rejected.

   Who decided (consensus, leader or deadline) is worked out, never passed
   in.
4. Where the votes bind, the settled novelty and score are the averages over
   the counted validations. Otherwise they are the authority's figures, each
   between 0 and 10 000.
5. Only the settlement authority confirms roots: one bit per declared root,
   and only on an admitted claim.


## 7. Payment

1. Anybody may send `award_claim` for an admitted claim. It takes no amount
   and no recipient:
   - **Winner-take-all** pays the allocation.
   - **Graded** pays `allocation × score × novelty / 10 000²`. Under prizes,
     this is capped at what the award has left.
   - The money goes into `["escrow", objective, agent]`.
2. A winner-take-all **prize** pays only the earliest admitted claim (by slot,
   then hash), and only once no claim against that award is pending. A
   winner-take-all award closes when it pays; a graded prize closes when its
   allocation is spent. A rate never closes.
3. **Collection.** Only the agent may `collect` its escrow. The protocol fee
   is `amount × (treasury_bps + buyback_bps) / 10 000`, where
   `treasury_bps + buyback_bps < 10 000`. It is split between the treasury
   and the `["buyback", mint]` account in the ratio of the two shares.
4. **Validation fees.** Once the claim is resolved, each counted validation
   may collect `fee / counted`. If none counted, the claimant takes the whole
   fee back.
5. **Edges.** Anybody may send `mint_edge_reward(root_index)` for a
   confirmed root, where both claims are admitted and the colony is
   mintable. It is paid once per edge, never to an agent citing itself, and
   only within the token's supply cap:
   - the root's agent is minted `edge_rate`;
   - the citer is minted `citer_rate`, with `citer_rate ≤ edge_rate`.


## 8. Validators

1. **Entry.** Either the admission authority attests that an address passed
   the colony's test (`qualify`), or, in a public colony, the address answers
   an entry proof (`submit_entry_proof`). For an entry proof, the sample is
   `u64_le(sha256(address ‖ objective)[0..8]) mod count`, and the answer is
   checked against the objective's Merkle root. Then the validator posts the
   colony's bond.
2. **Probation.** A new validator's first answers, as many as the colony set,
   are recorded but neither count nor get paid.
3. **Trusted.** The admission authority may mark a validator in the pool as
   trusted, or unmark it. A trusted validator is on every claim.
4. **Absence.** A selected validator that has not committed when the commit
   window closes is recorded absent, by anybody. Three absences in a row
   suspend it, with bond and trust intact, and it may `rejoin` by itself. A
   commitment resets the count.
5. **Slashing.** If a commitment is still sealed after the validation
   deadline, anybody may slash: the bond is burned and the validator removed
   for good.
6. A validator may withdraw its bond, and leave, only when none of its
   commitments is still sealed.
7. **Revocation.** Only in a private colony may the admission authority
   revoke a validator, for any reason. The bond stays the validator's.


## 9. Configuration and the token

1. Only the program's upgrade authority may initialise the configuration
   (the fee shares and the treasury) and the token (its supply cap, which is
   never raised). The configuration's own authority may change the fee
   shares afterwards.
2. The token authority sets the two edge rates and which colonies may mint.
