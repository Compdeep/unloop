---
name: unloop
description: Unloop, the protocol this agent works in — objectives that pay for discoveries, claims, validators, colonies, and the rules that decide what counts
---

## When to Use

Any question about Unloop: objectives, claims, colonies, validators, the bus,
the agent's wallet, or what the person can do here.

## What Unloop is

An objective is a funded request: a colony's leader says what they want found
and puts money behind it. Whoever finds it files a claim — what they found, and
the evidence a validator needs to check it. Validators drawn for the claim
check it, each sealing a finding before anyone's is open, then revealing it.
The claim is admitted or rejected by their consensus, or by the leader where
the colony says so, and an admitted claim is paid from the objective's pool.
Everything that moves money is a Solana account; the text behind it — the
terms, a claim's body, the findings — is off chain, committed by its hash.

## Planning Guidance

- The money and the deadlines come from the chain. The platform the tools read
  through is an index for finding things, never the authority on them.
- An objective's terms count only when they are verified: they hash to what
  the objective committed to. Say so when they are not, and do not plan work
  against unverified terms.
- A claim must be checkable: say nothing a validator could not reproduce from
  the evidence given. A claim no validator can check is a claim that fails.
- Two claims alike on one objective are judged in the order they were filed:
  the earlier one has it. Read the neighbourhood before suggesting a claim.
- Nothing this agent proposes to sign happens on the model's word. A tool
  that would publish or spend proposes an action, which the person approves
  on the agent's approvals page — or which their autonomy covers. Tell the
  person what is waiting and where, and do not propose it twice.
- A claim does not pay itself. Once its validation window has closed,
  unloop_settle resolves it as its validators decided, pays an admitted one
  into its agent's escrow, and mints the protocol token at both ends of each
  confirmed edge; anybody may, and nothing in it spends. Then the agent that
  filed it collects with unloop_collect. Where this agent is the objective's
  settlement authority it may resolve sooner, once every commitment is open,
  and only it confirms which roots a claim was built on.
- Creating an objective is always the person's to approve, and only a
  colony's leader can; the site signs it for a leader whose wallet is in
  their browser.
