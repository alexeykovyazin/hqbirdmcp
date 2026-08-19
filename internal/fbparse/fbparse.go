// Package fbparse classifies Firebird SQL statements for the MCP server's
// policy and preview layers. It is a classifier, not a validator, and holds
// no safety responsibility (see the project's claim C1): a wrong
// classification degrades preview quality, never the safety boundary.
//
// The lexical and statement grammar recognized here follows the Firebird
// engine sources (src/dsql/parse.y, src/dsql/Parser.cpp,
// src/common/ParserTokens.h). Where the engine's full grammar is out of
// scope, constructs are reported as Unknown with an issue rather than
// silently misparsed.
//
// Zero-value conventions:
//
//	Statement.ObjectType ""   — no object applies (reads, session SET, ...)
//	Statement.ObjectType "UNKNOWN" — classification attempted, not recognized
package fbparse

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Parse splits input into top-level statements and classifies each.
// It never fails: lexical oddities produce Unknown statements with Issues.
// Oversized input (beyond the configured cap) yields a single Unknown
// statement carrying an IssueTruncated issue; use ParseOne for a typed error.
func Parse(input string, opts ...Option) []Statement {
	cfg := newConfig(opts...)
	if len(input) > cfg.maxBytes {
		return []Statement{{
			Verb:          VerbUnknown,
			Mutating:      true,
			Confidence:    ConfidenceLow,
			Reversibility: ReversibilityNone,
			Issues:        []Issue{{Kind: IssueTruncated, Msg: "fbparse: input exceeds the configured size cap", Offset: 0}},
		}}
	}

	badUTF8Off := firstInvalidUTF8(input)

	var stmts []Statement
	for _, rs := range split(input, &cfg) {
		s := classify(rs, input, &cfg)
		if badUTF8Off >= 0 && badUTF8Off >= s.Span.Start && badUTF8Off < s.Span.End {
			s.Issues = append(s.Issues, Issue{
				Kind:   IssueInvalidUTF8,
				Msg:    "fbparse: input contains invalid UTF-8; classification of this statement is degraded",
				Offset: badUTF8Off,
			})
			s.Verb = VerbUnknown
			s.Mutating = true
			s.Confidence = ConfidenceLow
		}
		stmts = append(stmts, s)
	}
	return stmts
}

// ParseOne classifies exactly one statement; errors if the input contains
// zero or multiple statements, or exceeds the size cap.
func ParseOne(input string, opts ...Option) (Statement, error) {
	cfg := newConfig(opts...)
	if len(input) > cfg.maxBytes {
		return Statement{}, ErrTooLarge
	}
	stmts := Parse(input, opts...)
	switch len(stmts) {
	case 1:
		return stmts[0], nil
	case 0:
		return Statement{}, ErrEmptyInput
	default:
		return Statement{}, ErrMultipleStatements
	}
}

// Split returns statement spans only (no classification). Spans exclude the
// terminator and inter-statement whitespace; input[span.Start:span.End] is
// the exact statement text. Bytes outside spans are whitespace or
// terminators only, so splitting is lossless.
func Split(input string, opts ...Option) []Span {
	cfg := newConfig(opts...)
	if len(input) > cfg.maxBytes {
		return nil
	}
	var out []Span
	for _, rs := range split(input, &cfg) {
		out = append(out, rs.span)
	}
	return out
}

// IsReadOnly reports whether input is exactly one SELECT / WITH-SELECT
// statement. It returns false on any doubt (multiple statements, any issue,
// any non-read verb, WITH LOCK); it may reject valid read queries, never
// accept a mutating one. The P2.4 heavy-read guard consumes it as
// defense-in-depth.
func IsReadOnly(input string, opts ...Option) bool {
	cfg := newConfig(opts...)
	if len(input) > cfg.maxBytes {
		return false
	}
	stmts := Parse(input, opts...)
	if len(stmts) != 1 {
		return false
	}
	s := stmts[0]
	return s.Verb == VerbSelect && !s.Mutating && s.Confidence == ConfidenceHigh &&
		len(s.Issues) == 0 && !s.Flags.WithLock
}

