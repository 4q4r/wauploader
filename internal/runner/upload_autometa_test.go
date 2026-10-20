// Per-target item metadata (--auto-meta, Task 14): Title/Description
// override the batch HeaderOpts on that file's PUT; Extra values append
// after the batch values for the same names.
package runner

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/4q4r/wauploader/internal/ias3"
	"github.com/4q4r/wauploader/internal/journal"
)

func TestPerTargetMetaOverridesBatchHeaders(t *testing.T) {
	paths := writeUploadFiles(t, "one")
	f := newFakeIAS3(t)
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	tgt := UploadTarget{
		Path:        paths[0],
		Identifier:  "testid-autometa1",
		Key:         filepath.Base(paths[0]),
		Title:       "Derived Title",
		Description: "Derived description",
		Extra: ias3.Meta{
			"subject":  {"athens", "aristophanes frogs"},
			"language": {"rus"},
			"year":     {"2021"},
		},
	}

	o := uploadOpts()
	o.Title = "Batch Title"
	o.Description = "Batch description"
	o.Extra = ias3.Meta{"subject": {"batch-subj"}}

	_, sum, err := runUploadAll(t, context.Background(), r, []UploadTarget{tgt}, o, false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum.Done)

	hdr := f.headers(tgt.Key)
	assert.Equal(t, "Derived Title", hdr.Get("x-archive-meta-title"), "target title overrides the batch one")
	assert.Equal(t, "Derived description", hdr.Get("x-archive-meta-description"))
	assert.Equal(t, "batch-subj", hdr.Get("x-archive-meta00-subject"), "batch extra values ride first")
	assert.Equal(t, "athens", hdr.Get("x-archive-meta01-subject"), "target extra values append")
	assert.Equal(t, "aristophanes frogs", hdr.Get("x-archive-meta02-subject"))
	assert.Equal(t, "rus", hdr.Get("x-archive-meta-language"))
	assert.Equal(t, "2021", hdr.Get("x-archive-meta-year"))
	assert.Equal(t, journal.StateDone, mustUploadRecord(t, st, tgt).State)
}

// Without per-target metadata nothing changes (batch headers only).
func TestNoTargetMetaKeepsBatchHeaders(t *testing.T) {
	paths := writeUploadFiles(t, "one")
	f := newFakeIAS3(t)
	r, _ := newTestUploadRunner(t, f, uploadTestLimits())

	tgt := uploadTargets(paths, "testid-autometa2")[0]
	o := uploadOpts()
	o.Title = "Batch Title"

	_, sum, err := runUploadAll(t, context.Background(), r, []UploadTarget{tgt}, o, false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum.Done)

	hdr := f.headers(tgt.Key)
	assert.Equal(t, "Batch Title", hdr.Get("x-archive-meta-title"))
	assert.Empty(t, hdr.Get("x-archive-meta-language"))
}

// autoMetaResumeFixture: a 2-file batch whose SECOND file's PUT hangs until
// the context is cancelled — the interrupt leaves it queued (resumable),
// while the first file completes. Returns the store for the resumed run.
func autoMetaResumeFixture(t *testing.T, title string, extra ias3.Meta) (*fakeIAS3, *journal.Store, []UploadTarget) {
	t.Helper()
	paths := writeUploadFiles(t, "one", "two")
	f := newFakeIAS3(t)
	f.blockPutUntilCancel = true
	f.blockPutKey = "file2.bin"

	tgts := []UploadTarget{
		{Path: paths[0], Identifier: "testid-autometa3", Key: "file1.bin", Title: title, Description: "Derived description", Extra: extra},
		{Path: paths[1], Identifier: "testid-autometa3", Key: "file2.bin", Title: title, Description: "Derived description", Extra: extra},
	}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())

	ctx, cancel := context.WithCancel(context.Background())
	canceler := make(chan struct{})
	go func() {
		defer close(canceler)
		assert.Eventually(t, func() bool { return f.puts("file2.bin") == 1 },
			2*time.Second, 2*time.Millisecond, "the second PUT must start before the cancel")
		cancel()
	}()
	_, _, err := runUploadAll(t, ctx, r, tgts, uploadOpts(), false, false)
	<-canceler
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, journal.StateQueued, mustUploadRecord(t, st, tgts[1]).State)

	return f, st, []UploadTarget{tgts[1]}
}

