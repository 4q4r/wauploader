package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// writeTempConfig writes content to a temporary file and returns its path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wauploader.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestDefaultValues(t *testing.T) {
	cfg := Default()

	if cfg.SPN2.JSBehaviorTimeout != 5 {
		t.Errorf("JSBehaviorTimeout = %d, want 5", cfg.SPN2.JSBehaviorTimeout)
	}
	if cfg.SPN2.AuthMode != "auto" {
		t.Errorf("AuthMode = %q, want \"auto\"", cfg.SPN2.AuthMode)
	}
	if !cfg.Upload.QueueDerive {
		t.Error("QueueDerive = false, want true")
	}
	if !cfg.Upload.KeepOldVersion {
		t.Error("KeepOldVersion = false, want true")
	}
	if cfg.Upload.InteractivePriority {
		t.Error("InteractivePriority = true, want false")
	}
	if cfg.Upload.Collection != "" || cfg.Upload.Mediatype != "" {
		t.Errorf("Collection/Mediatype must have no defaults, got %q/%q",
			cfg.Upload.Collection, cfg.Upload.Mediatype)
	}
	if cfg.Limits.SavePerMinute != 5 {
		t.Errorf("SavePerMinute = %d, want 5", cfg.Limits.SavePerMinute)
	}
	if cfg.Limits.SaveConcurrency != 4 {
		t.Errorf("SaveConcurrency = %d, want 4", cfg.Limits.SaveConcurrency)
	}
	if cfg.Limits.UploadConcurrency != 2 {
		t.Errorf("UploadConcurrency = %d, want 2", cfg.Limits.UploadConcurrency)
	}
	if cfg.Limits.PerHostConcurrency != 3 {
		t.Errorf("PerHostConcurrency = %d, want 3", cfg.Limits.PerHostConcurrency)
	}
	if cfg.Limits.JobWait != "20m" {
		t.Errorf("JobWait = %q, want \"20m\"", cfg.Limits.JobWait)
	}
	if cfg.SPN2.IfNotArchivedWithin != "" {
		t.Errorf("IfNotArchivedWithin = %q, want empty (server default applies)", cfg.SPN2.IfNotArchivedWithin)
	}
}