// Sentinel errors for ParseOne.
var (
	ErrEmptyInput         = errors.New("fbparse: empty input")
	ErrMultipleStatements = errors.New("fbparse: input contains multiple statements")
	ErrTooLarge           = errors.New("fbparse: input exceeds the configured size cap")
)

type Dialect uint8

const (
	// Dialect3 is the default and fully supported mode (NFR-7).
	Dialect3 Dialect = iota
	// Dialect1 lexes double-quoted text as string literals (lenient,
	// flagged with IssueDialect1Quoting).
	Dialect1
)

const defaultMaxBytes = 16 << 20 // 16 MiB (NFR-4)

type config struct {
	dialect  Dialect
	term     string
	maxBytes int
}

// Option configures Parse, ParseOne, Split and IsReadOnly.
type Option func(*config)

// WithDialect selects the SQL dialect for lexical decisions.
func WithDialect(d Dialect) Option { return func(c *config) { c.dialect = d } }

// WithTerm sets the statement terminator (default ";"). An empty or
// over-long term is ignored (the default is kept). SET TERM directives in
// the input still override it positionally.
func WithTerm(term string) Option {
	return func(c *config) {
		if term != "" && len(term) <= 16 {
			c.term = term
		}
	}
}

// WithMaxBytes caps input size (default 16 MiB). Non-positive values keep
// the default.
func WithMaxBytes(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxBytes = n
		}
	}
}

