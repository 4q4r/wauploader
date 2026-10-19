// Package config loads, validates, and overrides the wauploader TOML
// configuration. The single configuration file follows the XDG path
// ~/.config/wauploader/wauploader.toml; a missing file is a valid
// zero-configuration setup. Environment variables are never consulted.
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// AuthConfig holds archive.org credentials: IA-S3 keys or SPN2 cookies.
// Both key fields must be set together or both left empty.
type AuthConfig struct {
	S3Access   string `toml:"s3_access"`
	S3Secret   string `toml:"s3_secret"`
	CookieUser string `toml:"cookie_user"`
	CookieSig  string `toml:"cookie_sig"`
}

// SPN2Config mirrors the Save Page Now 2 capture options. Duration fields
// keep the raw string form; use ParseIATimedelta to convert them.
type SPN2Config struct {
	IfNotArchivedWithin  string `toml:"if_not_archived_within"`
	CaptureAll           bool   `toml:"capture_all"`
	CaptureOutlinks      bool   `toml:"capture_outlinks"`
	CaptureScreenshot    bool   `toml:"capture_screenshot"`
	DelayWBAvailability  bool   `toml:"delay_wb_availability"`
	ForceGet             bool   `toml:"force_get"`
	SkipFirstArchive     bool   `toml:"skip_first_archive"`
	OutlinksAvailability bool   `toml:"outlinks_availability"`
	EmailResult          bool   `toml:"email_result"`
	JSBehaviorTimeout    int    `toml:"js_behavior_timeout"`
	AuthMode             string `toml:"auth_mode"`
}

// UploadConfig configures IA-S3 item uploads. Collection and mediatype have
// no defaults: the user must state them or validation fails.
type UploadConfig struct {
	Collection          string `toml:"collection"`
	Mediatype           string `toml:"mediatype"`
	QueueDerive         bool   `toml:"queue_derive"`
	KeepOldVersion      bool   `toml:"keep_old_version"`
	InteractivePriority bool   `toml:"interactive_priority"`
}

// LimitsConfig bounds pacing and concurrency across save and upload runs.
type LimitsConfig struct {
	SavePerMinute      int    `toml:"save_per_minute"`
	SaveConcurrency    int    `toml:"save_concurrency"`
	UploadConcurrency  int    `toml:"upload_concurrency"`
	PerHostConcurrency int    `toml:"per_host_concurrency"`
	JobWait            string `toml:"job_wait"`
}

// ConsoleConfig is reserved for future console settings; color is handled by
// flags and the NO_COLOR convention, not by the config file.
type ConsoleConfig struct{}

// Config is the whole configuration file.
type Config struct {
	Auth    AuthConfig
	SPN2    SPN2Config
	Upload  UploadConfig
	Limits  LimitsConfig
	Console ConsoleConfig
}

// Mode selects which side of the tool a validation pass covers. The zero
// value is ModeAny, the strictest choice.
type Mode int

const (
	// ModeAny validates everything: save-side and upload-side rules.
	ModeAny Mode = iota
	// ModeSave validates rules relevant to Save Page Now 2 runs.
	ModeSave
	// ModeUpload validates rules relevant to IA-S3 upload runs.
	ModeUpload
)

// mediatypeValues is the archive.org mediatype enumeration.
var mediatypeValues = []string{"texts", "etree", "audio", "movies", "software", "image", "data", "web"}

// knownSections maps every valid TOML section to its valid keys. The console
// section is reserved and intentionally empty. DetectUnknownKeys diffs the
// parsed document against this table.
var knownSections = map[string]map[string]bool{
	"auth": {
		"s3_access":   true,
		"s3_secret":   true,
		"cookie_user": true,
		"cookie_sig":  true,
	},
	"spn2": {
		"if_not_archived_within": true,
		"capture_all":            true,
		"capture_outlinks":       true,
		"capture_screenshot":     true,
		"delay_wb_availability":  true,
		"force_get":              true,
		"skip_first_archive":     true,
		"outlinks_availability":  true,
		"email_result":           true,
		"js_behavior_timeout":    true,
		"auth_mode":              true,
	},
	"upload": {
		"collection":           true,
		"mediatype":            true,
		"queue_derive":         true,
		"keep_old_version":     true,
		"interactive_priority": true,
	},
	"limits": {
		"save_per_minute":      true,
		"save_concurrency":     true,
		"upload_concurrency":   true,
		"per_host_concurrency": true,
		"job_wait":             true,
	},
	"console": {},
}

// Default returns the configuration with every documented default applied.
func Default() *Config {
	return &Config{
		SPN2: SPN2Config{
			JSBehaviorTimeout: 5,
			AuthMode:          "auto",
		},
		Upload: UploadConfig{
			QueueDerive:    true,
			KeepOldVersion: true,
		},
		Limits: LimitsConfig{
			SavePerMinute:      5,
			SaveConcurrency:    4,
			UploadConcurrency:  2,
			PerHostConcurrency: 3,
			JobWait:            "20m",
		},
	}
}

// Load reads the TOML file at path. A missing file yields the defaults
// (zero-configuration is valid, not an error). Parse failures carry the
// TOML document position and a Russian message prefix.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return nil, fmt.Errorf("не удалось прочитать файл конфигурации %s: %w", path, err)
	}
	cfg := Default()
	if err := toml.Unmarshal(data, cfg); err != nil {
		var decodeErr *toml.DecodeError
		if errors.As(err, &decodeErr) {
			row, col := decodeErr.Position()
			return nil, fmt.Errorf("не удалось разобрать файл конфигурации %s (строка %d, столбец %d): %w\n%s",
				path, row, col, decodeErr, decodeErr.String())
		}
		return nil, fmt.Errorf("не удалось разобрать файл конфигурации %s: %w", path, err)
	}
	return cfg, nil
}