// TestInterruptedAutoMetaResumeCarriesJournalMeta (CodeRabbit PR#16
// Major): the resumed run reconstructs the queued file's target from the
// journal — the persisted per-target metadata must ride the PUT.
func TestInterruptedAutoMetaResumeCarriesJournalMeta(t *testing.T) {
	f, st, queued := autoMetaResumeFixture(t, "Derived Title",
		ias3.Meta{"subject": {"athens", "aristophanes frogs"}})

	// Resume WITHOUT in-memory metadata: the journal must supply it all.
	f.setBlockPutUntilCancel(false)
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	resumed := []UploadTarget{{Path: queued[0].Path, Identifier: queued[0].Identifier, Key: queued[0].Key}}

	_, sum2, err := runUploadAll(t, context.Background(), r2, resumed, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum2.Done)

	hdr := f.headers("file2.bin")
	assert.Equal(t, "Derived Title", hdr.Get("x-archive-meta-title"))
	assert.Equal(t, "Derived description", hdr.Get("x-archive-meta-description"))
	assert.Equal(t, "athens", hdr.Get("x-archive-meta00-subject"))
	assert.Equal(t, "aristophanes frogs", hdr.Get("x-archive-meta01-subject"))
}

// TestResumeDedupesIdenticalDerivation: resuming with the SAME derivation
// the journal already holds must not duplicate subject values on the wire
// (CodeRabbit PR#16 round-2 🟡).
func TestResumeDedupesIdenticalDerivation(t *testing.T) {
	f, st, queued := autoMetaResumeFixture(t, "Derived Title",
		ias3.Meta{"subject": {"athens", "aristophanes frogs"}})

	f.setBlockPutUntilCancel(false)
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	resumed := []UploadTarget{{
		Path:       queued[0].Path,
		Identifier: queued[0].Identifier,
		Key:        queued[0].Key,
		Title:      "Derived Title",
		Extra:      ias3.Meta{"subject": {"athens", "aristophanes frogs"}},
	}}

	_, sum2, err := runUploadAll(t, context.Background(), r2, resumed, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum2.Done)

	hdr := f.headers("file2.bin")
	assert.Equal(t, "athens", hdr.Get("x-archive-meta00-subject"))
	assert.Equal(t, "aristophanes frogs", hdr.Get("x-archive-meta01-subject"))
	assert.Empty(t, hdr.Values("x-archive-meta02-subject"), "identical journal values must not duplicate")
	assert.Empty(t, hdr.Values("x-archive-meta03-subject"))
}

// TestResumeUnionsMixedDerivation: fresh values ride first as-is, journal
// values append only when new for the same key.
func TestResumeUnionsMixedDerivation(t *testing.T) {
	f, st, queued := autoMetaResumeFixture(t, "Derived Title",
		ias3.Meta{"subject": {"athens", "aristophanes frogs"}})

	f.setBlockPutUntilCancel(false)
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	resumed := []UploadTarget{{
		Path:       queued[0].Path,
		Identifier: queued[0].Identifier,
		Key:        queued[0].Key,
		Extra:      ias3.Meta{"subject": {"athens", "sparta"}},
	}}

	_, sum2, err := runUploadAll(t, context.Background(), r2, resumed, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum2.Done)

	hdr := f.headers("file2.bin")
	assert.Equal(t, []string{"athens", "sparta"}, []string{
		hdr.Get("x-archive-meta00-subject"),
		hdr.Get("x-archive-meta01-subject"),
	}, "fresh values replace the held name entirely")
	assert.Empty(t, hdr.Values("x-archive-meta02-subject"), "held values do not append under a fresh name")
}

