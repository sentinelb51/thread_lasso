package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// This file turns the Tuning struct into a list of described, editable
// settings. One registry drives the settings UI, the generated reference and
// the post-load validation, so a knob cannot exist in the config without also
// being visible and range-checked — which is the whole point of moving policy
// out of constants.

// Kind is what a setting looks like to an editor.
type Kind int

const (
	KindInt Kind = iota
	KindFloat
	KindBool
	KindChoice
	KindStrings // comma-separated list
)

// Inherit is the rendered value of an unset optional setting: the per-role
// overrides are tri-state, and "inherit" is the third state.
const Inherit = "inherit"

// Setting is one knob, bound to a live Tuning and to the default it would be
// reset to.
type Setting struct {
	Path    string // dotted config path, e.g. "buckets.interactive.priority"
	Group   string // display grouping, e.g. "Bucket: interactive"
	Label   string
	Desc    string
	Kind    Kind
	Choices []string

	Min, Max       float64
	HasMin, HasMax bool

	// Optional marks a pointer field, where the empty string (or "inherit")
	// clears the override instead of setting a value.
	Optional bool

	value    reflect.Value // addressable field inside the live Tuning
	fallback reflect.Value // the same field inside the defaults
}

// Settings lists every knob in declaration order, bound to live for editing and
// to defaults for "reset". Both arguments must be non-nil and of the same shape;
// pass the result of DefaultTuning for the game's aggression level as defaults.
func Settings(live *Tuning, defaults *Tuning) []Setting {
	var out []Setting
	walk(reflect.ValueOf(live).Elem(), reflect.ValueOf(defaults).Elem(), "", "", &out)

	return out
}

// walk appends a Setting for every leaf field, recursing through nested structs
// and carrying the nearest ancestor's group tag down with it.
func walk(live, defaults reflect.Value, path, group string, out *[]Setting) {
	structType := live.Type()

	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if !field.IsExported() {
			continue
		}

		name := jsonName(field)
		if name == "" {
			continue
		}

		childPath := name
		if path != "" {
			childPath = path + "." + name
		}
		childGroup := group
		if tag := field.Tag.Get("group"); tag != "" {
			childGroup = tag
		}

		value, fallback := live.Field(i), defaults.Field(i)

		// A nested struct is a section, not a value — unless it is a pointer,
		// which is how an optional leaf is spelled.
		if value.Kind() == reflect.Struct {
			walk(value, fallback, childPath, childGroup, out)
			continue
		}

		if setting, ok := leaf(field, value, fallback, childPath, childGroup); ok {
			*out = append(*out, setting)
		}
	}
}

// leaf builds a Setting for a single field, reporting false for types the
// registry does not render.
func leaf(field reflect.StructField, value, fallback reflect.Value, path, group string) (Setting, bool) {
	setting := Setting{
		Path:     path,
		Group:    group,
		Label:    label(field),
		Desc:     field.Tag.Get("desc"),
		value:    value,
		fallback: fallback,
	}

	target := value.Type()
	if target.Kind() == reflect.Pointer {
		setting.Optional = true
		target = target.Elem()
	}

	switch {
	case target.Kind() == reflect.Slice && target.Elem().Kind() == reflect.String:
		setting.Kind = KindStrings
	case field.Tag.Get("choices") != "":
		setting.Kind = KindChoice
		setting.Choices = strings.Split(field.Tag.Get("choices"), "|")
	case target.Kind() == reflect.Bool:
		setting.Kind = KindBool
	case target.Kind() == reflect.Int:
		setting.Kind = KindInt
	case target.Kind() == reflect.Float64:
		setting.Kind = KindFloat
	default:
		return Setting{}, false
	}

	// An optional field needs somewhere to express "no override". Enumerated
	// editors get an extra entry rather than a second widget; free-text editors
	// use the empty string.
	if setting.Optional {
		switch setting.Kind {
		case KindBool:
			setting.Kind = KindChoice
			setting.Choices = []string{Inherit, "true", "false"}
		case KindChoice:
			setting.Choices = append([]string{Inherit}, setting.Choices...)
		}
	}

	setting.Min, setting.HasMin = tagFloat(field, "min")
	setting.Max, setting.HasMax = tagFloat(field, "max")

	return setting, true
}