// DetectUnknownKeys reports key paths present in the file but absent from the
// known schema, as dotted paths such as "spn2.capture_al". Unknown sections
// are reported by name alone. A missing file yields no unknown keys.
func DetectUnknownKeys(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("не удалось прочитать файл конфигурации %s: %w", path, err)
	}
	var tree map[string]any
	if err := toml.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("не удалось разобрать файл конфигурации %s: %w", path, err)
	}
	var unknown []string
	for section, fields := range tree {
		keys, known := knownSections[section]
		if !known {
			unknown = append(unknown, section)
			continue
		}
		sub, isTable := fields.(map[string]any)
		if !isTable {
			unknown = append(unknown, section)
			continue
		}
		for key := range sub {
			if !keys[key] {
				unknown = append(unknown, section+"."+key)
			}
		}
	}
	sort.Strings(unknown)
	return unknown, nil
}

// maxIATimedeltaSeconds caps every parsed duration at 100 years so absurd
// magnitudes fail fast instead of silently overflowing time.Duration.
const maxIATimedeltaSeconds int64 = 100 * 365 * 24 * 3600

// ParseIATimedelta parses the Internet Archive timedelta form: a bare number
// means seconds ("120"), otherwise whitespace-separated number+unit tokens
// with units d, h, m, s ("3d 5h 20m"). Repeated units sum ("1d 2d" = 72h) and
// totals beyond 100 years are rejected.
func ParseIATimedelta(s string) (time.Duration, error) {
	const usage = `примеры допустимых форм "120" (секунды) и "3d 5h 20m" (единицы d, h, m, s)`
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("пустое значение длительности: %s", usage)
	}
	badFormat := func() error {
		return fmt.Errorf("недопустимое значение длительности %q: %s", s, usage)
	}
	overLimit := func() error {
		return fmt.Errorf("недопустимое значение длительности %q: значение превышает предел 100 лет (%s)", s, usage)
	}
	if seconds, err := strconv.Atoi(trimmed); err == nil {
		switch {
		case seconds < 0:
			return 0, badFormat()
		case int64(seconds) > maxIATimedeltaSeconds:
			return 0, overLimit()
		}
		return time.Duration(seconds) * time.Second, nil
	}
	var total int64
	for _, token := range strings.Fields(trimmed) {
		if len(token) < 2 {
			return 0, badFormat()
		}
		var unitSeconds int64
		switch token[len(token)-1] {
		case 'd':
			unitSeconds = 86400
		case 'h':
			unitSeconds = 3600
		case 'm':
			unitSeconds = 60
		case 's':
			unitSeconds = 1
		default:
			return 0, badFormat()
		}
		value, err := strconv.Atoi(token[:len(token)-1])
		if err != nil || value < 0 {
			return 0, badFormat()
		}
		// Division-first bound check: proves the multiply cannot overflow.
		if int64(value) > maxIATimedeltaSeconds/unitSeconds {
			return 0, overLimit()
		}
		total += int64(value) * unitSeconds
		if total > maxIATimedeltaSeconds {
			return 0, overLimit()
		}
	}
	return time.Duration(total) * time.Second, nil
}

// MergeFlag applies a flag-over-config mutation. Commands build a closure
// over their parsed flag values and hand it here, so flags win over the file.
func (c *Config) MergeFlag(fn func(*Config)) {
	fn(c)
}

// Validate checks the configuration against the rules that matter for mode.
// It fails fast: the first violated rule is returned as a Russian error.
func (c *Config) Validate(mode Mode) error {
	// Auth rules apply in every mode.
	if (c.Auth.S3Access == "") != (c.Auth.S3Secret == "") {
		return errors.New("s3_access и s3_secret должны быть заданы вместе (оба ключа или ни одного — тогда используются куки)")
	}
	switch c.SPN2.AuthMode {
	case "auto", "keys", "cookies":
	default:
		return fmt.Errorf("неизвестное значение auth_mode %q: допустимы auto, keys, cookies", c.SPN2.AuthMode)
	}

	saves := mode == ModeSave || mode == ModeAny
	if saves {
		if c.SPN2.IfNotArchivedWithin != "" {
			if _, err := ParseIATimedelta(c.SPN2.IfNotArchivedWithin); err != nil {
				return fmt.Errorf("недопустимое значение if_not_archived_within: %w", err)
			}
		}
		if c.SPN2.JSBehaviorTimeout < 0 || c.SPN2.JSBehaviorTimeout > 30 {
			return fmt.Errorf("js_behavior_timeout должен быть от 0 до 30 секунд, сейчас %d", c.SPN2.JSBehaviorTimeout)
		}
	}

	uploads := mode == ModeUpload || mode == ModeAny
	if uploads {
		if _, err := ParseIATimedelta(c.Limits.JobWait); err != nil {
			return fmt.Errorf("недопустимое значение job_wait: %w", err)
		}
		if c.Upload.Collection == "" {
			return errors.New("не задан обязательный параметр --collection: загружаемые элементы должны принадлежать коллекции archive.org")
		}
		allowed := strings.Join(mediatypeValues, ", ")
		if c.Upload.Mediatype == "" {
			return fmt.Errorf("не задан обязательный параметр mediatype: допустимые значения %s", allowed)
		}
		valid := false
		for _, mt := range mediatypeValues {
			if c.Upload.Mediatype == mt {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("недопустимое значение mediatype %q: допустимые значения %s", c.Upload.Mediatype, allowed)
		}
	}
	return nil
}
