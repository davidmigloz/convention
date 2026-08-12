package db

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var placeholderPattern = regexp.MustCompile(`\$\d+`)

func Where() whereExpectingFirstStatement {
	return &where{}
}

type whereExpectingFirstStatement interface {
	Noop() whereExpectingLogicalOperator
	Key(key string) whereExpectingOperators
	Search(text string) whereExpectingLogicalOperator
	CreatedBetween(a, b time.Time) whereExpectingLogicalOperator
	CreatedBy(user string) whereExpectingLogicalOperator
	UpdatedBetween(a, b time.Time) whereExpectingLogicalOperator
	UpdatedBy(user string) whereExpectingLogicalOperator
	Expression(where whereExpectingLogicalOperator) whereExpectingLogicalOperator
}

type whereStatement interface {
	statement() (string, []any, error)
	statementParts() (predicate string, tail string, params []any, err error)
}

type whereClosed interface {
	whereStatement
}

type whereOrdered interface {
	whereStatement

	LimitPerShard(limit int) whereLimited
}

type whereLimited interface {
	whereStatement

	Offset(offset int) whereClosed
}

type whereReady interface {
	whereStatement
}

type whereExpectingOperators interface {
	Equals() whereExpectingValue
	NotEquals() whereExpectingValue
	IsNull() whereExpectingLogicalOperator
	IsNotNull() whereExpectingLogicalOperator
	GreaterThan() whereExpectingValue
	GreaterThanOrEquals() whereExpectingValue
	LessThan() whereExpectingValue
	LessThanOrEquals() whereExpectingValue
	In() whereExpectingValues
	NotIn() whereExpectingValues
	Like() whereExpectingValue
}

type whereExpectingLogicalOperator interface {
	Or() whereExpectingFirstStatement
	And() whereExpectingFirstStatement

	OrderByAsc(key string) whereOrdered
	OrderByDesc(key string) whereOrdered
	OrderByCreatedAtDesc() whereOrdered
	OrderByCreatedAtAsc() whereOrdered
	OrderByUpdatedAtDesc() whereOrdered
	OrderByUpdatedAtAsc() whereOrdered

	whereStatement
}

type whereExpectingValue interface {
	Value(value any) whereExpectingLogicalOperator
}

type whereExpectingValues interface {
	Values(values ...any) whereExpectingLogicalOperator
}

// where latches the first error into err; every builder method is a no-op
// once err is set. strings.Builder writes never fail, so their results are
// discarded rather than assigned over a previously latched error.
type where struct {
	query  strings.Builder
	tail   strings.Builder
	params []any
	err    error
}

func (w *where) statement() (string, []any, error) {
	predicate, tail, params, err := w.statementParts()
	if tail == "" {
		return predicate, params, err
	}
	return predicate + " " + tail, params, err
}

func (w *where) statementParts() (predicate string, tail string, params []any, err error) {
	if w == nil {
		return "", "", nil, errors.New("where statement is nil")
	}
	return w.query.String(), w.tail.String(), w.params, w.err
}

func (w *where) appendTail(clause string) {
	if w.tail.Len() > 0 {
		w.tail.WriteRune(' ')
	}
	w.tail.WriteString(clause)
}

func (w *where) Noop() whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	w.query.WriteString("1=1")
	return w
}

func (w *where) Expression(where whereExpectingLogicalOperator) whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	_, w.err = w.query.WriteRune('(')
	if w.err != nil {
		return w
	}
	query, params, err := where.statement()
	if err != nil {
		w.err = err
		return w
	}

	// Shift every inner placeholder by the outer parameter count in one pass.
	// Sequential ReplaceAll must not be used here: it re-matches its own
	// output (collapsing distinct placeholders → Postgres 42P18) and the
	// "$1" pattern also matches the prefix of "$10".
	if offset := len(w.params); offset > 0 {
		query = placeholderPattern.ReplaceAllStringFunc(query, func(m string) string {
			n, err := strconv.Atoi(m[1:])
			if err != nil {
				return m
			}
			return "$" + strconv.Itoa(n+offset)
		})
	}
	w.params = append(w.params, params...)

	_, w.err = w.query.WriteString(query)
	if w.err != nil {
		return w
	}

	_, w.err = w.query.WriteRune(')')
	return w
}

