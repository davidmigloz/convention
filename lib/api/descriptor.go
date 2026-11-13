package api

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
)

func newDescriptor(host string, port int, pattern string, in, out reflect.Type) (desc descriptor) {

	var (
		segmentsSplit []string
	)

	// Extract method
	methodSplit := strings.Split(pattern, " ")
	hasMethodSpecific := len(methodSplit) > 1 && strings.HasPrefix(methodSplit[1], "/")
	if hasMethodSpecific {
		desc.method = methodSplit[0]
		pattern = strings.Join(methodSplit[1:], " ") // restore the pattern without the method
	} else {
		desc.method = http.MethodGet // Default method if not specified
	}

	// Extract query parameters
	querySplit := strings.Split(pattern, "?")
	if len(querySplit) > 1 {
		pattern = querySplit[0]
		// ignore parse errors on purpose as it is only used for openAPI generation
		values, _ := url.ParseQuery(querySplit[1])
		for n := range values {
			split := strings.Split(values.Get(n), "|")
			t := split[0]
			d := ""
			if len(split) > 1 {
				d = split[1]
			}
			desc.query = append(desc.query, queryParam{
				Name:        n,
				Type:        objectType(t),
				Description: d,
			})
		}
	}

	// Split the pattern into segments
	segmentsSplit = strings.Split(strings.Trim(pattern, "/"), "/")

	weight := 0
	for i, s := range segmentsSplit {

		if s == "{any...}" && i == len(segmentsSplit)-1 {
			desc.open = true
			break
		}

		isParam := strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")
		if isParam {
			s = strings.TrimLeft(s, "{")
			s = strings.TrimRight(s, "}")
			desc.segments = append(desc.segments, urlSegment{s, true})
		} else {
			desc.segments = append(desc.segments, urlSegment{s, false})
			weight++
		}
	}

	desc.weight = weight
	desc.host = host
	desc.port = port
	desc.in = objectFromType(in, false)
	desc.out = objectFromType(out, false)

	return
}

type descriptor struct {
	host     string
	port     int
	method   string
	segments []urlSegment
	query    []queryParam
	weight   int
	open     bool

	in, out *object
}

func (desc *descriptor) path() string {
	sb := strings.Builder{}
	for _, segment := range desc.segments {
		sb.WriteRune('/')
		if segment.Param {
			sb.WriteRune('{')
			sb.WriteString(segment.Value)
			sb.WriteRune('}')
		} else {
			sb.WriteString(segment.Value)
		}
	}
	return sb.String()
}

func (desc *descriptor) parameters() (params []string) {
	for _, segment := range desc.segments {
		if segment.Param {
			params = append(params, segment.Value)
		}
	}
	return
}

type objectType string

func (o objectType) IsSimple() bool {
	switch o {
	case objectTypeString,
		objectTypeInteger,
		objectTypeNumber,
		objectTypeBoolean:
		return true
	default:
		return false
	}
}

const (
	objectTypeString  objectType = "string"
	objectTypeInteger objectType = "integer"
	objectTypeNumber  objectType = "number"
	objectTypeBoolean objectType = "boolean"
	objectTypeArray   objectType = "array"
	objectTypeMap     objectType = "map"
	objectTypeObject  objectType = "object"
	objectTypeInvalid objectType = "invalid"
	objectTypeTime    objectType = "time"
	objectTypeEnum    objectType = "enum"
)

type object struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Type      objectType         `json:"type"`
	Mandatory bool               `json:"mandatory"`
	Elem      *object            `json:"elem"`
	Key       *object            `json:"key"`
	Fields    map[string]*object `json:"fields"`
}

func snakeName(name string) string {
	sb := strings.Builder{}
	wasCapital := false
	wasNumber := false
	for i, r := range name {
		isCapital := 'A' <= r && r <= 'Z'
		isNumber := '0' <= r && r <= '9'
		isLetter := ('a' <= r && r <= 'z') || r == '_' || isCapital
		isAllowed := isNumber || isLetter
		if !isAllowed {
			continue
		}
		if i == 0 {
			wasCapital = isCapital
		}
		if isCapital && !wasCapital {
			sb.WriteRune('_')
		}
		if isNumber && !wasNumber {
			sb.WriteRune('_')
		}
		sb.WriteRune(r)
		wasCapital = isCapital
		wasNumber = isNumber
	}
	return strings.ToLower(sb.String())
}

// Remove package name from a fully qualified type name
func friendlyName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Array, reflect.Slice:
		return "list_of_" + friendlyName(t.Elem())
	case reflect.Map:
		return "map_by_" + friendlyName(t.Key()) + "_of_" + friendlyName(t.Elem())
	default:
		return snakeName(strings.Join(extractAllNames(t.String()), " "))
	}
}

// extractAllNames extracts all type names from a string representation of a type,
func extractAllNames(s string) []string {
	if !strings.Contains(s, "[") {
		return []string{simplifyTypeName(s)}
	}

	var result []string
	var curr strings.Builder
	depth := 0

	for _, ch := range s {
		switch ch {
		case '[':
			if depth == 0 {
				result = append(result, simplifyTypeName(curr.String()))
			}
			curr.Reset()
			depth++
		case ']':
			if curr.Len() > 0 {
				result = append(result, simplifyTypeName(curr.String()))
				curr.Reset()
			}
			depth--
		default:
			curr.WriteRune(ch)
		}
	}
	return result
}