func TestValidateUploadRequiresCollection(t *testing.T) {
	cfg := Default()
	cfg.Upload.Collection = ""

	err := cfg.Validate(ModeUpload)
	if err == nil {
		t.Fatal("Validate(ModeUpload) without collection = nil, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--collection") {
		t.Errorf("error %q must name the --collection flag", msg)
	}
	if !strings.Contains(msg, "коллекц") {
		t.Errorf("error %q must explain items must belong to a collection", msg)
	}

	// ModeAny inherits the upload-side requirement.
	if err := cfg.Validate(ModeAny); err == nil {
		t.Error("Validate(ModeAny) without collection = nil, want error")
	}
}

func TestValidateMediatypeEnum(t *testing.T) {
	valid := []string{"texts", "etree", "audio", "movies", "software", "image", "data", "web"}
	for _, mt := range valid {
		cfg := Default()
		cfg.Upload.Collection = "test-collection"
		cfg.Upload.Mediatype = mt
		if err := cfg.Validate(ModeUpload); err != nil {
			t.Errorf("Validate with mediatype %q = %v, want nil", mt, err)
		}
	}

	cfg := Default()
	cfg.Upload.Collection = "test-collection"
	cfg.Upload.Mediatype = "video"
	err := cfg.Validate(ModeUpload)
	if err == nil {
		t.Fatal("Validate with mediatype \"video\" = nil, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mediatype") {
		t.Errorf("error %q must name the mediatype parameter", msg)
	}
	for _, want := range valid {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must list allowed value %q", msg, want)
		}
	}

	// Empty mediatype is required-and-missing, not a silent pass.
	empty := Default()
	empty.Upload.Collection = "test-collection"
	empty.Upload.Mediatype = ""
	if err := empty.Validate(ModeUpload); err == nil {
		t.Error("Validate with empty mediatype = nil, want error")
	}
}

func TestValidateKeysPaired(t *testing.T) {
	// Upload-side fields are satisfied so the pairing rule is the only thing
	// that can fail here — the assertion must not depend on rule ordering.
	onlyAccess := Default()
	onlyAccess.Upload.Collection = "test-collection"
	onlyAccess.Upload.Mediatype = "data"
	onlyAccess.Auth.S3Access = "AKIAEXAMPLE"
	err := onlyAccess.Validate(ModeUpload)
	if err == nil {
		t.Fatal("s3_access without s3_secret = nil, want error")
	}
	if !strings.Contains(err.Error(), "s3_access") {
		t.Errorf("error %q must be the key-pairing error naming s3_access", err)
	}

	onlySecret := Default()
	onlySecret.Auth.S3Secret = "secret"
	if err := onlySecret.Validate(ModeSave); err == nil {
		t.Error("s3_secret without s3_access = nil, want error")
	}

	both := Default()
	both.Auth.S3Access = "AKIAEXAMPLE"
	both.Auth.S3Secret = "secret"
	if err := both.Validate(ModeSave); err != nil {
		t.Errorf("both keys set = %v, want nil", err)
	}

	neither := Default()
	if err := neither.Validate(ModeSave); err != nil {
		t.Errorf("no keys set = %v, want nil (cookies are a valid auth source)", err)
	}
}

func TestLoadUnknownKeyReported(t *testing.T) {
	path := writeTempConfig(t, `
[spn2]
capture_all = true
capture_al = true
`)
	unknown, err := DetectUnknownKeys(path)
	if err != nil {
		t.Fatalf("DetectUnknownKeys: %v", err)
	}
	found := false
	for _, key := range unknown {
		if key == "spn2.capture_al" {
			found = true
		}
		if key == "spn2.capture_all" {
			t.Errorf("known key %q must not be reported as unknown", key)
		}
	}
	if !found {
		t.Errorf("unknown keys %v must contain \"spn2.capture_al\"", unknown)
	}
}

func TestLoadParseErrorHasPosition(t *testing.T) {
	path := writeTempConfig(t, "[spn2\ncapture_all =\n")

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with malformed TOML = nil, want error")
	}

	var decodeErr *toml.DecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("error %v must wrap a *toml.DecodeError", err)
	}
	row, column := decodeErr.Position()
	if row < 1 || column < 1 {
		t.Errorf("position (%d, %d) must be 1-indexed and positive", row, column)
	}
	if !strings.Contains(err.Error(), "не удалось разобрать") {
		t.Errorf("error %q must carry the Russian parse-failure prefix", err)
	}
	// The user-visible message itself must carry the position, not only the
	// wrapped DecodeError internals.
	for _, want := range []string{"строка", fmt.Sprintf("строка %d", row), fmt.Sprintf("столбец %d", column)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err, want)
		}
	}
	if decodeErr.String() == "" {
		t.Error("DecodeError.String() contextualized view must not be empty")
	}
}