func keyToJsonColumn(key string) string {
	split := strings.Split(key, ".")
	if len(split) == 0 {
		return ""
	}
	if len(split) == 1 {
		return `"object"->'` + escapeJSONKeySegment(split[0]) + `'`
	}
	var sb strings.Builder
	sb.WriteString(`"object"`)
	for _, s := range split {
		sb.WriteString(`->'` + escapeJSONKeySegment(s) + `'`)
	}
	return sb.String()
}

// escapeJSONKeySegment makes a key segment safe to embed in a single-quoted SQL
// literal: an apostrophe in the key (or a crafted key) cannot break out of the
// quoting in the generated query / index DDL.
func escapeJSONKeySegment(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func (w *where) Key(key string) whereExpectingOperators {
	if w.err != nil {
		return w
	}
	if key == "" {
		w.err = errors.New("key cannot be empty")
		return w
	}
	w.query.WriteString(keyToJsonColumn(key))
	return w
}

func (w *where) Equals() whereExpectingValue {
	if w.err != nil {
		return w
	}
	w.query.WriteRune('=')
	return w
}

func (w *where) NotEquals() whereExpectingValue {
	if w.err != nil {
		return w
	}
	w.query.WriteString(`!=`)
	return w
}

func (w *where) IsNull() whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` IS NULL`)
	return w
}

func (w *where) IsNotNull() whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` IS NOT NULL`)
	return w
}

func (w *where) GreaterThan() whereExpectingValue {
	if w.err != nil {
		return w
	}
	w.query.WriteRune('>')
	return w
}

func (w *where) GreaterThanOrEquals() whereExpectingValue {
	if w.err != nil {
		return w
	}
	w.query.WriteString(`>=`)
	return w
}

func (w *where) LessThan() whereExpectingValue {
	if w.err != nil {
		return w
	}
	w.query.WriteRune('<')
	return w
}

func (w *where) LessThanOrEquals() whereExpectingValue {
	if w.err != nil {
		return w
	}
	w.query.WriteString(`<=`)
	return w
}

func (w *where) Like() whereExpectingValue {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` LIKE `)
	return w
}

func (w *where) In() whereExpectingValues {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` IN `)
	return w
}

func (w *where) NotIn() whereExpectingValues {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` NOT IN `)
	return w
}

func (w *where) Value(value any) whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	jsonValue, err := json.Marshal(value)
	if err != nil {
		w.err = err
		return w
	}
	w.query.WriteString(`$` + strconv.Itoa(len(w.params)+1))
	w.params = append(w.params, string(jsonValue))
	return w
}

func (w *where) Values(values ...any) whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	w.query.WriteRune('(')
	for i, value := range values {
		jsonValue, err := json.Marshal(value)
		if err != nil {
			w.err = err
			return w
		}
		if i > 0 {
			w.query.WriteString(`,`)
		}
		w.query.WriteString(`$` + strconv.Itoa(len(w.params)+1))
		w.params = append(w.params, string(jsonValue))
	}
	w.query.WriteRune(')')
	return w
}

func (w *where) Or() whereExpectingFirstStatement {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` OR `)
	return w
}

func (w *where) And() whereExpectingFirstStatement {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` AND `)
	return w
}

// maxSearchTextBytes caps the text bound to plainto_tsquery. A tsquery holds at
// most ~1MB of lexemes; past that PostgreSQL raises SQLSTATE 54000 ("value is
// too big in tsquery") and fails the whole statement, which is the same class of
// user-triggerable 500 this code exists to remove. Measured on PostgreSQL 16:
// 339KB of distinct words is accepted, 1.06MB is not. 64KiB leaves a wide margin
// and is far more than a search box can produce, while bounding how many GIN
// keys one search fans out across every shard. An over-long single *word* is not
// a concern - Postgres drops it with a notice rather than failing.
const maxSearchTextBytes = 1 << 16

