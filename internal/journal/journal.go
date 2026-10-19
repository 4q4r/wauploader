// Package journal implements the wauploader resume store: a bbolt-backed
// journal of save/upload record states with versioned records and a
// single-writer file lock. One journal record per unit of work (an SPN2 save
// target or an IA-S3 upload item), keyed by hex(sha256(kind + "\x00" + target)).
package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// Kind selects the journal bucket and the work type a record describes.
type Kind string

// Supported record kinds; each kind has its own bbolt bucket.
const (
	KindSave   Kind = "save"
	KindUpload Kind = "upload"
)

// State is the record lifecycle state driven by the runner state machines.
type State string

// Supported record states.
const (
	StateQueued      State = "queued"
	StateSubmitted   State = "submitted"
	StatePending     State = "pending"
	StateDone        State = "done"
	StateFailed      State = "failed"
	StateSkipped     State = "skipped"
	StateQuotaParked State = "quota_parked"
)

// schemaVersion is the record schema version written by this build. It is
// stored as the first byte of every value; a record carrying an unknown
// version must fail loudly (a newer/older binary wrote this journal), never
// be read silently.
const schemaVersion uint8 = 1

// lockTimeout bounds how long Open waits for the bbolt flock before
// reporting the journal as busy. A batch CLI should fail fast, not queue
// behind another wauploader process.
const lockTimeout = 500 * time.Millisecond

// Record is one journal entry: the full state needed to resume or report a
// single unit of work.
type Record struct {
	// Key is hex(sha256(kind + "\x00" + target)); derived, never trusted
	// from the caller. save target = URL, upload target = abs path +
	// "\x00" + identifier + "/" + key.
	Key string
	// Kind selects the bucket ("save" or "upload").
	Kind Kind
	// Target is the caller-facing unit of work (URL or composite upload path).
	Target string

	State State
	// Attempts counts runner retries for this record.
	Attempts int

	SPN2JobID         string
	SnapshotURL       string
	SnapshotTimestamp string

	MD5    string
	SHA256 string
	Size   int64

	ErrKind string
	ErrMsg  string

	// UpdatedAt is maintained by Put/UpdateState when left zero.
	UpdatedAt time.Time

	// SchemaVersion is the on-disk record schema version (currently 1).
	SchemaVersion uint8
}

// Store is a handle to the bbolt journal file. Safe for concurrent use; bbolt
// serializes writers internally, and the file itself is flock-protected so a
// second wauploader process cannot open it concurrently.
type Store struct {
	db *bolt.DB
}

// bucket names, one per Kind.
var bucketNames = map[Kind][]byte{
	KindSave:   []byte("save"),
	KindUpload: []byte("upload"),
}

var allStates = map[State]bool{
	StateQueued:      true,
	StateSubmitted:   true,
	StatePending:     true,
	StateDone:        true,
	StateFailed:      true,
	StateSkipped:     true,
	StateQuotaParked: true,
}

// errRecordBusy is the contract error returned when the journal file is
// flock-held by another wauploader process.
var errRecordBusy = errors.New("журнал занят другим процессом wauploader")

// KeyFor derives the stable bucket key for a (kind, target) pair:
// hex(sha256(kind + "\x00" + target)). Callers must use this instead of
// re-implementing the hash so keys stay stable across versions.
func KeyFor(kind Kind, target string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + target))
	return hex.EncodeToString(sum[:])
}

// Open opens (creating if needed) the journal file at path and ensures the
// per-kind buckets exist. The bbolt flock enforces a single writer process:
// if another process holds the journal, Open fails after lockTimeout with the
// Russian "busy" error instead of hanging.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("не удалось создать каталог журнала: %w", err)
		}
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: lockTimeout})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, fmt.Errorf("%w: %s", errRecordBusy, path)
		}

		return nil, fmt.Errorf("не удалось открыть журнал %s: %w", path, err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range bucketNames {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("не удалось создать секцию журнала %q: %w", name, err)
			}
		}

		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("не удалось подготовить журнал %s: %w", path, err)
	}

	return &Store{db: db}, nil
}

// Close releases the journal file and its flock.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}

	return s.db.Close()
}

// recordBody is the gob-encoded payload of a version-1 record. SchemaVersion
// travels as the first byte of the stored value, outside the payload, so the
// version can be checked before any decode attempt. A future schema bump adds
// a new body type and a decode branch in this file only.
type recordBody struct {
	Kind              Kind
	Target            string
	State             State
	Attempts          int
	SPN2JobID         string
	SnapshotURL       string
	SnapshotTimestamp string
	MD5               string
	SHA256            string
	Size              int64
	ErrKind           string
	ErrMsg            string
	UpdatedAt         time.Time
}