func TestParseIATimedeltaForms(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"120", 120 * time.Second},
		{"2147483648", 2147483648 * time.Second},
		{"3d 5h 20m", 3*24*time.Hour + 5*time.Hour + 20*time.Minute},
		{"1d", 24 * time.Hour},
		{"45s", 45 * time.Second},
		{"0", 0},
	}
	for _, tc := range cases {
		got, err := ParseIATimedelta(tc.input)
		if err != nil {
			t.Errorf("ParseIATimedelta(%q) = error %v, want %v", tc.input, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseIATimedelta(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}

	got, err := ParseIATimedelta("2x")
	if err == nil {
		t.Errorf("ParseIATimedelta(\"2x\") = %v, want error", got)
	} else {
		msg := err.Error()
		for _, want := range []string{"3d 5h 20m", "120"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q must include the example form %q", msg, want)
			}
		}
	}

	if got, err := ParseIATimedelta(""); got != 0 || err == nil {
		t.Errorf("ParseIATimedelta(\"\") = (%v, %v), want (0, error)", got, err)
	}
}

func TestDetectUnknownKeysReportsUnknownSection(t *testing.T) {
	path := writeTempConfig(t, "[spam2]\nfoo = true\n")
	unknown, err := DetectUnknownKeys(path)
	if err != nil {
		t.Fatalf("DetectUnknownKeys: %v", err)
	}
	found := false
	for _, key := range unknown {
		if key == "spam2" {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown keys %v must contain the unknown section \"spam2\"", unknown)
	}
}

// TestKnownSectionsCoversEveryStructKey guards against drift between the
// struct TOML tags and the hand-maintained knownSections table: the two must
// name exactly the same sections and keys, every struct field must carry an
// explicit tag, and a generated document holding every known key must load
// cleanly (Load succeeds, DetectUnknownKeys silent, every value round-trips).
func TestKnownSectionsCoversEveryStructKey(t *testing.T) {
	sectionTypes := map[string]reflect.Type{
		"auth":    reflect.TypeOf(AuthConfig{}),
		"spn2":    reflect.TypeOf(SPN2Config{}),
		"upload":  reflect.TypeOf(UploadConfig{}),
		"limits":  reflect.TypeOf(LimitsConfig{}),
		"console": reflect.TypeOf(ConsoleConfig{}),
	}
	if len(sectionTypes) != len(knownSections) {
		t.Fatalf("section count drift: %d struct sections vs %d knownSections entries",
			len(sectionTypes), len(knownSections))
	}
	for name, typ := range sectionTypes {
		known, ok := knownSections[name]
		if !ok {
			t.Errorf("struct section %q missing from knownSections", name)
			continue
		}
		tags := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			tag := typ.Field(i).Tag.Get("toml")
			if tag == "" {
				t.Errorf("section %q field %s has no toml tag", name, typ.Field(i).Name)
				continue
			}
			tags[tag] = true
		}
		for tag := range tags {
			if !known[tag] {
				t.Errorf("struct tag %q (section %s) missing from knownSections", tag, name)
			}
		}
		for key := range known {
			if !tags[key] {
				t.Errorf("knownSections key %q (section %s) has no matching struct tag", key, name)
			}
		}
	}

	placeholder := func(typ reflect.Type, key string) string {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.Tag.Get("toml") != key {
				continue
			}
			switch field.Type.Kind() {
			case reflect.Bool:
				return "true"
			case reflect.Int:
				return "1"
			default:
				return `"example-` + key + `"`
			}
		}
		return ""
	}
	var doc strings.Builder
	for _, name := range []string{"auth", "spn2", "upload", "limits", "console"} {
		fmt.Fprintf(&doc, "[%s]\n", name)
		keys := make([]string, 0, len(knownSections[name]))
		for key := range knownSections[name] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&doc, "%s = %s\n", key, placeholder(sectionTypes[name], key))
		}
	}

	path := writeTempConfig(t, doc.String())
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load of the all-keys document failed: %v", err)
	}
	unknown, err := DetectUnknownKeys(path)
	if err != nil {
		t.Fatalf("DetectUnknownKeys: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("DetectUnknownKeys on the all-keys document = %v, want empty", unknown)
	}

	// Every key must round-trip into the field its struct tag names, proving
	// go-toml maps the lowercase section/key names used by knownSections.
	cfgValue := reflect.ValueOf(cfg).Elem()
	sectionIndex := map[string]int{}
	for i := 0; i < cfgValue.NumField(); i++ {
		sectionIndex[strings.ToLower(cfgValue.Type().Field(i).Name)] = i
	}
	for name, typ := range sectionTypes {
		section := cfgValue.Field(sectionIndex[name])
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			key := field.Tag.Get("toml")
			value := section.Field(i)
			switch field.Type.Kind() {
			case reflect.Bool:
				if !value.Bool() {
					t.Errorf("%s.%s did not round-trip: got false, want true", name, key)
				}
			case reflect.Int:
				if value.Int() != 1 {
					t.Errorf("%s.%s did not round-trip: got %d, want 1", name, key, value.Int())
				}
			default:
				if want := "example-" + key; value.String() != want {
					t.Errorf("%s.%s did not round-trip: got %q, want %q", name, key, value.String(), want)
				}
			}
		}
	}
}

func TestParseIATimedeltaRejectsOverflow(t *testing.T) {
	for _, bad := range []string{"999999999d", "500000000h", "9999999999999999", "36000d 36000d"} {
		got, err := ParseIATimedelta(bad)
		if err == nil {
			t.Errorf("ParseIATimedelta(%q) = %v, want error (absurd magnitudes must fail, not wrap)", bad, got)
			continue
		}
		if got < 0 {
			t.Errorf("ParseIATimedelta(%q) = %v, negative results leak the overflow", bad, got)
		}
	}
}