// String renders the current value. An unset optional setting reads "inherit".
func (s *Setting) String() string { return render(s.value) }

// Default renders the value Reset would restore.
func (s *Setting) Default() string { return render(s.fallback) }

// IsDefault reports whether the setting still holds its preset value, which is
// what an editor uses to decide whether a reset control is worth offering.
func (s *Setting) IsDefault() bool { return s.String() == s.Default() }

// Reset restores the preset value.
func (s *Setting) Reset() { assign(s.value, s.fallback) }

// Set parses text and stores it, leaving the setting untouched if the text is
// unusable or out of range. The error is written for a user reading it in a
// tooltip, not for a log.
func (s *Setting) Set(text string) error {
	text = strings.TrimSpace(text)

	if s.Optional && (text == "" || text == Inherit) {
		s.value.Set(reflect.Zero(s.value.Type()))
		return nil
	}

	switch s.Kind {
	case KindStrings:
		s.store(reflect.ValueOf(splitList(text)))
		return nil

	case KindChoice:
		for _, choice := range s.Choices {
			if choice != text || choice == Inherit {
				continue
			}
			// An optional bool is presented as a three-way choice so the UI
			// needs no separate tri-state widget, but it is still a bool
			// underneath and storing the literal text would panic.
			if s.elemKind() == reflect.Bool {
				parsed, err := strconv.ParseBool(text)
				if err != nil {
					return fmt.Errorf("must be true or false")
				}
				s.store(reflect.ValueOf(parsed))
				return nil
			}
			s.store(reflect.ValueOf(text))
			return nil
		}
		return fmt.Errorf("must be one of %s", strings.Join(s.usable(), ", "))

	case KindBool:
		parsed, err := strconv.ParseBool(text)
		if err != nil {
			return fmt.Errorf("must be true or false")
		}
		s.store(reflect.ValueOf(parsed))
		return nil

	case KindInt:
		parsed, err := strconv.Atoi(text)
		if err != nil {
			return fmt.Errorf("must be a whole number")
		}
		if err := s.inRange(float64(parsed)); err != nil {
			return err
		}
		s.store(reflect.ValueOf(parsed))
		return nil

	default: // KindFloat
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return fmt.Errorf("must be a number")
		}
		if err := s.inRange(parsed); err != nil {
			return err
		}
		s.store(reflect.ValueOf(parsed))
		return nil
	}
}

// elemKind is the kind of the value the setting writes, looking through the
// pointer an optional setting is held in.
func (s *Setting) elemKind() reflect.Kind {
	target := s.value.Type()
	if target.Kind() == reflect.Pointer {
		target = target.Elem()
	}

	return target.Kind()
}

// usable is Choices without the inherit sentinel, for error messages.
func (s *Setting) usable() []string {
	out := make([]string, 0, len(s.Choices))
	for _, choice := range s.Choices {
		if choice != Inherit {
			out = append(out, choice)
		}
	}

	return out
}

func (s *Setting) inRange(v float64) error {
	switch {
	case s.HasMin && s.HasMax && (v < s.Min || v > s.Max):
		return fmt.Errorf("must be between %s and %s", trim(s.Min), trim(s.Max))
	case s.HasMin && v < s.Min:
		return fmt.Errorf("must be at least %s", trim(s.Min))
	case s.HasMax && v > s.Max:
		return fmt.Errorf("must be at most %s", trim(s.Max))
	}

	return nil
}

// store writes a parsed value, allocating the pointer for an optional setting.
func (s *Setting) store(parsed reflect.Value) {
	if !s.Optional {
		s.value.Set(parsed.Convert(s.value.Type()))
		return
	}

	held := reflect.New(s.value.Type().Elem())
	held.Elem().Set(parsed.Convert(s.value.Type().Elem()))
	s.value.Set(held)
}

