# Firebird SQL Parser — Go Library Requirements

Requirements and API specification for the statement-classification library required by the MCP server design. Origin: the open implementation question in [`phase4_plan.md`](phase4_plan.md) §3 (P4.1 T1 / ADR-019), consumed by the K6 kernel service (see [`mcp_implementation_plan.md`](mcp_implementation_plan.md) §3 registry), adversarially bounded by claim C1 in [`phase6_plan.md`](phase6_plan.md) §2.

---

## source of truth

E:\Projects_2026\firebird\src\dsql - firebird sql parser in C++

## 1. Purpose

The library turns Firebird SQL text into a **structured classification**: what kind of statement this is, what it targets, and what it (heuristically) affects. Its outputs drive:

1. **Tier assignment** — verb × object-type mapped to a v3 operation row → tier/risk/impact (the mapping itself stays server-side; the library provides the vocabulary).
2. **Previews (dry-run)** — affected objects, WHERE extraction for row estimates, reversibility class (ADR-021 dual-mode contract).
3. **Statement-list handling** — correct splitting of scripts including PSQL bodies; tier-mixing detection; EXECUTE BLOCK detection.
4. **Defense-in-depth** — non-SELECT/WITH refusal on the read path; guard rails on the write path.
5. **Audit templating** — literal-scrubbed statement templates for the audit log (P1.4).

## 2. Scope — what this library is and is not

**It is NOT a safety component.** Safety comes from the engine (read-only TPB), the human gate, and the gated admin pool. A wrong classification degrades preview *quality* — never the safety boundary (claim C1). The library must therefore optimize for *honesty about uncertainty* (confidence + issues) over guessing.