func TestParseIATimedeltaEdgeSemantics(t *testing.T) {
	// "1d5h" without whitespace violates the space-separated grammar.
	if got, err := ParseIATimedelta("1d5h"); err == nil {
		t.Errorf("ParseIATimedelta(\"1d5h\") = %v, want error", got)
	}
	// "+120" is accepted: strconv.Atoi sign semantics, pinned here on purpose.
	if got, err := ParseIATimedelta("+120"); err != nil || got != 120*time.Second {
		t.Errorf("ParseIATimedelta(\"+120\") = (%v, %v), want (2m0s, nil)", got, err)
	}
	// Repeated units sum, pinned so Task 6 does not assume strictness.
	if got, err := ParseIATimedelta("1d 2d"); err != nil || got != 72*time.Hour {
		t.Errorf("ParseIATimedelta(\"1d 2d\") = (%v, %v), want (72h0m0s, nil)", got, err)
	}
}

func TestValidateJSBehaviorTimeoutBounds(t *testing.T) {
	for _, ok := range []int{0, 5, 30} {
		cfg := Default()
		cfg.SPN2.JSBehaviorTimeout = ok
		if err := cfg.Validate(ModeSave); err != nil {
			t.Errorf("JSBehaviorTimeout %d = %v, want nil", ok, err)
		}
	}
	for _, bad := range []int{-1, 31} {
		cfg := Default()
		cfg.SPN2.JSBehaviorTimeout = bad
		if err := cfg.Validate(ModeSave); err == nil {
			t.Errorf("JSBehaviorTimeout %d = nil, want error", bad)
		}
	}
}

func TestValidateDurationFormatRejection(t *testing.T) {
	badWait := Default()
	badWait.Limits.JobWait = "forever"
	err := badWait.Validate(ModeUpload)
	if err == nil {
		t.Fatal("job_wait \"forever\" = nil, want error")
	}
	for _, want := range []string{"3d 5h 20m", "120"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("job_wait error %q must include example %q", err, want)
		}
	}

	badDedup := Default()
	badDedup.SPN2.IfNotArchivedWithin = "2x"
	if err := badDedup.Validate(ModeSave); err == nil {
		t.Error("if_not_archived_within \"2x\" = nil, want error")
	}
}

func TestValidateModeScoping(t *testing.T) {
	// Save mode does not demand upload-side fields.
	cfg := Default()
	if err := cfg.Validate(ModeSave); err != nil {
		t.Errorf("Validate(ModeSave) on defaults = %v, want nil", err)
	}
}

func TestLoadMissingFileDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.toml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(absent) = %v, want nil (zero-config is not an error)", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("Load(absent) = %+v, want defaults %+v", cfg, Default())
	}
}

func TestLoadRoundTripAndMergeFlag(t *testing.T) {
	path := writeTempConfig(t, `
[auth]
s3_access = "AKIAEXAMPLE"
s3_secret = "example-secret"

[upload]
collection = "example-collection"
mediatype = "data"
queue_derive = false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.S3Access != "AKIAEXAMPLE" || cfg.Auth.S3Secret != "example-secret" {
		t.Errorf("auth round-trip failed: %+v", cfg.Auth)
	}
	if cfg.Upload.Collection != "example-collection" || cfg.Upload.Mediatype != "data" {
		t.Errorf("upload round-trip failed: %+v", cfg.Upload)
	}
	if cfg.Upload.QueueDerive {
		t.Error("queue_derive = false in file overrode the default true")
	}
	if !cfg.Upload.KeepOldVersion {
		t.Error("KeepOldVersion default true must survive partial file")
	}
	if err := cfg.Validate(ModeUpload); err != nil {
		t.Errorf("loaded config must validate: %v", err)
	}

	cfg.MergeFlag(func(c *Config) {
		c.Limits.SavePerMinute = 7
	})
	if cfg.Limits.SavePerMinute != 7 {
		t.Errorf("MergeFlag did not apply: SavePerMinute = %d, want 7", cfg.Limits.SavePerMinute)
	}
}