// Validate clamps every out-of-range value in a tuning table back to its
// default and returns a message for each, so a bad number is reported rather
// than silently producing a governor that behaves strangely.
func (t *Tuning) Validate(aggression string) []string {
	defaults := DefaultTuning(aggression)

	var problems []string
	for _, setting := range Settings(t, &defaults) {
		current := setting.String()
		if current == Inherit || setting.Kind == KindStrings {
			continue
		}

		if setting.Kind == KindChoice {
			if err := setting.Set(current); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %q %v; using %s",
					setting.Path, current, err, setting.Default()))
				setting.Reset()
			}
			continue
		}

		value, err := strconv.ParseFloat(current, 64)
		if err != nil {
			continue // bool
		}
		if err := setting.inRange(value); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s %v; using %s",
				setting.Path, current, err, setting.Default()))
			setting.Reset()
		}
	}
	sort.Strings(problems)

	return problems
}

// Groups returns the setting groups in first-seen order, which is declaration
// order — the order a reader of the struct would expect.
func Groups(settings []Setting) []string {
	var order []string
	seen := make(map[string]bool, len(settings))

	for i := range settings {
		if group := settings[i].Group; !seen[group] {
			seen[group] = true
			order = append(order, group)
		}
	}

	return order
}

// render prints a field value the way the config file and the UI both spell it.
func render(value reflect.Value) string {
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return Inherit
		}
		value = value.Elem()
	}

	switch value.Kind() {
	case reflect.Bool:
		return strconv.FormatBool(value.Bool())
	case reflect.Int:
		return strconv.FormatInt(value.Int(), 10)
	case reflect.Float64:
		// Plain decimal rather than %g: "1000000" is readable in a text field
		// and round-trips, where "1e+06" invites someone to retype it wrong.
		return strconv.FormatFloat(value.Float(), 'f', -1, 64)
	case reflect.String:
		return value.String()
	case reflect.Slice:
		parts := make([]string, value.Len())
		for i := range parts {
			parts[i] = value.Index(i).String()
		}
		return strings.Join(parts, ", ")
	}

	return ""
}

// assign copies a value, deep-copying the two reference types the schema uses
// so that a reset cannot alias the defaults.
func assign(target, source reflect.Value) {
	switch source.Kind() {
	case reflect.Pointer:
		if source.IsNil() {
			target.Set(reflect.Zero(target.Type()))
			return
		}
		held := reflect.New(source.Type().Elem())
		held.Elem().Set(source.Elem())
		target.Set(held)

	case reflect.Slice:
		if source.IsNil() {
			target.Set(reflect.Zero(target.Type()))
			return
		}
		copied := reflect.MakeSlice(source.Type(), source.Len(), source.Len())
		reflect.Copy(copied, source)
		target.Set(copied)

	default:
		target.Set(source)
	}
}

func splitList(text string) []string {
	var out []string
	for _, part := range strings.Split(text, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

func jsonName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if name, _, _ := strings.Cut(tag, ","); name != "" {
		return name
	}

	return field.Name
}

func tagFloat(field reflect.StructField, name string) (float64, bool) {
	tag := field.Tag.Get(name)
	if tag == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(tag, 64)

	return value, err == nil
}

// labelWords expands the abbreviations a config key uses into something a
// reader can scan. Everything else is passed through as a lowercase word.
var labelWords = map[string]string{
	"ms":  "(ms)",
	"s":   "(s)",
	"kb":  "(KB)",
	"cv":  "variation",
	"io":  "I/O",
	"cpu": "CPU",
	"gpu": "GPU",
	"qos": "QoS",
	"lo":  "lower bound",
	"hi":  "upper bound",
}

// label turns a config key into a display label, e.g. "poll_interval_ms" into
// "Poll interval (ms)". An explicit label tag wins.
func label(field reflect.StructField) string {
	if tag := field.Tag.Get("label"); tag != "" {
		return tag
	}

	words := strings.Split(jsonName(field), "_")
	for i, word := range words {
		if expanded, ok := labelWords[word]; ok {
			words[i] = expanded
		}
	}

	text := strings.Join(words, " ")
	if text == "" {
		return field.Name
	}

	return strings.ToUpper(text[:1]) + text[1:]
}

func trim(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