// TestRelistedFailedKeepsJournalTitle: a failed file re-listed WITHOUT a
// fresh derivation must not wipe the persisted metadata — the re-upload
// still carries the journal-held Title and subjects (round-3 nit).
func TestRelistedFailedKeepsJournalTitle(t *testing.T) {
	paths := writeUploadFiles(t, "one", "two")
	f := newFakeIAS3(t)
	f.putFn = func(identifier, key string, nth int) int {
		if key == "file1.bin" && nth == 1 {
			return http.StatusBadRequest // terminal, no ias3 retry loop
		}

		return http.StatusOK
	}

	meta := ias3.Meta{"subject": {"athens", "aristophanes frogs"}}
	tgts := []UploadTarget{
		{Path: paths[0], Identifier: "testid-autometa4", Key: "file1.bin", Title: "Derived Title", Description: "Derived description", Extra: meta},
		{Path: paths[1], Identifier: "testid-autometa4", Key: "file2.bin", Title: "Derived Title", Description: "Derived description", Extra: meta},
	}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())
	_, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum.Done)
	require.Equal(t, journal.StateFailed, mustUploadRecord(t, st, tgts[0]).State)

	// Re-list the failed file with NO fresh derivation.
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	relisted := []UploadTarget{{Path: paths[0], Identifier: "testid-autometa4", Key: "file1.bin"}}
	_, sum2, err := runUploadAll(t, context.Background(), r2, relisted, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum2.Done)

	hdr := f.headers("file1.bin")
	assert.Equal(t, "Derived Title", hdr.Get("x-archive-meta-title"), "empty-fresh re-list must keep the journal Title")
	assert.Equal(t, "athens", hdr.Get("x-archive-meta00-subject"))
	assert.Equal(t, "aristophanes frogs", hdr.Get("x-archive-meta01-subject"))
}

// TestRelistResetKeepsHeldMetaInJournal (round-4 🟠 FIX 1): an empty-fresh
// re-list of a FAILED file persists the MERGED (held) metadata at reset
// time — if the run stops right after planRun, the journal still holds it.
func TestRelistResetKeepsHeldMetaInJournal(t *testing.T) {
	paths := writeUploadFiles(t, "one", "two")
	f := newFakeIAS3(t)
	f.putFn = func(identifier, key string, nth int) int {
		if key == "file1.bin" && nth == 1 {
			return http.StatusBadRequest // terminal, no ias3 retry loop
		}

		return http.StatusOK
	}

	meta := ias3.Meta{"subject": {"athens", "aristophanes frogs"}}
	tgts := []UploadTarget{
		{Path: paths[0], Identifier: "testid-autometa5", Key: "file1.bin", Title: "Derived Title", Description: "Derived description", Extra: meta},
		{Path: paths[1], Identifier: "testid-autometa5", Key: "file2.bin", Title: "Derived Title", Description: "Derived description", Extra: meta},
	}
	r, st := newTestUploadRunner(t, f, uploadTestLimits())
	_, sum, err := runUploadAll(t, context.Background(), r, tgts, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum.Done)

	// Re-list the failed file with NO fresh derivation; the PUT hangs and
	// the run is cancelled right after it starts — the only journal write
	// after the reset is the interrupted queued state.
	f.setBlockPutUntilCancel(true)
	f.blockPutKey = "file1.bin"
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	relisted := []UploadTarget{{Path: paths[0], Identifier: "testid-autometa5", Key: "file1.bin"}}

	ctx, cancel := context.WithCancel(context.Background())
	canceler := make(chan struct{})
	go func() {
		defer close(canceler)
		assert.Eventually(t, func() bool { return f.puts("file1.bin") == 1 },
			2*time.Second, 2*time.Millisecond)
		cancel()
	}()
	_, _, _ = runUploadAll(t, ctx, r2, relisted, uploadOpts(), false, false)
	<-canceler

	rec, found, err := st.Get(journal.KindUpload, uploadRecordTarget(relisted[0]))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "Derived Title", rec.Title, "the reset-time persist must carry the MERGED values, not the raw empty fresh ones")
	require.Equal(t, "Derived description", rec.Description)
	require.Equal(t, map[string][]string{"subject": {"athens", "aristophanes frogs"}}, rec.Extra)
}

