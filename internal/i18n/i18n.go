// Package i18n is how every sentence a person reads leaves the CLI.
//
// Use it like this, never with a string literal in a command:
//
//	fmt.Fprintln(out, i18n.T("listen.ready", i18n.Args{"badge": "[TEST]", "secret": s}))
//
// A catalog value holds {name} placeholders and T fills them from Args. Two
// catalogs ship, en.json and es.json (embedded), and a test holds their key
// sets equal, every placeholder present on both sides, and the characters
// banned from copy (em dash, en dash, middle dot, the delivery provider's name)
// out of both. Commands, flags and JSON keys are NOT translated.
//
// To add a message: put the key in BOTH files, group it under your area's
// prefix (login., listen., logs., deliveries., simulate., update., trigger.),
// and keep each file sorted by key, so two lanes adding keys under different
// prefixes never touch the same lines. Spanish is Mexican, informal (tu).
//
// The language is chosen once, in the root command, by Detect (the rule of
// section 4.7.1) and set with SetLang. T before SetLang answers English.
package i18n

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed en.json es.json
var files embed.FS

// Supported languages, in the spelling --lang and LISTO_LANG take.
const (
	English = "en"
	Spanish = "es"
)

// Args fills the {name} placeholders of a catalog value.
type Args map[string]any

var (
	mu       sync.RWMutex
	current  = English
	catalogs = mustLoad()
)

func mustLoad() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, lang := range []string{English, Spanish} {
		m, err := load(lang)
		if err != nil {
			panic(err)
		}
		out[lang] = m
	}
	return out
}

func load(lang string) (map[string]string, error) {
	raw, err := files.ReadFile(lang + ".json")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("i18n: %s.json: %w", lang, err)
	}
	return m, nil
}

// SetLang selects the output language. Anything but "es" selects English.
func SetLang(lang string) {
	mu.Lock()
	defer mu.Unlock()
	if lang == Spanish {
		current = Spanish
	} else {
		current = English
	}
}

// Lang is the language T is answering in: "es" or "en".
func Lang() string {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// AcceptLanguage is the Accept-Language the API client sends so the server's
// error messages and doc_url arrive in the same language: "es-MX" or "en".
func AcceptLanguage() string {
	if Lang() == Spanish {
		return "es-MX"
	}
	return "en"
}

// T returns the message for key in the current language with its placeholders
// filled. An unknown key answers the key itself, which is visible on purpose
// (a test fails first); a placeholder with no argument is left as written.
func T(key string, args ...Args) string {
	mu.RLock()
	lang := current
	mu.RUnlock()
	return render(lang, key, args...)
}

// TIn is T for an explicit language, for tests and for a message that must go
// out in a language other than the running one.
func TIn(lang, key string, args ...Args) string {
	if lang != Spanish {
		lang = English
	}
	return render(lang, key, args...)
}

func render(lang, key string, args ...Args) string {
	msg, ok := catalogs[lang][key]
	if !ok {
		msg, ok = catalogs[English][key]
		if !ok {
			return key
		}
	}
	if len(args) == 0 {
		return msg
	}
	var pairs []string
	for _, a := range args {
		for k, v := range a {
			pairs = append(pairs, "{"+k+"}", fmt.Sprint(v))
		}
	}
	return strings.NewReplacer(pairs...).Replace(msg)
}

// Has reports whether key exists in the English catalog (the reference one).
func Has(key string) bool {
	_, ok := catalogs[English][key]
	return ok
}

// Keys lists a language's keys, sorted. For tests and tools.
func Keys(lang string) []string {
	out := make([]string, 0, len(catalogs[lang]))
	for k := range catalogs[lang] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Env is what Detect reads from the process: environment variables and, on a
// Mac, the system locale. Tests pass fakes.
type Env struct {
	Getenv      func(string) string
	GOOS        string
	AppleLocale func() string
}

// Detect applies section 4.7.1: the --lang flag, else LISTO_LANG, else
// LC_ALL, LC_MESSAGES, LANG (the first one set), else on macOS the system's
// AppleLocale. A value starting with "es" is Spanish, anything else English.
// Pass an empty flag when --lang was not given.
func Detect(flag string, env Env) string {
	if flag != "" {
		return normalize(flag)
	}
	for _, name := range []string{"LISTO_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.TrimSpace(env.Getenv(name)); v != "" {
			return normalize(v)
		}
	}
	if env.GOOS == "darwin" && env.AppleLocale != nil {
		if v := strings.TrimSpace(env.AppleLocale()); v != "" {
			return normalize(v)
		}
	}
	return English
}

func normalize(v string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "es") {
		return Spanish
	}
	return English
}

// ValidFlag reports whether v is a value --lang accepts.
func ValidFlag(v string) bool { return v == English || v == Spanish }

// SystemEnv is the Env of the running process, with the macOS lookup wired to
// `defaults read -g AppleLocale` under a two second timeout (a Mac with no such
// key, or a stuck defaults, must never hang a command).
func SystemEnv(getenv func(string) string) Env {
	return Env{Getenv: getenv, GOOS: runtime.GOOS, AppleLocale: appleLocale}
}

func appleLocale() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "defaults", "read", "-g", "AppleLocale").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