// encodeRecord marshals r behind the single marshal pair: version byte + gob
// payload. r.Key must already be derived by the caller.
func encodeRecord(r *Record) ([]byte, error) {
	body := recordBody{
		Kind:              r.Kind,
		Target:            r.Target,
		State:             r.State,
		Attempts:          r.Attempts,
		SPN2JobID:         r.SPN2JobID,
		SnapshotURL:       r.SnapshotURL,
		SnapshotTimestamp: r.SnapshotTimestamp,
		MD5:               r.MD5,
		SHA256:            r.SHA256,
		Size:              r.Size,
		ErrKind:           r.ErrKind,
		ErrMsg:            r.ErrMsg,
		UpdatedAt:         r.UpdatedAt,
	}

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&body); err != nil {
		return nil, fmt.Errorf("журнал: не удалось закодировать запись: %w", err)
	}

	return append([]byte{schemaVersion}, buf.Bytes()...), nil
}

// decodeRecord unmarshals the stored value behind the single deserialize
// pair, rejecting unknown schema versions with an explicit Russian error.
func decodeRecord(val []byte) (*Record, error) {
	if len(val) == 0 {
		return nil, errors.New("журнал: пустое значение записи")
	}

	version := val[0]
	if version != schemaVersion {
		return nil, fmt.Errorf("журнал: запись с неизвестной версией схемы %d (поддерживается %d); автоматическая миграция не поддерживается — используйте версию wauploader, записавшую этот журнал", version, schemaVersion)
	}

	var body recordBody
	if err := gob.NewDecoder(bytes.NewReader(val[1:])).Decode(&body); err != nil {
		return nil, fmt.Errorf("журнал: не удалось разобрать запись: %w", err)
	}

	return &Record{
		Key:               KeyFor(body.Kind, body.Target),
		Kind:              body.Kind,
		Target:            body.Target,
		State:             body.State,
		Attempts:          body.Attempts,
		SPN2JobID:         body.SPN2JobID,
		SnapshotURL:       body.SnapshotURL,
		SnapshotTimestamp: body.SnapshotTimestamp,
		MD5:               body.MD5,
		SHA256:            body.SHA256,
		Size:              body.Size,
		ErrKind:           body.ErrKind,
		ErrMsg:            body.ErrMsg,
		UpdatedAt:         body.UpdatedAt,
		SchemaVersion:     version,
	}, nil
}

// Put stores r under its derived key, forcing the schema version and the
// UpdatedAt stamp on the STORED copy — the caller's record is never written
// to. Kind and State are validated fail-fast.
func (s *Store) Put(r *Record) error {
	if err := validateKind(r.Kind); err != nil {
		return err
	}

	if err := validateState(r.State); err != nil {
		return err
	}

	stored := *r

	stored.Key = KeyFor(stored.Kind, stored.Target)
	if stored.UpdatedAt.IsZero() {
		stored.UpdatedAt = time.Now()
	}

	stored.SchemaVersion = schemaVersion

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNames[stored.Kind])
		if b == nil {
			return fmt.Errorf("журнал: отсутствует бакет %q", stored.Kind)
		}

		encoded, err := encodeRecord(&stored)
		if err != nil {
			return err
		}

		return b.Put([]byte(stored.Key), encoded)
	})
}

// Get returns the record for (kind, target). found=false for an absent key is
// not an error.
func (s *Store) Get(kind Kind, target string) (*Record, bool, error) {
	if err := validateKind(kind); err != nil {
		return nil, false, err
	}

	var (
		rec   *Record
		found bool
	)

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNames[kind])
		if b == nil {
			return fmt.Errorf("журнал: отсутствует бакет %q", kind)
		}

		val := b.Get([]byte(KeyFor(kind, target)))
		if val == nil {
			return nil
		}

		decoded, err := decodeRecord(val)
		if err != nil {
			return err
		}

		rec = decoded
		found = true

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	return rec, found, nil
}