// TestRelistExplicitFreshReplacesDerivedNames (round-4 🟠 FIX 2): fresh
// values REPLACE held per NAME — a --meta subject=x retry after derived
// subjects must send [x] only, and a creator-source both→file retry must
// send the file author only.
func TestRelistExplicitFreshReplacesDerivedNames(t *testing.T) {
	f, st, queued := autoMetaResumeFixture(t, "Derived Title", ias3.Meta{
		"subject": {"athens", "aristophanes frogs"},
		"creator": {"test", "PDF Author"},
	})

	f.setBlockPutUntilCancel(false)
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	resumed := []UploadTarget{{
		Path:       queued[0].Path,
		Identifier: queued[0].Identifier,
		Key:        queued[0].Key,
		Extra: ias3.Meta{
			"subject": {"x"},
			"creator": {"test"},
		},
	}}

	_, sum2, err := runUploadAll(t, context.Background(), r2, resumed, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum2.Done)

	hdr := f.headers("file2.bin")
	assert.Equal(t, "x", hdr.Get("x-archive-meta-subject"), "fresh name replaces the held values entirely (single value = unnumbered header)")
	assert.Empty(t, hdr.Values("x-archive-meta00-subject"))
	assert.Equal(t, "test", hdr.Get("x-archive-meta-creator"), "creator retry replaces the both-sourced pair")
	assert.Empty(t, hdr.Values("x-archive-meta01-creator"))
}

// TestRelistedHeldNamesFillMissingFresh: held values fill only names the
// fresh derivation does not carry.
func TestRelistedHeldNamesFillMissingFresh(t *testing.T) {
	f, st, queued := autoMetaResumeFixture(t, "Derived Title", ias3.Meta{
		"subject": {"athens", "aristophanes frogs"},
		"creator": {"test"},
	})

	f.setBlockPutUntilCancel(false)
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	resumed := []UploadTarget{{
		Path:       queued[0].Path,
		Identifier: queued[0].Identifier,
		Key:        queued[0].Key,
		Extra:      ias3.Meta{"language": {"rus"}},
	}}

	_, sum2, err := runUploadAll(t, context.Background(), r2, resumed, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum2.Done)

	hdr := f.headers("file2.bin")
	assert.Equal(t, "rus", hdr.Get("x-archive-meta-language"), "fresh-only name lands")
	assert.Equal(t, "athens", hdr.Get("x-archive-meta00-subject"), "held names fill when fresh lacks them")
	assert.Equal(t, "aristophanes frogs", hdr.Get("x-archive-meta01-subject"))
	assert.Equal(t, "test", hdr.Get("x-archive-meta-creator"))
}

// TestRelistedTargetPrefersFreshDerivation: the merge rule — fresh
// derivation wins when present, the journal fills the rest (per field;
// Extra per NAME: fresh replaces, held fills missing names).
func TestRelistedTargetPrefersFreshDerivation(t *testing.T) {
	f, st, queued := autoMetaResumeFixture(t, "Derived Title",
		ias3.Meta{"subject": {"athens", "aristophanes frogs"}})

	f.setBlockPutUntilCancel(false)
	r2 := newTestUploadRunnerOnStore(t, f, st, uploadTestLimits())
	resumed := []UploadTarget{{
		Path:       queued[0].Path,
		Identifier: queued[0].Identifier,
		Key:        queued[0].Key,
		Title:      "Fresh Title",
		Extra:      ias3.Meta{"subject": {"sparta"}},
	}}

	_, sum2, err := runUploadAll(t, context.Background(), r2, resumed, uploadOpts(), false, false)
	require.NoError(t, err)
	require.Equal(t, 1, sum2.Done)

	hdr := f.headers("file2.bin")
	assert.Equal(t, "Fresh Title", hdr.Get("x-archive-meta-title"), "fresh derivation wins")
	assert.Equal(t, "Derived description", hdr.Get("x-archive-meta-description"), "journal fills the field fresh lacks")
	assert.Equal(t, "sparta", hdr.Get("x-archive-meta-subject"), "fresh name replaces the held values entirely (single value = unnumbered header)")
	assert.Empty(t, hdr.Values("x-archive-meta00-subject"), "held values do not survive a fresh name override")
}