// sanitizeSearchText makes arbitrary caller text safe to bind as a parameter.
// It deliberately does not escape or interpret query syntax: plainto_tsquery
// reads its argument as data, and the moment this package starts building
// tsquery syntax again the metacharacter crash is back. It removes only what
// Postgres cannot accept at all - a NUL byte is rejected outright ("null
// character not permitted") and an invalid UTF-8 sequence fails when the
// parameter is decoded - and trims a length the server would refuse.
func sanitizeSearchText(text string) string {

	// Replace rather than drop, so a NUL cannot fuse two words into one
	text = strings.ReplaceAll(text, "\x00", " ")

	// A Go string is arbitrary bytes; anything not valid UTF-8 fails on decode
	text = strings.ToValidUTF8(text, " ")

	// Cut on a rune boundary so the string stays valid UTF-8
	if len(text) > maxSearchTextBytes {
		cut := maxSearchTextBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}

	return text
}

// Search matches the object's generated "text_search" column against the
// caller's text, which is bound as data and never compiled into a query.
// plainto_tsquery hands its argument to the text-search parser and never reads
// it as tsquery syntax, so an apostrophe, a dangling operator or an unbalanced
// parenthesis typed into a search box can no longer reach the server as broken
// tsquery and fail the statement with SQLSTATE 42601. Do not reintroduce a
// Go-side tsquery builder: an escaper is only as exhaustive as the grammar it
// was written against, while not parsing is correct by construction.
//
// Words are AND-ed after stemming and stop-word removal, matching what the
// previous to_tsquery path produced for ordinary input. tsquery operators are
// no longer honoured - see the Text Search section of AGENTS.md.
func (w *where) Search(text string) whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	w.query.WriteString(`"text_search" @@ plainto_tsquery('english', $` + strconv.Itoa(len(w.params)+1) + `)`)
	w.params = append(w.params, sanitizeSearchText(text))
	return w
}

func (w *where) CreatedBetween(a, b time.Time) whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	w.query.WriteString(`"created_at" BETWEEN $` + strconv.Itoa(len(w.params)+1) + ` AND $` + strconv.Itoa(len(w.params)+2))
	w.params = append(w.params, a, b)
	return w
}

func (w *where) CreatedBy(user string) whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	w.query.WriteString(`"created_by" = $` + strconv.Itoa(len(w.params)+1))
	w.params = append(w.params, user)
	return w
}

func (w *where) UpdatedBetween(a, b time.Time) whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	w.query.WriteString(`"updated_at" BETWEEN $` + strconv.Itoa(len(w.params)+1) + ` AND $` + strconv.Itoa(len(w.params)+2))
	w.params = append(w.params, a, b)
	return w
}

func (w *where) UpdatedBy(user string) whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	w.query.WriteString(`"updated_by" = $` + strconv.Itoa(len(w.params)+1))
	w.params = append(w.params, user)
	return w
}

func (w *where) OrderByCreatedAtAsc() whereOrdered {
	if w.err != nil {
		return w
	}
	w.appendTail(`ORDER BY "created_at" ASC`)
	return w
}

func (w *where) OrderByCreatedAtDesc() whereOrdered {
	if w.err != nil {
		return w
	}
	w.appendTail(`ORDER BY "created_at" DESC`)
	return w
}

func (w *where) OrderByUpdatedAtAsc() whereOrdered {
	if w.err != nil {
		return w
	}
	w.appendTail(`ORDER BY "updated_at" ASC`)
	return w
}

func (w *where) OrderByUpdatedAtDesc() whereOrdered {
	if w.err != nil {
		return w
	}
	w.appendTail(`ORDER BY "updated_at" DESC`)
	return w
}

func (w *where) OrderByAsc(key string) whereOrdered {
	if w.err != nil {
		return w
	}
	w.appendTail(`ORDER BY ` + keyToJsonColumn(key) + ` ASC`)
	return w
}

func (w *where) OrderByDesc(key string) whereOrdered {
	if w.err != nil {
		return w
	}
	w.appendTail(`ORDER BY ` + keyToJsonColumn(key) + ` DESC`)
	return w
}

func (w *where) LimitPerShard(limit int) whereLimited {
	if w.err != nil {
		return w
	}
	w.appendTail(`LIMIT ` + strconv.Itoa(limit))
	return w
}

func (w *where) Offset(offset int) whereClosed {
	if w.err != nil {
		return w
	}
	w.appendTail(`OFFSET ` + strconv.Itoa(offset))

	return w
}
