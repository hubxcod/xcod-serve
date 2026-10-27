// Package i18n translates the panel. The English text is the key: a language file
// (locales/<lang>.json) maps each English string to its translation, and anything missing
// falls back to English. Formats keep the fmt verbs (%s, %d, %q) in the same order.
//
// Errors meant for the panel are created with Errorf, which keeps the format and arguments so
// they can be translated when shown (Text), while Error() still returns the English text for
// logs.
package i18n

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Default is the language used when none is chosen.
const Default = "en"

// Lang is a language offered in the panel.
type Lang struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Langs lists the panel languages, in menu order.
var Langs = []Lang{{"en", "English"}, {"pt-BR", "Português (Brasil)"}}

//go:embed locales/*.json
var localesFS embed.FS

var catalogs = func() map[string]map[string]string {
	out := map[string]map[string]string{}
	files, _ := fs.Glob(localesFS, "locales/*.json")
	for _, f := range files {
		raw, err := localesFS.ReadFile(f)
		if err != nil {
			panic(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			panic(fmt.Sprintf("i18n: %s: %v", f, err))
		}
		out[strings.TrimSuffix(strings.TrimPrefix(f, "locales/"), ".json")] = m
	}
	return out
}()

// Supported normalizes a language code; unknown codes give Default.
func Supported(code string) string {
	for _, l := range Langs {
		if strings.EqualFold(l.Code, code) {
			return l.Code
		}
	}
	return Default
}

// T translates a fixed string.
func T(lang, s string) string {
	if v, ok := catalogs[lang][s]; ok && v != "" {
		return v
	}
	return s
}

// Sprintf translates a format and fills it in.
func Sprintf(lang, format string, args ...any) string {
	return fmt.Sprintf(T(lang, format), args...)
}

// Catalog returns the translations of a language (for the panel's JavaScript).
func Catalog(lang string) map[string]string { return catalogs[lang] }

// Msg is a translatable message: an English format and its arguments.
type Msg struct {
	Format string
	Args   []any
}

func (m Msg) Error() string { return fmt.Sprintf(m.Format, m.Args...) }

// Errorf creates an error that the panel can show translated.
func Errorf(format string, args ...any) error { return Msg{Format: format, Args: args} }

// Text returns err translated: a Msg is translated with its arguments; any other error is
// looked up by its full text.
func Text(lang string, err error) string {
	if err == nil {
		return ""
	}
	var m Msg
	if errors.As(err, &m) {
		return Sprintf(lang, m.Format, m.Args...)
	}
	return T(lang, err.Error())
}
