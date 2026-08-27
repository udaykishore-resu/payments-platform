# ADR-018: Money is integer minor units with an explicit currency value object

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §7 (Money), §9 (invariants I1, I2, I4), §19.3 / §23 (wire format) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-004 (ledger); constrains every API and event schema

## Context

Money representation is the kind of decision that is nearly free to make correctly at the start
and nearly impossible to change later: it is embedded in every API contract, every event schema,
every database column, every test fixture and every merchant integration.

The forces:

1. **Binary floating point cannot represent decimal fractions.** `0.10` is not representable in
   IEEE 754; the nearest `float64` is `0.1000000000000000055511151231257827…`. Ten additions of
   `0.1` do not equal `1.0`. In a ledger that must balance to the cent across billions of
   entries, this is not a rounding nuisance — it is a class of defect that produces reconciliation
   exceptions no one can explain.
2. **Currencies have different exponents.** USD has 2 decimal places, JPY has 0, BHD has 3, CLF
   has 4. A representation that assumes "cents" is wrong for a third of the world. "¥1000" and
   "$10.00" are both four-digit minor-unit values with completely different meanings.
3. **Amount without currency is meaningless and dangerous.** `1000` could be $10.00 or ¥1000.
   Adding two amounts of different currencies must be an error, not a silent number.
4. **Gateways speak minor units.** Stripe, Adyen and PayPal card APIs all take integer minor
   units. Every conversion we perform between our representation and theirs is a rounding
   opportunity we do not need.
5. **Splitting must be exact.** Multi-capture, fee allocation and partial refunds split an amount
   into parts that **must** sum back to the whole. Naive per-part rounding loses or creates
   currency.
6. **JSON has one number type** and JavaScript parses it as `float64`. A decimal amount sent as a
   JSON number is at the mercy of the client's parser. `2^53` is the safe integer limit — ample
   for minor units (9 quadrillion) but not something to leave to chance with decimals.
7. **Invariants must be checkable in SQL.** I1 (`sum(refunds) ≤ captured`) and I2 (`captured ≤
   authorized`) are database `CHECK` constraints. Exact integer comparison makes them trivially
   correct; float comparison makes them approximately correct, which for money means wrong.

What breaks if we choose wrong: a ledger that does not balance, discovered during settlement
reconciliation, with no way to determine which of millions of entries drifted.

## Decision

**Money is an immutable value object: `int64` amount in **minor units**, plus an explicit
`Currency`. No floating point anywhere in the money path. Ever.**

```go
type Money struct {  // immutable
    amount   int64   // MINOR units, always
    currency Currency
}
```

Binding rules (§7), enforced by the type system and by tests:

1. **No `float32`/`float64` in any money computation, storage or serialization.** A lint rule
   forbids float types in `pkg/money`, `internal/domain` and the money-path packages.
2. **Amounts are minor units.** The exponent comes from an embedded ISO 4217 table: `USD`=2,
   `JPY`=0, `BHD`=3, `CLF`=4. Formatting for display is a presentation concern that consults the
   table; it is never how the value is stored or transmitted.
3. **Arithmetic across currencies returns `ErrCurrencyMismatch`** — not a panic (which would take
   down a payment worker on a data error) and not a silent conversion (which would fabricate an
   exchange rate).
4. **Splitting uses largest-remainder allocation**, so parts always sum to the whole. Splitting
   1000 minor units three ways yields 334/333/333, never 333/333/333 with one unit vanished.
5. **Wire format is `{"amount": 1050, "currency": "USD"}`** — integer minor units. Never a decimal
   string, never a float. This is fixed in the OpenAPI contracts, the event schemas (§13) and the
   configuration document (§23), and it is the same shape everywhere so there is one thing to
   learn.
6. **Negative amounts are legal only for ledger credit entries.** The API rejects `amount <= 0`
   on payment, capture and refund requests.
7. **Database storage is `BIGINT` plus a `CHAR(3)` currency column**, never `NUMERIC` and never
   `FLOAT`. Amount and currency are always adjacent and always written together (I4 makes them
   immutable after creation).