func newConfig(opts ...Option) config {
	c := config{dialect: Dialect3, term: ";", maxBytes: defaultMaxBytes}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// Span is a half-open byte range into the Parse input.
type Span struct{ Start, End int }

// Verb is the statement verb, including the multi-word forms Firebird
// grammar treats as a single leading construct (FR-4).
type Verb string

const (
	VerbSelect        Verb = "SELECT" // includes WITH ... SELECT
	VerbInsert        Verb = "INSERT"
	VerbUpdate        Verb = "UPDATE"
	VerbDelete        Verb = "DELETE"
	VerbMerge         Verb = "MERGE"
	VerbCreate        Verb = "CREATE"
	VerbDrop          Verb = "DROP"
	VerbAlter         Verb = "ALTER"
	VerbRecreate      Verb = "RECREATE"
	VerbCreateOrAlter Verb = "CREATE OR ALTER"
	VerbGrant         Verb = "GRANT"
	VerbRevoke        Verb = "REVOKE"
	VerbComment       Verb = "COMMENT ON"
	VerbSet           Verb = "SET"
	VerbDeclare       Verb = "DECLARE"
	VerbExecuteProc   Verb = "EXECUTE PROCEDURE"
	VerbExecuteBlock  Verb = "EXECUTE BLOCK"
	VerbUnknown       Verb = "UNKNOWN"
)

// ObjectType is the kind of object a statement targets. "" means no object
// applies; ObjUnknown means classification was attempted but the type was
// not recognized.
type ObjectType string

const (
	ObjTable           ObjectType = "TABLE"
	ObjGlobalTempTable ObjectType = "GLOBAL TEMPORARY TABLE"
	ObjExternalTable   ObjectType = "EXTERNAL TABLE"
	ObjView            ObjectType = "VIEW"
	ObjIndex           ObjectType = "INDEX"
	ObjSequence        ObjectType = "SEQUENCE"
	ObjProcedure       ObjectType = "PROCEDURE"
	ObjFunction        ObjectType = "FUNCTION"
	ObjPackage         ObjectType = "PACKAGE"
	ObjTrigger         ObjectType = "TRIGGER"
	ObjDomain          ObjectType = "DOMAIN"
	ObjUser            ObjectType = "USER"
	ObjRole            ObjectType = "ROLE"
	ObjMapping         ObjectType = "MAPPING"
	ObjDatabase        ObjectType = "DATABASE"
	ObjConstraint      ObjectType = "CONSTRAINT"
	ObjException       ObjectType = "EXCEPTION"
	ObjFilter          ObjectType = "FILTER"
	ObjShadow          ObjectType = "SHADOW"
	ObjUnknown         ObjectType = "UNKNOWN"
)

// ObjectRef references a named object. Name is normalized (unquoted, case
// as written); qualified names are joined with '.'. Quoted is true when any
// component was double-quoted.
type ObjectRef struct {
	Name   string
	Quoted bool
}

// Flags carries the v3-relevant statement modifiers (FR-6). All fields are
// best-effort; see each field for its grammar anchor.
type Flags struct {
	// GrantOption: GRANT ... WITH GRANT OPTION, or REVOKE GRANT OPTION FOR.
	GrantOption bool
	// AdminOption: role GRANT ... WITH ADMIN OPTION, or REVOKE ADMIN OPTION FOR.
	AdminOption bool
	// ColumnMutation: "", "TYPE", "SIZE", "NOT_NULL", "DEFAULT", "RENAME"
	// for ALTER TABLE ... ALTER COLUMN (best-effort).
	ColumnMutation string
	// ConstraintKind: "PK", "UNIQUE", "FK", "CHECK" ("" if not a
	// constraint statement).
	ConstraintKind string
	// IndexUnique, IndexDescending, IndexExpression: CREATE INDEX attributes.
	IndexUnique, IndexDescending bool
	IndexExpression              bool
	// IndexActivation: "ACTIVE", "INACTIVE" (ALTER INDEX), "" otherwise.
	IndexActivation string
	// TriggerKind: "DML", "DDL", "DATABASE_EVENT" (best-effort).
	TriggerKind string
	// Upsert: UPDATE OR INSERT.
	Upsert bool
	// WithLock: SELECT ... WITH LOCK — read verb, but takes row locks
	// (P2.4 preview warning).
	WithLock bool
	// Extras is a forward-compatible extension point.
	Extras map[string]string
}

func (f *Flags) setExtra(k, v string) {
	if f.Extras == nil {
		f.Extras = make(map[string]string, 4)
	}
	f.Extras[k] = v
}

// Reversibility classifies how a statement can be undone (FR-11).
type Reversibility uint8

const (
	// ReversibilityNone: reads — nothing to undo.
	ReversibilityNone Reversibility = iota
	// ReversibilityReverseDDL: reversible via generated DDL (drop of a
	// creatable object, grant/revoke).
	ReversibilityReverseDDL
	// ReversibilityRestorePoint: requires a restore point (DML, column
	// type changes, DROP DATABASE).
	ReversibilityRestorePoint
)

// Confidence is normative: High = verb and object recognized, no issues;
// Medium = recognized with heuristic/partial flags only; Low = any other
// issue present, or unrecognized. Unknown statements are always Low.
type Confidence uint8

const (
	ConfidenceHigh Confidence = iota
	ConfidenceMedium
	ConfidenceLow
)

// BodyInfo summarizes the PSQL body of a statement (FR-2, FR-9).
type BodyInfo struct {
	// HasDML: body contains INSERT/UPDATE/DELETE/MERGE.
	HasDML bool
	// EmbeddedVerbs: distinct verbs found in the body, in first-seen order.
	EmbeddedVerbs []Verb
	// Bytes: body size (the BEGIN..END region).
	Bytes int
}

// IssueKind enumerates classifier issues (FR-3, FR-12, FR-13).
type IssueKind uint8

const (
	IssueUnclosedLiteral IssueKind = iota // unterminated string or comment
	IssueDialect1Quoting                  // " used as string delimiter
	IssueUnsupportedConstruct
	IssueAmbiguousParse
	IssueHeuristicVersion
	IssueInvalidUTF8
	IssueTruncated
)

// Issue reports a lexical or heuristic caveat attached to a statement.
// Offset is a byte offset into the Parse input where available.
type Issue struct {
	Kind   IssueKind
	Msg    string
	Offset int
}

// Statement is one classified top-level statement. It is immutable and
// goroutine-safe after construction (NFR-3).
type Statement struct {
	// Raw is the exact original text of this statement (terminator
	// excluded); input[Span.Start:Span.End] == Raw (FR-8).
	Raw        string
	Span       Span
	Verb       Verb
	ObjectType ObjectType
	// Object is the primary target. Where the operation targets a column
	// of a container (COMMENT ON COLUMN t.c, ALTER TABLE ... COLUMN c),
	// the container is Object and the column is Column.
	Object    ObjectRef
	Column    *ObjectRef
	Secondary []ObjectRef // e.g. MERGE source, REFERENCES target (best-effort)
	// Grantee: DCL TO <user | role> target (zero value when not DCL).
	Grantee ObjectRef
	// Privileges: DCL privileges as written, e.g. ["SELECT","EXECUTE"] or ["ALL"].
	Privileges    []string
	Flags         Flags
	Mutating      bool
	Reversibility Reversibility
	// MinVersion: e.g. "3.0"; "" if none detected (heuristic, FR-12 —
	// detection only ever raises the floor).
	MinVersion string
	// Where: extracted WHERE text for UPDATE/DELETE (uninterpreted, FR-10).
	Where string
	// Body: non-nil for PSQL-bearing statements.
	Body       *BodyInfo
	Confidence Confidence
	Issues     []Issue

	// variant is the OpKey variant computed at classify time.
	variant string
	// rawTarget is the primary target exactly as written (quoting
	// preserved) for RowEstimateQuery.
	rawTarget string
	// dialect1 records the parse dialect so Template() treats
	// double-quoted text consistently with classification.
	dialect1 bool
}

// OpKey returns the canonical classification key. The SERVER maps
// OpKey → v3 operation row → tier/impact via generated metadata; this
// library deliberately knows nothing about tiers or policy.
func (s Statement) OpKey() OpKey {
	return OpKey{Verb: s.Verb, ObjectType: s.ObjectType, Variant: s.variant}
}

// Template returns the statement with literal values replaced by '?' and
// everything else (including identifier quoting) unchanged — the audit-log
// form (FR-14). String-family literals (single-quoted strings, hex string
// constants, dialect-1 double-quoted strings) are replaced; numeric
// literals are preserved because Firebird grammar places bare numbers in
// name positions (DROP SHADOW n, POSITION n) and literals are inert for
// classification. Statements with unterminated literals are returned
// unchanged: their token boundaries cannot be trusted, and the audit log
// should see the oddity.
func (s Statement) Template() string {
	for _, iss := range s.Issues {
		if iss.Kind == IssueUnclosedLiteral {
			return s.Raw
		}
	}
	return templateOf(s.Raw, s.dialect1)
}

// RowEstimateQuery returns a best-effort
// "SELECT COUNT(*) FROM <target> WHERE <where>" for UPDATE/DELETE
// statements; ok=false when it cannot be built safely (FR-10, §5).
// The query is assembled from the raw WHERE span (real literals — a COUNT
// needs them), never from Template(); identifiers are re-quoted exactly as
// written.
func (s Statement) RowEstimateQuery() (query string, ok bool) {
	if s.Verb != VerbUpdate && s.Verb != VerbDelete {
		return "", false
	}
	if s.Flags.Upsert { // UPDATE OR INSERT has no search WHERE semantics
		return "", false
	}
	if s.rawTarget == "" || s.Object.Name == "" {
		return "", false
	}
	for _, iss := range s.Issues {
		if iss.Kind == IssueHeuristicVersion {
			continue
		}
		return "", false
	}
	if s.Confidence == ConfidenceLow {
		return "", false
	}
	// Positioned updates cannot be turned into a searched COUNT.
	if strings.Contains(strings.ToUpper(s.Where), "CURRENT OF") {
		return "", false
	}
	q := "SELECT COUNT(*) FROM " + s.rawTarget
	if w := strings.TrimSpace(s.Where); w != "" {
		q += " " + w
	}
	return q, true
}

// firstInvalidUTF8 returns the byte offset of the first invalid UTF-8
// sequence, or -1 when the input is valid.
func firstInvalidUTF8(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return -1
}
