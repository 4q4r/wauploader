package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bolt "go.etcd.io/bbolt"
)

func TestKeyForStability(t *testing.T) {
	// Known-vector: independently recompute the expected digest.
	want := sha256.Sum256([]byte(string(KindSave) + "\x00" + "https://example.com/"))
	assert.Equal(t, hex.EncodeToString(want[:]), KeyFor(KindSave, "https://example.com/"))

	// Determinism across calls and kind sensitivity.
	assert.Equal(t, KeyFor(KindSave, "https://example.com/"), KeyFor(KindSave, "https://example.com/"))
	assert.NotEqual(t, KeyFor(KindSave, "https://example.com/"), KeyFor(KindUpload, "https://example.com/"))
	assert.NotEqual(t, KeyFor(KindSave, "https://example.com/"), KeyFor(KindSave, "https://example.org/"))
}

func testRecord(kind Kind, target string) *Record {
	return &Record{
		Kind:     kind,
		Target:   target,
		State:    StateQueued,
		Attempts: 0,
		Size:     1234,
		MD5:      "aa",
		SHA256:   "bb",
	}
}

// TestPutGetRoundTripTargetMeta: the per-target item metadata (--auto-meta,
// fix round 2) survives the journal round trip — add-fields-only gob
// evolution per the T9 policy, SchemaVersion stays 1.
func TestPutGetRoundTripTargetMeta(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	r := testRecord(KindUpload, "/docs/01_a_topic.pdf\x00testid-meta01/01_a_topic.pdf")
	r.Title = "Derived Title"
	r.Description = "Derived description"
	r.Extra = map[string][]string{"subject": {"athens", "aristophanes frogs"}, "language": {"rus"}}

	require.NoError(t, store.Put(r))

	got, found, err := store.Get(KindUpload, r.Target)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "Derived Title", got.Title)
	assert.Equal(t, "Derived description", got.Description)
	assert.Equal(t, map[string][]string{"subject": {"athens", "aristophanes frogs"}, "language": {"rus"}}, got.Extra)
	assert.Equal(t, uint8(1), got.SchemaVersion, "add-fields evolution must not bump the schema")
}

func TestPutGetRoundTrip(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	r := testRecord(KindSave, "https://example.com/page")
	r.State = StatePending
	r.Attempts = 2
	r.SPN2JobID = "spn2-job-1"
	r.SnapshotURL = "https://web.archive.org/web/2026/https://example.com/page"
	r.SnapshotTimestamp = "20261019120000"
	r.ErrKind = ""
	r.ErrMsg = ""
	now := time.Now().Truncate(time.Second)
	r.UpdatedAt = now

	require.NoError(t, store.Put(r))

	got, found, err := store.Get(KindSave, "https://example.com/page")
	require.NoError(t, err)
	require.True(t, found, "record must be found after Put")

	assert.Equal(t, KeyFor(KindSave, "https://example.com/page"), got.Key)
	assert.Equal(t, KindSave, got.Kind)
	assert.Equal(t, "https://example.com/page", got.Target)
	assert.Equal(t, StatePending, got.State)
	assert.Equal(t, 2, got.Attempts)
	assert.Equal(t, "spn2-job-1", got.SPN2JobID)
	assert.Equal(t, "https://web.archive.org/web/2026/https://example.com/page", got.SnapshotURL)
	assert.Equal(t, "20261019120000", got.SnapshotTimestamp)
	assert.Equal(t, "aa", got.MD5)
	assert.Equal(t, "bb", got.SHA256)
	assert.Equal(t, int64(1234), got.Size)
	assert.Equal(t, uint8(1), got.SchemaVersion)
	assert.True(t, got.UpdatedAt.Equal(now), "UpdatedAt must round-trip: want %v got %v", now, got.UpdatedAt)

	// Miss on an unknown target is not an error.
	_, found, err = store.Get(KindSave, "https://example.com/absent")
	require.NoError(t, err)
	assert.False(t, found)

	// Kinds are isolated: same target under a different kind is a miss.
	_, found, err = store.Get(KindUpload, "https://example.com/page")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestKeyStableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.bbolt")

	store, err := Open(path)
	require.NoError(t, err)
	r := testRecord(KindUpload, "/tmp/data/item.bin\x00my-identifier/my-key")
	r.State = StateDone
	r.Size = 4096
	require.NoError(t, store.Put(r))
	key := KeyFor(KindUpload, "/tmp/data/item.bin\x00my-identifier/my-key")
	require.NoError(t, store.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	defer reopened.Close()

	got, found, err := reopened.Get(KindUpload, "/tmp/data/item.bin\x00my-identifier/my-key")
	require.NoError(t, err)
	require.True(t, found, "record must survive close+reopen")
	assert.Equal(t, key, got.Key)
	assert.Equal(t, StateDone, got.State)
	assert.Equal(t, int64(4096), got.Size)
}

func TestSecondOpenLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.bbolt")

	first, err := Open(path)
	require.NoError(t, err)
	defer first.Close()

	_, err = Open(path)
	require.Error(t, err, "second concurrent open must fail")
	assert.Contains(t, err.Error(), "журнал занят другим процессом wauploader",
		"lock error must carry the exact Russian contract message, got: %v", err)

	// After the holder closes, the file can be opened again.
	require.NoError(t, first.Close())
	third, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, third.Close())
}