8. **`Money` is in `pkg/money`**, which is stdlib-only (§4), so it can be published in client SDKs
   and imposes no dependency on consumers.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **`int64` minor units + Currency VO (chosen)** | Exact by construction — no rounding exists to get wrong; matches what every gateway API expects, so zero conversions on the wire; comparisons and sums are trivially correct in Go and in SQL, making the database `CHECK` constraints exact; `int64` holds ±9.2 × 10¹⁸ minor units, which is ~92 quadrillion USD — no practical overflow; JSON-safe as an integer well inside `2^53`; the currency is impossible to omit because it is part of the type | Callers must know the exponent to format for display (mitigated by the ISO table and formatting helpers); large-exponent currencies (CLF, 4dp) consume more of the range, though not remotely near the limit; percentage calculations (fees, FX) must be done with explicit rounding policy rather than falling out of the arithmetic | **Accepted** |
| **Decimal type (`shopspring/decimal`, or SQL `NUMERIC(20,4)`)** | Exact decimal arithmetic including division; natural human representation (`10.50`); handles arbitrary exponents without a lookup table; avoids the "how many decimal places?" question entirely; well-supported libraries | Still requires the currency alongside it, so it does not solve the harder half of the problem; heap-allocated and ~10–50× slower than `int64` arithmetic, which matters when every payment does several operations at 15 000 TPS; introduces a non-stdlib dependency into `pkg/`, violating §4's stdlib-only rule for the package we most want to publish; still needs conversion to minor units at every gateway boundary, reintroducing rounding at exactly the point we are trying to eliminate it; equality and serialization have surprising edge cases (`10.5` vs `10.50` compare unequal in some libraries). This is the option an engineer from a financial-reporting background pushes for, and for a general ledger with FX and interest it would be right — it loses here because our arithmetic is addition, subtraction and exact splitting, and our boundaries are all minor-unit APIs | Rejected for the money path; a decimal type remains appropriate for *rates* (fee percentages, FX rates), which are not `Money` |
| **`float64`** | Fastest; simplest; native JSON number; no library | Cannot represent `0.10`. Sums drift. Comparisons require epsilons, and an epsilon in a refund-limit check is a specification of how much money we are willing to lose. Explicitly forbidden by §7 rule 1, and the single most common root cause of ledger discrepancies in financial systems | Rejected |
| **Decimal string (`"10.50"`) on the wire, parsed internally** | Human-readable; unambiguous in JSON regardless of parser; no precision loss in transport; used by several well-known payment APIs | Every consumer must parse, and parsers disagree on `"10.5"` vs `"10.50"` vs `"1.05e1"`; validation of a string amount is fiddly and error-prone; sorting and comparing require parsing; it invites clients to do decimal arithmetic in JavaScript, which is exactly the float problem one layer removed. An integer needs no parsing contract at all | Rejected |
| **Amount in major units as an integer (dollars, not cents)** | Simplest for humans | Cannot express `$10.50`. Not a serious option, listed because it is the accidental result of a careless schema | Rejected |

## Consequences

### Positive

- Money arithmetic is exact by construction. There is no rounding mode to get wrong on addition
  and subtraction because there is no rounding.
- The database invariants I1 and I2 are exact integer comparisons — the constraint means precisely
  what it says.
- Zero conversion at gateway boundaries: our `int64` is what Stripe, Adyen and PayPal expect.
- Currency cannot be forgotten: `Money` has no constructor that omits it, so there is no
  "amount without currency" value anywhere in the system.
- `Money` lives in stdlib-only `pkg/money` and is publishable in SDKs without imposing
  dependencies.
- Performance: value-type arithmetic with no allocation, which matters on a 15 000 TPS path.

### Negative

- Every display surface must format via the ISO exponent table. A missing or wrong exponent entry
  shows `¥10.00` instead of `¥1000`, and the error is in presentation where it is least tested.