func simplifyTypeName(name string) string {
	if dotIndex := strings.LastIndex(name, "."); dotIndex != -1 {
		return name[dotIndex+1:]
	}
	return name
}

func objectFromType(t reflect.Type, optional bool, knownObjects ...*object) (o *object) {

	if t == nil {
		return nil
	}

	o = &object{}

	isPointer := t.Kind() == reflect.Ptr

	if isPointer {
		t = t.Elem() // Dereference if it's a pointer
	}

	o.Name = friendlyName(t)
	o.ID = t.PkgPath() + "/" + o.Name

	for _, known := range knownObjects {
		if known.ID == o.ID {
			return known
		}
	}

	knownObjects = append(knownObjects, o)

	switch t.Kind() {

	case reflect.Bool:
		o.Mandatory = !optional && !isPointer
		o.Type = objectTypeBoolean

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		o.Mandatory = !optional && !isPointer
		o.Type = objectTypeInteger

	case reflect.Float32, reflect.Float64:
		o.Mandatory = !optional && !isPointer
		o.Type = objectTypeNumber

	case reflect.String:
		o.Mandatory = !optional && !isPointer
		o.Type = objectTypeString

	case reflect.Array, reflect.Slice:
		o.Mandatory = false // Arrays and slices are optional by default
		o.Type = objectTypeArray
		o.Elem = objectFromType(t.Elem(), true, knownObjects...)

	case reflect.Map:
		o.Mandatory = false // Maps are optional by default
		o.Type = objectTypeMap
		o.Key, o.Elem = objectFromType(t.Key(), false, knownObjects...), objectFromType(t.Elem(), false, knownObjects...)

	case reflect.Struct:
		o.Mandatory = !isPointer

		switch t {
		case reflect.TypeOf(time.Time{}):
			o.Type = objectTypeTime
		default:
			o.Type = objectTypeObject
			o.Fields = make(map[string]*object)
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				if field.PkgPath != "" {
					continue // Skip unexported fields
				}
				jsonTag := field.Tag.Get("json")
				if jsonTag == "-" {
					continue // Skip fields with json tag "-"
				}
				omitEmpty := false
				if jsonTag != "" {
					split := strings.Split(jsonTag, ",")
					jsonTag = split[0] // Get the first part of the json tag
					for _, tag := range split[1:] {
						if tag == "omitempty" {
							omitEmpty = true // Check for omitempty tag
						}
					}
				}
				if jsonTag == "" {
					jsonTag = field.Name
				}

				if field.Anonymous {
					// If the field is an anonymous struct, we recursively add its fields
					subObject := objectFromType(field.Type, omitEmpty, knownObjects...)
					for subName, subField := range subObject.Fields {
						o.Fields[subName] = subField
					}
					continue
				}

				o.Fields[jsonTag] = objectFromType(field.Type, omitEmpty, knownObjects...)
			}
		}

	case reflect.Interface:
		o.Type = objectTypeObject

	case reflect.Invalid, reflect.Uintptr, reflect.Complex64, reflect.Complex128,
		reflect.Chan, reflect.Func,
		reflect.UnsafePointer:
		o.Type = objectTypeInvalid
	}

	return
}

func (desc *descriptor) isSet() bool {
	return desc != nil && desc.host != ""
}

func (desc *descriptor) match(r *http.Request) (values values, match bool) {

	if desc.method != "{any}" && desc.method != r.Method {
		match = false
		return
	}

	// Split the escaped path, then unescape each segment, so an escaped '/'
	// (%2F) stays inside its segment. Splitting the decoded r.URL.Path would
	// turn a value such as "x%2F@projection" into two segments and could
	// route the request onto a longer sibling endpoint.
	urlSplit := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
	for i, s := range urlSplit {
		unescaped, err := url.PathUnescape(s)
		if err != nil {
			match = false
			return
		}
		urlSplit[i] = unescaped
	}

	if len(urlSplit) < len(desc.segments) {
		match = false
		return
	}

	segmentsCountMatch := len(urlSplit) == len(desc.segments)

	if !desc.open && !segmentsCountMatch {
		match = false
		return
	}

	for i, segment := range desc.segments {
		if segment.Param {
			values.Add(segment.Value, urlSplit[i])
			continue
		}
		if segment.Value != urlSplit[i] {
			match = false
			return
		}
	}

	match = segmentsCountMatch || desc.open

	return

}

func (desc *descriptor) newRequest(vls values, body io.Reader) (*http.Request, error) {

	sb := strings.Builder{}

	sb.WriteString("https://")
	sb.WriteString(fmt.Sprintf("%s:%d", desc.host, desc.port))
	sb.WriteRune('/')

	vi := 0

	for i, segment := range desc.segments {

		if segment.Param {
			val := vls.GetByIndex(vi)
			if val == "" {
				return nil, fmt.Errorf("missing value '%s'", segment.Value)
			}
			// Escape the value so it is always exactly one path segment:
			// a '/', '?', '#' or '%' in it must not change the route.
			sb.WriteString(url.PathEscape(val))
			vi++
		} else {
			sb.WriteString(segment.Value)
		}

		if i < len(desc.segments)-1 {
			sb.WriteRune('/')
		}
	}

	return http.NewRequest(desc.method, sb.String(), body)
}

type urlSegment struct {
	Value string
	Param bool
}

type queryParam struct {
	Name        string
	Type        objectType
	Description string
}