func TestByStatesResumeScan(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	seed := []struct {
		kind  Kind
		state State
	}{
		{KindSave, StateQueued},
		{KindSave, StatePending},
		{KindSave, StateDone},
		{KindSave, StateFailed},
		{KindUpload, StateQueued},
		{KindUpload, StateQuotaParked},
		{KindUpload, StateSkipped},
		{KindUpload, StateDone},
	}
	for i, s := range seed {
		r := testRecord(s.kind, fmt.Sprintf("https://example.com/%d", i))
		r.State = s.state
		require.NoError(t, store.Put(r))
	}

	// Resume scan: both kinds, queued+pending+quota_parked.
	got, err := store.ByStates([]Kind{KindSave, KindUpload}, []State{StateQueued, StatePending, StateQuotaParked})
	require.NoError(t, err)
	assert.Len(t, got, 4)
	gotStates := map[State]int{}
	gotKinds := map[Kind]int{}
	for _, r := range got {
		gotStates[r.State]++
		gotKinds[r.Kind]++
	}
	assert.Equal(t, map[State]int{StateQueued: 2, StatePending: 1, StateQuotaParked: 1}, gotStates)
	assert.Equal(t, map[Kind]int{KindSave: 2, KindUpload: 2}, gotKinds)

	// Single kind, single state.
	got, err = store.ByStates([]Kind{KindUpload}, []State{StateSkipped})
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, StateSkipped, got[0].State)
	assert.Equal(t, KindUpload, got[0].Kind)

	// Empty result set is valid.
	got, err = store.ByStates([]Kind{KindSave}, []State{StateSubmitted})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCounts(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	seed := map[Kind]map[State]int{
		KindSave:   {StateQueued: 2, StateDone: 1, StateFailed: 1},
		KindUpload: {StateDone: 3, StateQuotaParked: 1},
	}
	for kind, states := range seed {
		for state, count := range states {
			for j := 0; j < count; j++ {
				r := testRecord(kind, fmt.Sprintf("https://example.com/%s-%s-%d", kind, state, j))
				r.State = state
				require.NoError(t, store.Put(r))
			}
		}
	}

	counts, err := store.Counts()
	require.NoError(t, err)
	assert.Equal(t, seed, counts)

	// Update one record and re-count.
	require.NoError(t, store.UpdateState(KindSave, "https://example.com/"+string(KindSave)+"-"+string(StateQueued)+"-0", StateDone, nil))
	counts, err = store.Counts()
	require.NoError(t, err)
	assert.Equal(t, 1, counts[KindSave][StateQueued])
	assert.Equal(t, 2, counts[KindSave][StateDone])
}

func TestUnknownSchemaVersionError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.bbolt")

	store, err := Open(path)
	require.NoError(t, err)
	r := testRecord(KindSave, "https://example.com/legacy")
	require.NoError(t, store.Put(r))
	require.NoError(t, store.Close())

	// Corrupt the schema-version byte of that record to an unknown future
	// version, simulating a journal written by a newer wauploader.
	err = boltUpdateVersionByte(path, KeyFor(KindSave, "https://example.com/legacy"), 9)
	require.NoError(t, err)

	reopened, err := Open(path)
	require.NoError(t, err)
	defer reopened.Close()

	_, _, err = reopened.Get(KindSave, "https://example.com/legacy")
	require.Error(t, err, "unknown schema version must never be read silently")
	assert.Contains(t, err.Error(), "версией схемы",
		"migration error must be an explicit Russian message, got: %v", err)

	// The scan path must surface the same error, never skip silently.
	_, err = reopened.ByStates([]Kind{KindSave}, []State{StateQueued})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "версией схемы")
}

