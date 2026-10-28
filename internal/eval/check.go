package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Check types.
const (
	CheckContainsAny      = "contains_any"      // Values: at least one, case-insensitive
	CheckNotContains      = "not_contains"      // Values: none of them, case-insensitive
	CheckRegex            = "regex"             // Pattern: RE2, must match
	CheckMaxWords         = "max_words"         // N: at most N words
	CheckJSONSchema       = "json_schema"       // Schema: a JSON object with these keys and types
	CheckLang             = "lang"              // Lang: de or en (stop-word count)
	CheckEqualsNormalized = "equals_normalized" // Value: equal after Normalize
)

var checkTypes = []string{CheckContainsAny, CheckNotContains, CheckRegex, CheckMaxWords, CheckJSONSchema, CheckLang, CheckEqualsNormalized}

var jsonTypes = []string{"string", "number", "boolean", "array", "object"}

// Check is one deterministic rule on the answer text.
type Check struct {
	Type    string            `json:"type"`
	Values  []string          `json:"values,omitempty"`
	Value   string            `json:"value,omitempty"`
	Pattern string            `json:"pattern,omitempty"`
	N       int               `json:"n,omitempty"`
	Lang    string            `json:"lang,omitempty"`
	Schema  map[string]string `json:"schema,omitempty"`

	re *regexp.Regexp
}

// compile checks the fields of the type and compiles a pattern.
func (c *Check) compile() error {
	switch c.Type {
	case CheckContainsAny, CheckNotContains:
		if len(c.Values) == 0 || slices.Contains(c.Values, "") {
			return fmt.Errorf("%s needs non-empty values", c.Type)
		}
	case CheckRegex:
		re, err := regexp.Compile(c.Pattern)
		if err != nil || c.Pattern == "" {
			return fmt.Errorf("regex: pattern %q does not compile", c.Pattern)
		}
		c.re = re
	case CheckMaxWords:
		if c.N < 1 {
			return errors.New("max_words needs n >= 1")
		}
	case CheckJSONSchema:
		if len(c.Schema) == 0 {
			return errors.New("json_schema needs a schema")
		}
		for k, t := range c.Schema {
			if !slices.Contains(jsonTypes, t) {
				return fmt.Errorf("json_schema: key %q has unknown type %q", k, t)
			}
		}
	case CheckLang:
		if c.Lang != "de" && c.Lang != "en" {
			return fmt.Errorf("lang must be de or en, not %q", c.Lang)
		}
	case CheckEqualsNormalized:
		if Normalize(c.Value) == "" {
			return errors.New("equals_normalized needs a value")
		}
	default:
		return fmt.Errorf("type %q is unknown (%s)", c.Type, strings.Join(checkTypes, ", "))
	}
	return nil
}

// Apply runs the check. The reason names the rule, never the answer text.
func (c *Check) Apply(answer string) (bool, string) {
	low := strings.ToLower(answer)
	switch c.Type {
	case CheckContainsAny:
		for _, v := range c.Values {
			if strings.Contains(low, strings.ToLower(v)) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("contains none of %q", c.Values)
	case CheckNotContains:
		for _, v := range c.Values {
			if strings.Contains(low, strings.ToLower(v)) {
				return false, fmt.Sprintf("contains %q", v)
			}
		}
		return true, ""
	case CheckRegex:
		if c.re == nil {
			if err := c.compile(); err != nil {
				return false, err.Error()
			}
		}
		if c.re.MatchString(answer) {
			return true, ""
		}
		return false, fmt.Sprintf("does not match %q", c.Pattern)
	case CheckMaxWords:
		if n := len(strings.Fields(answer)); n > c.N {
			return false, fmt.Sprintf("%d words, max %d", n, c.N)
		}
		return true, ""
	case CheckJSONSchema:
		return c.applySchema(answer)
	case CheckLang:
		de, en := LangScore(answer)
		ok := (c.Lang == "de" && de >= 2 && de > en) || (c.Lang == "en" && en >= 2 && en > de)
		if ok {
			return true, ""
		}
		return false, fmt.Sprintf("language is not %s (de %d, en %d stop words)", c.Lang, de, en)
	case CheckEqualsNormalized:
		if Normalize(answer) == Normalize(c.Value) {
			return true, ""
		}
		return false, fmt.Sprintf("is not %q", c.Value)
	default:
		return false, fmt.Sprintf("type %q is unknown", c.Type)
	}
}

// fence matches a Markdown code fence around the whole answer.
var fence = regexp.MustCompile("(?s)^```[a-zA-Z]*\\s*\\n?(.*?)\\n?```$")

func (c *Check) applySchema(answer string) (bool, string) {
	text := strings.TrimSpace(answer)
	if m := fence.FindStringSubmatch(text); m != nil {
		text = strings.TrimSpace(m[1])
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return false, "is not one JSON object"
	}
	keys := make([]string, 0, len(c.Schema))
	for k := range c.Schema {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, ok := obj[k]
		if !ok {
			return false, fmt.Sprintf("key %q is missing", k)
		}
		if got := jsonType(v); got != c.Schema[k] {
			return false, fmt.Sprintf("key %q is %s, want %s", k, got, c.Schema[k])
		}
	}
	return true, ""
}

func jsonType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "null"
	}
}

// Normalize lowercases, drops punctuation at both ends and folds white space.
func Normalize(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r) })
}

// Stop words that are frequent in one language and rare in the other.
var (
	stopDE = set("der die das und ist nicht ein eine einen ich du sie wir ihr es mit auf für von zu den dem des im auch sich wird werden kann sind war hat haben oder aber wenn dass wie noch nur bei nach aus über einem einer")
	stopEN = set("the and is not a of to it you we they he she with on for from that this are be have has or but if how what which can would there their at by as")
)

func set(words string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(words) {
		out[w] = true
	}
	return out
}

// LangScore counts the German and the English stop words of a text.
func LangScore(text string) (de, en int) {
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if stopDE[w] {
			de++
		}
		if stopEN[w] {
			en++
		}
	}
	return de, en
}