| In scope | Out of scope |
|---|---|
| Lexical analysis: literals, quoted identifiers, comments, terminators | SQL validation or error diagnosis (the engine's job) |
| Statement splitting incl. PSQL body awareness | Execution, planning, name resolution against a live catalog |
| Verb + object-type + name extraction | Full AST / expression grammar |
| v3-relevant flags (grant options, OR ALTER, column-mutation kinds) | Semantic correctness guarantees |
| WHERE-text extraction, best-effort affected objects | Guaranteed-complete affected-object sets |
| Reversibility & minimum-version heuristics | Version certification (heuristic only, flagged as such) |
| Audit template (literals → `?`) | Secret detection (scrubbing stays in the server) |
| Firebird-dialect reality only | Portability assumptions from other SQL dialects (Firebird has no `TRUNCATE`, no `USE`) — lookalikes are flagged Unknown, never mapped |

## 3. Functional requirements

**Splitting**
- **FR-1** Split input into top-level statements on the terminator (default `;`, overridable — isql `SET TERM` emulation), correctly skipping: string literals (`'…'` with `''` escapes), quoted identifiers (`"…"` with `""` escapes), line (`--`) and block (`/* … */`) comments.
- **FR-2** PSQL bodies (`CREATE/ALTER PROCEDURE | FUNCTION | TRIGGER | PACKAGE`, `EXECUTE BLOCK`) are single statements: internal semicolons inside `BEGIN … END` blocks must not split. `EXECUTE BLOCK` must classify as **mutating** if its body contains DML (the classic naive-filter bypass).
- **FR-3** Unclosed literal/comment, dialect-1 input (`"` used for strings), or other lexical oddity ⇒ statement classified `Unknown` with an `Issue`, never a silent misparse.

**Classification**
- **FR-4** Extract the verb, including multi-word forms: `EXECUTE PROCEDURE`, `EXECUTE BLOCK`, `COMMENT ON`, `SET GENERATOR`, `SET STATISTICS`, `DROP DATABASE`, `GRANT … ON`, `REVOKE … FROM`, `RECREATE`, `CREATE OR ALTER`, `DECLARE …` (external functions, filters).
- **FR-5** Extract the object type and target name for: TABLE (incl. GLOBAL TEMPORARY, EXTERNAL), VIEW, INDEX, SEQUENCE/GENERATOR, PROCEDURE, FUNCTION, PACKAGE, TRIGGER (DML/DDL/event — sub-kind flagged best-effort), DOMAIN, USER, ROLE, MAPPING, DATABASE, EXCEPTION, FILTER, SHADOW; constraint mutations reached via `ALTER TABLE … ADD/DROP CONSTRAINT` (PK/UNIQUE/FOREIGN KEY/CHECK). Where the operation targets a column of a container object (`COMMENT ON COLUMN t.c`, `ALTER TABLE t ALTER COLUMN c`), the container is `Object` and the column is the `Column` sub-object.
- **FR-6** Extract v3-relevant flags (§5 `Flags`): grant/admin options, `CREATE OR ALTER` vs `RECREATE`, column-mutation kind (`ALTER TABLE ALTER COLUMN`: type/size change, `NOT NULL`/null drop, default set/drop, rename), index attributes (UNIQUE, DESCENDING, computed/expression, `ALTER INDEX ACTIVE/INACTIVE`), DCL detail (`Grantee`, `Privileges` as written, incl. `ALL`), `UPDATE OR INSERT`, and `SELECT … WITH LOCK` (a read verb that takes row locks — preview-warning material for P2.4).
- **FR-7** Determine `Mutating` (any DML/DDL/DCL/EXECUTE) vs read (`SELECT`, `WITH … SELECT`).
- **FR-8** Multi-statement results preserve original text spans exactly (`Raw`, offsets) so audit logs the exact statement.

**Extraction & heuristics (all best-effort, flagged)**
- **FR-9** Affected objects for DML: `INSERT INTO t`, `UPDATE t` / `UPDATE OR INSERT`, `DELETE FROM t`, `MERGE INTO t USING s` (target and source). DML targets inside `EXECUTE BLOCK` bodies are extracted best-effort into `Secondary`, flagged with an issue.
- **FR-10** WHERE-clause text extraction for `UPDATE`/`DELETE` (text span, uninterpreted).
- **FR-11** Reversibility class: `ReverseDDL` (drop of creatable object, grant/revoke), `RestorePoint` (DML, column type changes, `DROP DATABASE`), `None` (reads).
- **FR-12** Minimum-version heuristic (e.g. packages/`BOOLEAN`/system privileges → 3.0, `ALTER DATABASE SET LINGER`-class syntax → 4.0, partial indexes (`CREATE INDEX … WHERE`) → 5.0). Incomplete by design; always paired with an `Issue` noting heuristic detection.
- **FR-13** Confidence (`High/Medium/Low`) plus `Issues` list; ambiguity is reported, never silently resolved.
- **FR-14** `Template()` — return the statement with literal values replaced by `?` (for audit scrubbing); must never alter identifier quoting.
- **FR-15** Read-path convenience: `IsReadOnly(input)` — exactly one `SELECT`/`WITH … SELECT` statement and nothing else; **false on any doubt** (multiple statements, any issue, any non-read verb). This is the P2.4 heavy-read guard's defense-in-depth check.
- **FR-16** Session/transaction-level `SET` statements (`SET TRANSACTION …`, `SET SESSION …`) classify as `Verb=Set` with a Variant and **no object** — they carry no v3 row of their own unless the mapping assigns one (e.g. `SET TRANSACTION … LOCK TIMEOUT`, op 43); the server-side mapping decides, the library just refuses to guess an object.

## 4. Non-functional requirements

- **NFR-1** Pure Go, **zero cgo**, zero external dependencies (stdlib only). *This effectively rules out tree-sitter bindings (cgo) under the project's `CGO_ENABLED=0` cross-compile constraint — a decisive input to ADR-019's option (b).*
- **NFR-2** No panics on **any** input (fuzz-guaranteed; feeds P6.1's continuous fuzzing). Errors only for caller misuse (oversized input, wrong statement count in `ParseOne`).
- **NFR-3** Stateless, immutable results, goroutine-safe.
- **NFR-4** Linear-time streaming lexer; bounded memory: input size cap option (default 16 MiB) with a typed error beyond it.
- **NFR-5** Performance budget: classify ≥ 10,000 average statements/sec on one core (the classifier runs per tool call, never in hot loops).
- **NFR-6** UTF-8 input; quoted identifiers may contain any Unicode; invalid UTF-8 ⇒ `Issue`, not failure.
- **NFR-7** Dialect 3 is the default and the fully supported mode; dialect-1 input is flagged (FR-3) and parsed leniently.

## 5. API specification (planned Go)

```go
// Package fbparse classifies Firebird SQL statements for the MCP server's
// policy and preview layers. It is a classifier, not a validator, and holds
// no safety responsibility (see the project's claim C1).
package fbparse

// Parse splits input into top-level statements and classifies each.
// It never fails: lexical oddities produce Unknown statements with Issues.
func Parse(input string, opts ...Option) []Statement

// ParseOne classifies exactly one statement; errors if the input contains
// zero or multiple statements, or exceeds the size cap.
func ParseOne(input string, opts ...Option) (Statement, error)

// Split returns statement spans only (no classification).
func Split(input string, opts ...Option) []Span

type Option func(*config)

func WithDialect(d Dialect) Option   // default Dialect3
func WithTerm(term string) Option    // default ";"
func WithMaxBytes(n int) Option      // default 16 MiB

type Span struct{ Start, End int }   // byte offsets into the original input

type Verb string
const (
    VerbSelect Verb = "SELECT" // includes WITH ... SELECT
    VerbInsert Verb = "INSERT"
    VerbUpdate Verb = "UPDATE"
    VerbDelete Verb = "DELETE"
    VerbMerge  Verb = "MERGE"
    VerbCreate Verb = "CREATE"
    VerbDrop   Verb = "DROP"
    VerbAlter  Verb = "ALTER"
    VerbRecreate     Verb = "RECREATE"
    VerbCreateOrAlter Verb = "CREATE OR ALTER"
    VerbGrant  Verb = "GRANT"
    VerbRevoke Verb = "REVOKE"
    VerbComment Verb = "COMMENT ON"
    VerbSet     Verb = "SET"
    VerbDeclare Verb = "DECLARE"
    VerbExecuteProc  Verb = "EXECUTE PROCEDURE"
    VerbExecuteBlock Verb = "EXECUTE BLOCK"
    VerbUnknown Verb = "UNKNOWN"
)

type ObjectType string
const (
    ObjTable           ObjectType = "TABLE"
    ObjGlobalTempTable ObjectType = "GLOBAL TEMPORARY TABLE"
    ObjExternalTable   ObjectType = "EXTERNAL TABLE"
    ObjView      ObjectType = "VIEW"
    ObjIndex     ObjectType = "INDEX"
    ObjSequence  ObjectType = "SEQUENCE"
    ObjProcedure ObjectType = "PROCEDURE"
    ObjFunction  ObjectType = "FUNCTION"
    ObjPackage   ObjectType = "PACKAGE"
    ObjTrigger   ObjectType = "TRIGGER"
    ObjDomain    ObjectType = "DOMAIN"
    ObjUser      ObjectType = "USER"
    ObjRole      ObjectType = "ROLE"
    ObjMapping   ObjectType = "MAPPING"
    ObjDatabase  ObjectType = "DATABASE"
    ObjConstraint ObjectType = "CONSTRAINT"
    ObjException ObjectType = "EXCEPTION"
    ObjFilter    ObjectType = "FILTER"
    ObjShadow    ObjectType = "SHADOW"
    ObjUnknown   ObjectType = "UNKNOWN"
)

type ObjectRef struct {
    Name   string // normalized: unquoted, case as written
    Quoted bool
}

type Flags struct {
    GrantOption, AdminOption bool   // GRANT/REVOKE modifiers
    ColumnMutation           string // "", "TYPE", "SIZE", "NOT_NULL", "DEFAULT", "RENAME" (ALTER COLUMN, best-effort)
    ConstraintKind           string // "PK", "UNIQUE", "FK", "CHECK" ("" if not a constraint statement)
    IndexUnique, IndexDescending bool
    IndexExpression          bool   // computed/function-based index
    IndexActivation          string // "ACTIVE", "INACTIVE" (ALTER INDEX), "" otherwise
    TriggerKind              string // "DML", "DDL", "DATABASE_EVENT" (best-effort)
    Upsert                   bool   // UPDATE OR INSERT
    WithLock                 bool   // SELECT ... WITH LOCK — read verb, but takes row locks (P2.4 preview warning)
    Extras                   map[string]string // forward-compatible extension point
}

type Reversibility uint8
const (
    ReversibilityNone uint8 = iota        // reads
    ReversibilityReverseDDL               // reversible via generated DDL (drop-of-creatable, grant/revoke)
    ReversibilityRestorePoint             // requires backup/restore point (DML, type changes, DROP DATABASE)
)

type Confidence uint8
const (
    ConfidenceHigh Confidence = iota
    ConfidenceMedium
    ConfidenceLow
)

type BodyInfo struct {
    HasDML        bool    // PSQL body contains INSERT/UPDATE/DELETE/MERGE
    EmbeddedVerbs []Verb  // distinct verbs found in the body
    Bytes         int     // body size
}

type IssueKind uint8
const (
    IssueUnclosedLiteral IssueKind = iota
    IssueDialect1Quoting
    IssueUnsupportedConstruct
    IssueAmbiguousParse
    IssueHeuristicVersion
    IssueInvalidUTF8
    IssueTruncated
)

type Issue struct {
    Kind   IssueKind
    Msg    string
    Offset int
}

type Statement struct {
    Raw           string      // exact original text of this statement
    Span          Span        // byte offsets into Parse input
    Verb          Verb
    ObjectType    ObjectType
    Object        ObjectRef   // primary target
    Column        *ObjectRef  // sub-object: column of a container (COMMENT ON COLUMN t.c, ALTER TABLE ... COLUMN c); nil otherwise
    Secondary     []ObjectRef // e.g. MERGE source, REFERENCES target (best-effort)
    Grantee       ObjectRef   // DCL: TO <user | role> target (zero value when not DCL)
    Privileges    []string    // DCL: privileges as written, e.g. ["SELECT","EXECUTE"] or ["ALL"]
    Flags         Flags
    Mutating      bool
    Reversibility Reversibility
    MinVersion    string      // "3.0" etc.; "" if none detected (heuristic, see FR-12)
    Where         string      // extracted WHERE text for UPDATE/DELETE (FR-10)
    Body          *BodyInfo   // non-nil for PSQL-bearing statements
    Confidence    Confidence
    Issues        []Issue
}

// OpKey returns the canonical classification key. The SERVER maps
// OpKey → v3 operation row → tier/impact via generated metadata;
// this library deliberately knows nothing about tiers or policy.
func (s Statement) OpKey() OpKey

type OpKey struct {
    Verb       Verb
    ObjectType ObjectType
    Variant    string // flag-reduced variant, e.g. "COLUMN_TYPE", "GRANT_WITH_OPTION", ""
}

// Template returns the statement with literal values replaced by '?' and
// everything else (including identifier quoting) unchanged — the audit-log form.
func (s Statement) Template() string

// RowEstimateQuery returns a best-effort "SELECT COUNT(*) FROM <target> WHERE <where>"
// for UPDATE/DELETE statements; ok=false when it cannot be built safely.
func (s Statement) RowEstimateQuery() (query string, ok bool)

// IsReadOnly reports whether input is exactly one SELECT / WITH-SELECT statement.
// It returns false on any doubt (multiple statements, any issue, any non-read verb);
// the P2.4 heavy-read guard consumes it as defense-in-depth.
func IsReadOnly(input string, opts ...Option) bool

// KnownOpKeys enumerates every OpKey the library can emit. The fbmcp CI drift
// gate cross-checks this against the generated v3 mapping so neither side can
// grow an unmapped or orphan classification.
func KnownOpKeys() []OpKey

// Sentinel errors for ParseOne.
var (
    ErrEmptyInput         = errors.New("fbparse: empty input")
    ErrMultipleStatements = errors.New("fbparse: input contains multiple statements")
    ErrTooLarge           = errors.New("fbparse: input exceeds the configured size cap")
)
```

**Semantic contracts worth stating explicitly:**
- `Parse` on garbage returns `[]Statement{{Verb: VerbUnknown, Confidence: ConfidenceLow, Issues: […]}}` — the caller (P4.1) turns Unknown into *deny*. The library must never map an unclassifiable statement to a read verb.
- `Template()` round-trip guarantee: `Parse(s.Template())` classifies identically to `Parse(s)` (literals are semantically inert for classification).
- Spans are exact: `input[span.Start:span.End] == stmt.Raw`.
- `MinVersion` heuristics only ever *raise* the floor; absence of detection means "" (unknown), not "2.5-safe".
- **Confidence is normative:** `High` = verb and object recognized, no issues; `Medium` = recognized but with heuristic/partial flags or unusual syntax; `Low` = any issue present, or unrecognized. `Unknown` statements are always `Low`.
- `RowEstimateQuery` is assembled from the **raw** WHERE span (real literals — a COUNT needs them), never from `Template()`; identifiers are re-quoted exactly as written.
- `IsReadOnly` returns false on any doubt — it may reject valid read queries, never accept a mutating one.

## 6. Boundary: what stays in the server

| Concern | Owner |
|---|---|
| OpKey → v3 row → tier/risk/impact mapping (generated, CI-drifted) | fbmcp `tools/metadata` |
| Policy decisions (deny on Unknown, tier ceilings, tier mixing) | fbmcp `internal/policy` (P1.5 / P4.1) |
| Affected-object *resolution* against live metadata | fbmcp via P2.6 (library names objects; the server verifies they exist) |
| Secret scrubbing beyond literal templating | fbmcp `internal/audit` (P1.4) |
| Version certification | `fb_info` (P2.1) — parser heuristics are advisory input only |
| Reverse-DDL / compensation **text generation** | fbmcp P4.1 + P2.6 extraction — the library only classifies `Reversibility` |
| `firebird.conf` / `databases.conf` editing | P4.6 — not SQL; K6/parser involvement there is limited to any `ALTER DATABASE SET …` statements, never the conf files |

## 7. Quality & acceptance

- **Canonical classification matrix** — the library ships fixtures for every v3 mutating row's canonical statement (~25–30 verb×object/variant combinations incl. DCL and flag variants); each must classify to the expected OpKey with `ConfidenceHigh`. This fixture set is the same corpus the fbmcp CI matrix test consumes ([phase4_plan.md](phase4_plan.md) §11). Library acceptance is a precondition for P4.1's exit (and therefore the M4 chain).
- **Adversarial corpus** (must classify safely-as-Unknown or correctly): `EXECUTE BLOCK … (INSERT …)`; comments hiding verbs (`/* DROP */ DELETE`); quoted identifiers containing `;` and `--`; dialect-1 quoting; unterminated everything; unicode identifiers; **unicode-confusable verbs** (Cyrillic-lookalike `CREATE` — mirrors the phase6 P6.1 T2 suite); empty/whitespace input; `SET TERM` blocks; nested `BEGIN…END`; `MERGE` with sub-selects.
- **Bidirectional OpKey drift gate:** CI cross-checks `KnownOpKeys()` against the server's generated v3 mapping — no OpKey the library can emit may be unmapped, and no mapped OpKey may be unemittable. Either direction of drift fails the build.
- **Property tests:** `Parse` never panics (also a fuzz target); spans reassemble to input; `Template` classification-equivalence; splitting is lossless.
- **Fuzz targets committed** (`FuzzParse`, `FuzzSplit`) — seed corpus from the adversarial set; these feed the P6.1 continuous fuzzing lane.
- **Benchmarks:** the NFR-5 budget, plus a pathological-input suite (deep nesting, long literals) proving linear behavior.

## 8. Delivery & integration

- **Location:** starts as `fbmcp/internal/fbparse` (K6's substrate); extraction to a standalone module is allowed once the API is stable (post-M4), not before.
- **ADR-019 evaluation criteria** for implementation options, weighted by: pure-Go/cgo-free (hard, per NFR-1), coverage of FR-1…FR-6 out of the box, fuzz-friendliness, maintenance burden, license cleanliness (only for option (c), adapting an OSS Firebird tool's parser). The hand-rolled lexer (option a) is the default expectation given NFR-1.
- **Versioning:** follows the server's versioning until extracted; API stability guaranteed from M4 onward (Appendix B discipline applies post-extraction).

## 9. Open questions

1. **MERGE depth:** extract only target+source (planned) or also `ON` predicate text for estimates? Deferred until P4.1 preview needs are measured in dogfooding.
2. **Dialect-1 support depth:** flag-and-lenient (planned) vs full lexical mode — revisit only if D1 (min FB version) keeps 2.5 in scope with dialect-1 databases.
3. **`RowEstimateQuery` placement:** library (planned, marked best-effort) vs server-side assembly from `Object`+`Where` — revisit if the WHERE extraction proves too lossy to be safe even best-effort.
4. **Trigger sub-kind detection (DML/DDL/event):** needed for tier display niceties only; may ship as `""` + issue initially.
5. **DCL privilege vocabulary normalization:** keep privileges as written (`["ALL"]` vs expanded list) and normalize server-side against RDB$ — or normalize in the library. Server-side preferred (v3 vocabulary is policy); revisit if previews look inconsistent.