func TestConcurrentSingleWriter(t *testing.T) {
	t.Run("serialized_writer_ok", func(t *testing.T) {
		store := openTestStore(t)
		defer store.Close()

		const workers = 8
		const perWorker = 25
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					target := fmt.Sprintf("https://example.com/w%d-%d", w, i)
					r := testRecord(KindSave, target)
					if err := store.Put(r); err != nil {
						errs <- fmt.Errorf("put %s: %w", target, err)
						return
					}
					st := StatePending
					if i%2 == 0 {
						st = StateSubmitted
					}
					if err := store.UpdateState(KindSave, target, st, func(rec *Record) {
						rec.Attempts++
						rec.SPN2JobID = fmt.Sprintf("job-%d-%d", w, i)
					}); err != nil {
						errs <- fmt.Errorf("update %s: %w", target, err)
						return
					}
					rec, found, err := store.Get(KindSave, target)
					if err != nil || !found {
						errs <- fmt.Errorf("get %s: found=%v err=%v", target, found, err)
						return
					}
					if rec.Attempts != 1 || rec.SPN2JobID == "" {
						errs <- fmt.Errorf("mutate lost for %s: %+v", target, rec)
						return
					}
				}
			}(w)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		counts, err := store.Counts()
		require.NoError(t, err)
		assert.Equal(t, workers*perWorker, counts[KindSave][StateSubmitted]+counts[KindSave][StatePending])
	})

	t.Run("second_open_locked", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal.bbolt")
		first, err := Open(path)
		require.NoError(t, err)
		defer first.Close()

		done := make(chan error, 1)
		go func() {
			_, err := Open(path)
			done <- err
		}()
		select {
		case err := <-done:
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), "журнал занят другим процессом wauploader"))
		case <-time.After(10 * time.Second):
			t.Fatal("second Open must fail fast, not hang")
		}
	})
}

// TestUpdateStateMissingRecord pins the loud-failure contract: updating the
// state of an unknown record is a bug and must return an error, not create it.
func TestUpdateStateMissingRecord(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	err := store.UpdateState(KindSave, "https://example.com/absent", StateDone, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "не найдена")
}

// TestPutValidatesKindAndState pins fail-fast validation of the enum fields.
func TestPutValidatesKindAndState(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	r := testRecord(Kind("wiki"), "https://example.com/")
	err := store.Put(r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "неизвестный тип записи")

	r = testRecord(KindSave, "https://example.com/")
	r.State = State("finished")
	err = store.Put(r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "неизвестное состояние")
}

// TestUpdateStateMutateRevalidation pins the re-validation contract: a mutate
// hook may not persist an invalid state, flip the record's kind, or change the
// derived key — the record must stay unchanged on re-read.
func TestUpdateStateMutateRevalidation(t *testing.T) {
	cases := []struct {
		name     string
		wantErr  string
		mutate   func(*Record)
		wantKeep State // state expected on re-read after the rejected mutate
	}{
		{
			name:     "invalid_state_rejected",
			wantErr:  "неизвестное состояние",
			mutate:   func(r *Record) { r.State = "broken" },
			wantKeep: StateQueued,
		},
		{
			name:     "kind_flip_rejected",
			wantErr:  "изменение типа записи запрещено",
			mutate:   func(r *Record) { r.Kind = KindUpload },
			wantKeep: StateQueued,
		},
		{
			name:     "key_change_rejected",
			wantErr:  "изменение ключа записи запрещено",
			mutate:   func(r *Record) { r.Key = "deadbeef" },
			wantKeep: StateQueued,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t)
			defer store.Close()

			target := "https://example.com/revalidate-" + tc.name
			require.NoError(t, store.Put(testRecord(KindSave, target)))

			err := store.UpdateState(KindSave, target, StateDone, tc.mutate)
			require.Error(t, err, "mutate must be re-validated before persist")
			assert.Contains(t, err.Error(), tc.wantErr)

			got, found, err := store.Get(KindSave, target)
			require.NoError(t, err)
			require.True(t, found, "record must still exist")
			assert.Equal(t, tc.wantKeep, got.State, "record must be unchanged after rejected mutate")
			assert.Equal(t, KindSave, got.Kind)
			assert.Equal(t, KeyFor(KindSave, target), got.Key)
		})
	}
}

// TestPutDoesNotMutateCaller pins Put purity: the caller's record is never
// written to; derived fields stay zero in the caller's copy.
func TestPutDoesNotMutateCaller(t *testing.T) {
	store := openTestStore(t)
	defer store.Close()

	r := testRecord(KindSave, "https://example.com/pure-put")
	require.NoError(t, store.Put(r))

	assert.Empty(t, r.Key, "Put must not write Key back into the caller's record")
	assert.True(t, r.UpdatedAt.IsZero(), "Put must not stamp UpdatedAt in the caller's record")
	assert.Zero(t, r.SchemaVersion, "Put must not set SchemaVersion in the caller's record")

	// The stored copy still carries the derived values.
	got, found, err := store.Get(KindSave, "https://example.com/pure-put")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, KeyFor(KindSave, "https://example.com/pure-put"), got.Key)
	assert.Equal(t, uint8(1), got.SchemaVersion)
	assert.False(t, got.UpdatedAt.IsZero())
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "journal.bbolt"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// boltUpdateVersionByte rewrites the first byte (schema version) of the record
// stored under key, using a raw bbolt handle opened outside the journal API.
func boltUpdateVersionByte(path, key string, version uint8) error {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("save"))
		if b == nil {
			return fmt.Errorf("bucket save missing")
		}
		val := b.Get([]byte(key))
		if val == nil {
			return fmt.Errorf("key %s missing", key)
		}
		out := make([]byte, len(val))
		copy(out, val)
		out[0] = version
		return b.Put([]byte(key), out)
	})
}
