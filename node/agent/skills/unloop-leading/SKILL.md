---
name: unloop-leading
description: Drafting an Unloop objective with the person — what to ask for, how it will be judged and paid — and handing it to the site to sign or creating it from this agent's wallet
---

## When to Use

The person wants to set an objective, post a bounty or a prize, fund a
search, or asks how to ask Unloop for something.

## Planning Guidance

1. Find out what they want found, and how anybody would know it was found.
   That second answer decides the validation mode: a fixed procedure that
   measures it is deterministic; a method the claimant must supply is
   procedural; judgement against stated conditions is semantic.
2. Choose the colony with them (unloop_colonies): the objective is judged by
   its validators and paid in its currency, and only its leader creates
   objectives in it.
3. Write it so a stranger could claim it and a validator could check it: a
   plain statement, the claim format and where it is defined, and for a
   semantic or procedural objective the predicates — required ones a claim
   must meet, scored ones that set what it earns.
4. Money and time: a prize is paid once per award and must fit in the pool; a
   rate pays each discovery until the pool runs out. Claims close, then
   commitments, then validation; the defaults are two, three and four weeks.
5. Draft it with unloop_draft_objective and show the person the document,
   what it pays, and both hashes: those are what every claim is judged
   against, and nobody can change them once it is created.
6. If this agent's wallet leads the colony, offer unloop_create_objective,
   which waits for the person on the approvals page. Otherwise give them the
   draft's link: it opens the site's create page filled in, where their
   browser wallet signs, and the page says whether the hashes are still the
   ones shown here.

Never make the terms vaguer to make them easier to claim: a claim no
validator can check fails, and so does an objective nobody can claim.
