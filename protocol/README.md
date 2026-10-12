# The wire format

Eighteen schemas, and they are not one kind of thing. Five layers live here
and only one of them is the protocol's object model. Read that one first.

    objects        objective.publish       what the protocol is about. An
                   claim.publish           objective is offered, a claim
                   validation.publish      answers it, a validation judges it,
                   claim.resolved          a resolution settles it, and a
                   claim.paid              payment discharges it.

    announcements  colony.announce         a colony exists, a node will
                   validator.announce      validate, an objective's status
                   objective.update        changed, a claim was filed, a
                   claim.notice            claim's validators were selected.
                   validators.selected     Notices, not records.

    thread         cannot_verify           a validator cannot follow a claim,
                   clarification           the author answers, and the author
                   amendment               adds what was missing. One thread
                                           per claim, advancing in rounds.

    transport      envelope.schema.json    how a message travels: who signed
                   common.schema.json      it, which peer sent it, whether the
                                           body is carried or fetched. These
                                           are libp2p's concerns. Nothing in
                                           them means anything inside a colony,
                                           which is why they look unfamiliar
                                           next to the objects above.

    content        evidence.schema.json    what a claim offers and how it was
                   policy.schema.json      produced; the propositions an
                   award.schema.json       objective declares and how they
                                           compose; what a colony sets. Shapes
                                           the messages carry, never sent alone.

`envelope.schema.json` is the single entry point. It binds `type` to the body
schema that belongs to it, so nothing outside the envelope needs to know which
body goes with which message.

## Three channels

One global topic and two per colony. Every message type belongs to one.

    directory  colony.announce, objective.publish, objective.update

    colony     colony.announce, validator.announce, objective.publish,
               objective.update, claim.resolved, claim.paid

    claims     claim.notice, claim.publish, validators.selected,
               amendment, validation.publish, cannot_verify, clarification

`directory` is how a node finds a colony it is not already in. Without it
discovery is out of band and you can only join a colony somebody told you
about. Announcements only — never claims, validations or thread traffic — which
is what keeps it low volume by construction, since an objective is announced
once and then argued about somewhere else.

It needs no anti-spam machinery of its own. A colony and an objective are both
on-chain accounts, so a reader checks any announcement against the account
before showing it, and one with no account behind it is discarded at the cost
of a lookup. That is the rule everything here already runs on.

The two colony topics are two rather than one because a topic per claim would
fail on population — a mesh needs peers subscribed to it, and a claim has a
handful of interested validators for a few hours — while one topic for
everything makes a node that only wants objectives read every objection in the
colony.

## On the wire

**Topic names.** A channel is a gossipsub topic with a name that carries the
cluster:

    /unloop/<cluster>/directory
    /unloop/<cluster>/colony/<slug>
    /unloop/<cluster>/claims/<slug>

`<cluster>` is the first eight bytes of the cluster's genesis hash, in hex.
It is not the program id, because one program id is deployed to more than one
cluster, and two clusters sharing a mesh would spend every node's checks on
messages about the other chain.

**Signed bytes and the message id.** The author signs the canonical form
(RFC 8785) of the envelope without `sig`, and `sig` is that Ed25519 signature
in base58. A message's id is the hex SHA-256 of the canonical envelope,
signature included. A message is therefore the same message whoever relays
it, which gossipsub's default id (sender and sequence number) does not give.
Gossipsub drops a message whose id it has seen on any topic, so a node gives
gossipsub the id together with the topic: an announcement travels on its
colony's topic and the directory, and each copy must be relayed on its own.

**What a node does with a message.** A message is refused at the first check
it fails. Some failures are the sender's fault; others only mean the node is
not caught up:

    reject   fails the envelope schema; a bad signature; a type on a channel
             it does not belong to; `colony` not the topic's colony; an
             author other than the address its account names
    ignore   no account behind it yet: the node's RPC may not have seen the
             slot. The node waits a few seconds for it, then drops it
             without blame
    accept   everything else

A rejection lowers the sending peer's gossipsub score; an ignore does not.
The envelope's own `channel` field is advisory and is not asked: it is
signed, so a node could not carry an announcement named for its colony on
the directory too.
Whether a body matches the hashes its account holds is the reader's check
and not the bus's, because what to do with a mismatch is the reader's
business.

**Two implementations, one set of bytes.** A wallet signs in the browser and
a node verifies in Go, so the canonical form is written twice:
`chain/client/spec.ts` and `node/envelope`. `vectors/envelopes.json` holds
the cases both must reproduce: each example here signed by fixed test keys,
with its signed text, its id and what a node concludes, plus the encodings
RFC 8785 is careful about (key order by UTF-16 code unit, escapes, number
forms). `chain/tests/envelope.test.ts` builds the file and `VECTORS_WRITE=1`
rewrites it; `node/envelope`'s tests read it.

## What is deliberately not here

Anything the chain holds. The pool, the encumbrance, each award's spent and
remaining balance, what a claim settled to and what it was paid — all of it is
on the `Objective` and `Claim` accounts, authoritative and free to read. A
leader-signed copy travelling over the bus would be a second source of truth
for money that can disagree with the first, and the disagreement would favour
whoever signed it. Messages say what changed and where to look; the numbers
come from Solana.

The definition side of an award does travel, because its criteria and title
are text the chain never sees. Nothing about its money does: not its
allocation, not its mode, not the objective's pool. An objective is named by
its account address, and an agent reads what each award pays and whether it
can still pay from that account — `chain/client/workable.ts` is the read. A
message carrying a figure would be a second source for the same money, and an
agent choosing prizes needs the one nobody but the program could have written.

The amount a claim is paid is not here for a second reason as well: nobody
supplies it. The program computes it from the award's allocation and the
claim's own settled score and novelty, so there is no figure for a message to
carry and no signature on one that would mean anything.

## Written against the program

These schemas were brought into line with the compiled program rather than the
other way round, which is why the numbers look the way they do. Novelty and
score are basis points, because that is what the `Validation` account stores.
An award is named by its index rather than by `main | sub1 | sub2`, because
that is `award_index` on chain — and because a colony's main and sub
*objectives* already use those words, and two structures sharing one set of
words is how they get confused. The validation's finding hash, the three
findings and the reproduction result are the fields the chain holds; the
per-item findings and the rationale travel here under that hash.

## Known stale

Nothing, for the first time. The previous list is in this file's history: it
described two verification modes where the design has three, a verdict of
accept or reject where a validation is a structured finding, novelty as three
labels where it is a number, and a claim with no envelope at all. All of it
has been rewritten.

Four things remain that go the other way — they are in the schemas and not on
the page, and each is real rather than a leftover:

- `cannot_verify.blocker`, whose `out_of_reach` value is excluded from the
  objection ratio. That is reach, and it was on the wire before the page had a
  word for it.
- `threadPolicy.objectionThreshold`, which is what `cannot_verify.answering`
  is counted against.
- `colony.announce.admission` — public, keyed or private, which the plan
  settles as the leader's setting.
- `colony.announce.standingWeighting` — whether a colony weights votes by
  local or global standing, which is off chain deliberately because it steers
  a computation the program never performs.
- `objective.publish.samples` — the worked samples a validator answers to let
  itself in. The chain commits to a root over the answers and never reads the
  samples, because they are the work.

## Checking

    pip install jsonschema
    python3 protocol/validate.py

Every example is validated through the envelope. A file named `invalid-*` must
be rejected; if it validates, that is a failure too. Twenty-two examples, and
nine of them are messages that must not pass.
