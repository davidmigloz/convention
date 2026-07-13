package db

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

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

type whereClosed interface {
	statement() (string, []any, error)
}

type whereOrdered interface {
	statement() (string, []any, error)

	LimitPerShard(limit int) whereLimited
}

type whereLimited interface {
	statement() (string, []any, error)

	Offset(offset int) whereClosed
}

type whereReady interface {
	statement() (string, []any, error)
}

type whereExpectingOperators interface {
	Equals() whereExpectingValue
	NotEquals() whereExpectingValue
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

	statement() (string, []any, error)
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
	params []any
	err    error
}

func (w *where) statement() (string, []any, error) {
	if w == nil {
		return "", nil, errors.New("where statement is nil")
	}
	return w.query.String(), w.params, w.err
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

	for i, param := range params {
		query = strings.ReplaceAll(query, "$"+strconv.Itoa(i+1), "$"+strconv.Itoa(len(w.params)+1))
		w.params = append(w.params, param)
	}

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

func (w *where) Search(text string) whereExpectingLogicalOperator {
	if w.err != nil {
		return w
	}
	w.query.WriteString(`"text_search" @@ to_tsquery('english', $` + strconv.Itoa(len(w.params)+1) + `)`)
	w.params = append(w.params, toTSQuery(text))
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
	w.query.WriteString(` ORDER BY "created_at" ASC`)
	return w
}

func (w *where) OrderByCreatedAtDesc() whereOrdered {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` ORDER BY "created_at" DESC`)
	return w
}

func (w *where) OrderByUpdatedAtAsc() whereOrdered {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` ORDER BY "updated_at" ASC`)
	return w
}

func (w *where) OrderByUpdatedAtDesc() whereOrdered {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` ORDER BY "updated_at" DESC`)
	return w
}

func (w *where) OrderByAsc(key string) whereOrdered {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` ORDER BY ` + keyToJsonColumn(key) + ` ASC`)
	return w
}

func (w *where) OrderByDesc(key string) whereOrdered {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` ORDER BY ` + keyToJsonColumn(key) + ` DESC`)
	return w
}

func (w *where) LimitPerShard(limit int) whereLimited {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` LIMIT ` + strconv.Itoa(limit))
	return w
}

func (w *where) Offset(offset int) whereClosed {
	if w.err != nil {
		return w
	}
	w.query.WriteString(` OFFSET ` + strconv.Itoa(offset))

	return w
}

var whitespacePattern = regexp.MustCompile(`\s+`)

func toTSQuery(input string) string {

	// Step 1: Replace multiple spaces with a single space
	input = whitespacePattern.ReplaceAllString(input, " ")

	// Step 2: Trim leading and trailing spaces (if any)
	input = strings.TrimSpace(input)

	// Step 3: Replace spaces with the '&' operator
	input = strings.ReplaceAll(input, " ", " & ")

	return input
}