// UpdateState loads the record for (kind, target), sets its state, applies
// the optional mutate hook, stamps UpdatedAt and persists — all inside one
// bbolt write transaction. An unknown record is a caller bug and errors out;
// it is never created implicitly.
func (s *Store) UpdateState(kind Kind, target string, st State, mutate func(*Record)) error {
	if err := validateKind(kind); err != nil {
		return err
	}

	if err := validateState(st); err != nil {
		return err
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNames[kind])
		if b == nil {
			return fmt.Errorf("журнал: отсутствует бакет %q", kind)
		}

		key := []byte(KeyFor(kind, target))

		val := b.Get(key)
		if val == nil {
			return fmt.Errorf("запись не найдена в журнале (тип %s): %s", kind, target)
		}

		rec, err := decodeRecord(val)
		if err != nil {
			return err
		}

		rec.State = st
		if mutate != nil {
			mutate(rec)
		}
		// Re-validate after the hook: a mutate may not persist an invalid
		// state, flip the kind (bucket misfiling), or change the key.
		if err := validateState(rec.State); err != nil {
			return err
		}

		if err := validateKind(rec.Kind); err != nil {
			return err
		}

		if rec.Kind != kind {
			return fmt.Errorf("изменение типа записи запрещено (тип %s): %s", kind, target)
		}

		if rec.Key != string(key) {
			return fmt.Errorf("изменение ключа записи запрещено: %s", target)
		}

		rec.UpdatedAt = time.Now()
		rec.SchemaVersion = schemaVersion

		encoded, err := encodeRecord(rec)
		if err != nil {
			return err
		}

		return b.Put(key, encoded)
	})
}

// Delete removes the record stored for kind+target and reports whether a
// record existed. Added for the `jobs --clear` surface (Task 12): no other
// mutation surface changed.
func (s *Store) Delete(kind Kind, target string) (bool, error) {
	if err := validateKind(kind); err != nil {
		return false, err
	}

	key := KeyFor(kind, target)

	var existed bool

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNames[kind])
		if b == nil {
			return fmt.Errorf("секция журнала %q отсутствует", bucketNames[kind])
		}

		existed = b.Get([]byte(key)) != nil

		return b.Delete([]byte(key))
	})
	if err != nil {
		return false, err
	}

	return existed, nil
}

// ByStates scans the given kinds' buckets and returns records whose state is
// in states — the resume scan. A nil/empty kinds list means both kinds; a
// nil/empty states list means all states. Records come back as copies, safe
// outside the transaction.
func (s *Store) ByStates(kinds []Kind, states []State) ([]Record, error) {
	kindList, err := resolveKinds(kinds)
	if err != nil {
		return nil, err
	}

	stateFilter := make(map[State]bool, len(states))
	for _, st := range states {
		if !allStates[st] {
			return nil, fmt.Errorf("неизвестное состояние %q", string(st))
		}

		stateFilter[st] = true
	}

	matchState := func(st State) bool {
		if len(stateFilter) == 0 {
			return true
		}

		return stateFilter[st]
	}

	var out []Record

	err = s.db.View(func(tx *bolt.Tx) error {
		for _, kind := range kindList {
			b := tx.Bucket(bucketNames[kind])
			if b == nil {
				continue
			}

			err := b.ForEach(func(_, val []byte) error {
				if val == nil {
					return nil
				}

				rec, err := decodeRecord(val)
				if err != nil {
					return err
				}

				if matchState(rec.State) {
					out = append(out, *rec)
				}

				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// Counts returns per-kind, per-state record counts for the console summary.
func (s *Store) Counts() (map[Kind]map[State]int, error) {
	counts := map[Kind]map[State]int{
		KindSave:   {},
		KindUpload: {},
	}

	err := s.db.View(func(tx *bolt.Tx) error {
		for _, name := range bucketNames {
			b := tx.Bucket(name)
			if b == nil {
				continue
			}

			err := b.ForEach(func(_, val []byte) error {
				if val == nil {
					return nil
				}

				rec, err := decodeRecord(val)
				if err != nil {
					return err
				}

				if counts[rec.Kind] == nil {
					counts[rec.Kind] = map[State]int{}
				}

				counts[rec.Kind][rec.State]++

				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return counts, nil
}

// validateKind fails fast on a kind outside the supported set.
func validateKind(kind Kind) error {
	if _, ok := bucketNames[kind]; !ok {
		return fmt.Errorf("неизвестный тип записи %q (допустимо: save, upload)", string(kind))
	}

	return nil
}

// validateState fails fast on a state outside the supported set.
func validateState(st State) error {
	if !allStates[st] {
		return fmt.Errorf("неизвестное состояние %q", string(st))
	}

	return nil
}

// resolveKinds validates and expands the kind list for scans.
func resolveKinds(kinds []Kind) ([]Kind, error) {
	if len(kinds) == 0 {
		return []Kind{KindSave, KindUpload}, nil
	}

	out := make([]Kind, 0, len(kinds))
	for _, kind := range kinds {
		if err := validateKind(kind); err != nil {
			return nil, err
		}

		out = append(out, kind)
	}

	return out, nil
}