- Merchants and integrators will get this wrong at first — sending `10.50` instead of `1050` is a
  predictable and recurring integration error. The API must reject non-integers loudly and the
  documentation must lead with it.
- Percentage arithmetic (fees, FX, TRA thresholds) needs an explicit rounding policy at each site
  rather than an implicit one. This is more code, but it is code that says what it does.
- The ISO 4217 exponent table is embedded data that must be maintained; currency redenominations
  are rare but real.

### Neutral / accepted costs

- Largest-remainder allocation is more code than naive division and must be tested with
  property-based tests over many splits and amounts.
- Some very-high-exponent or hyperinflated currencies would consume more of the `int64` range;
  still far from the limit, but worth noting when adding a currency.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| A float sneaks into the money path (a JSON tag, a report, an analytics job) | Medium | **High** — silent drift | Lint rule forbidding float types in money packages; `Money` has no float constructor or accessor; JSON marshalling is custom and integer-only; analytics reads the integer column | Lint gate; a reconciliation discrepancy that is a fraction of a minor unit is the diagnostic signature |
| Wrong exponent for a currency | Low | High — 100× amount errors | ISO 4217 table is embedded, unit-tested against the published standard, and covers every currency in `supportedCurrencies`; adding a currency requires a table entry and a test | `TestISO4217ExponentsMatchStandard`; L6 amount-echo validation catches a mismatch against the gateway |
| Merchant sends `10.50` meaning $10.50 | High | Medium — rejected payments, support load | Schema rejects non-integers with a specific error naming minor units; documentation and SDKs lead with the convention; certification (§11.4) includes an amount-echo assertion | `VALIDATION_FAILED` on the amount field by merchant, especially clustered at go-live |
| Splitting loses or creates a unit | Low | High — ledger imbalance | Largest-remainder allocation with property-based tests asserting `sum(parts) == whole` for random amounts and part counts, including exponent-0 and exponent-3 currencies | Ledger imbalance check; property test |
| Currency mismatch handled as a panic | Low | Medium — worker crash on bad data | `ErrCurrencyMismatch` is a returned error by contract; a panic in money arithmetic is a review-blocking defect | Panic counter in the orchestrator; test asserting error, not panic |
| `int64` overflow via a hostile or buggy amount | Low | Medium | API rejects amounts above the merchant's configured maximum long before overflow; arithmetic uses checked addition that returns an error on overflow | Overflow error counter |

## Validation

- **Property tests:** for random `(amount, currency, parts)`, splitting sums exactly to the whole;
  addition is associative and commutative; subtraction and addition round-trip exactly.
- **No-float gate:** static check that no float type appears in `pkg/money`, `internal/domain` or
  the money-path packages.
- **Contract test:** every OpenAPI schema and every event JSON Schema representing an amount uses
  `{"amount": <integer>, "currency": <ISO code>}`. Asserted by a schema lint over `api/`.
- **Round-trip test:** amount sent to a gateway equals the amount echoed back (L6, §21) for every
  adapter in the certification matrix, across a 0-decimal, 2-decimal and 3-decimal currency.
- **The outcome metric:** ledger imbalance. Target: **exactly zero** discrepancy attributable to
  representation or rounding, forever. Any fractional-unit discrepancy is proof this ADR was
  violated somewhere.

## Revisit criteria

Reopen if:

1. We take on a use case requiring sub-minor-unit precision — interest accrual, per-unit pricing
   below the minor unit, or FX intermediate values held at higher precision. The likely answer is
   a *separate* higher-precision type for those computations with an explicit, tested rounding
   boundary into `Money`, not a change to `Money` itself.
2. A supported currency's exponent or redenomination makes the embedded table insufficient.
3. `int64` range becomes a genuine constraint (it will not at any plausible volume, but a
   hyperinflated currency with a 4-decimal exponent is the scenario worth checking before adding
   it).
4. A gateway requires a decimal-string wire format — an adapter-level concern (ADR-011) that must
   not propagate into the domain.
