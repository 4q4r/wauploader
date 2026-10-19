// jobs-command tests: listing with state filter, the quota_parked
// visibility rule (ledger carry: parked jobs are shown, never auto-resumed)
// and --clear removal counts.
package commands

import (
	"path/filepath"
	"testing"

	"github.com/4q4r/wauploader/internal/journal"
	"github.com/stretchr/testify/require"
)

// seedJobs opens a real journal at jp and writes the fixture records.
func seedJobs(t *testing.T, jp string, recs ...*journal.Record) {
	t.Helper()
	st, err := journal.Open(jp)
	require.NoError(t, err)
	for _, rec := range recs {
		require.NoError(t, st.Put(rec))
	}
	require.NoError(t, st.Close())
}

func TestJobsListFilterClear(t *testing.T) {
	jp := filepath.Join(t.TempDir(), "journal.bbolt")
	seedJobs(t, jp,
		&journal.Record{Kind: journal.KindSave, Target: "https://example.com/ok", State: journal.StateDone},
		&journal.Record{Kind: journal.KindSave, Target: "https://example.com/bad", State: journal.StateFailed},
		&journal.Record{Kind: journal.KindSave, Target: "https://example.com/parked", State: journal.StateQuotaParked},
		&journal.Record{Kind: journal.KindUpload, Target: "/tmp/x\x00item-1/a.txt", State: journal.StateDone},
	)

	// Unfiltered listing shows everything, including quota_parked (ledger
	// carry: parked jobs are visible; requeue happens by re-listing).
	r := runRoot(t, "--journal", jp, "jobs")
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.Contains(t, r.out, "https://example.com/ok")
	require.Contains(t, r.out, "https://example.com/bad")
	require.Contains(t, r.out, "https://example.com/parked", "quota_parked must be visible")
	require.Contains(t, r.out, "item-1/a.txt", "upload composites use the display form")

	// State filter: only failed records.
	r = runRoot(t, "--journal", jp, "jobs", "--state", "failed")
	require.Equal(t, ExitOK, r.code)
	require.Contains(t, r.out, "https://example.com/bad")
	require.NotContains(t, r.out, "https://example.com/ok")
	require.NotContains(t, r.out, "https://example.com/parked")

	// Unknown filter value is a usage error (exit 2).
	r = runRoot(t, "--journal", jp, "jobs", "--state", "bogus")
	require.Equal(t, ExitUsage, r.code)
	require.Contains(t, r.errOut, "failed")

	// Clear removes every record and reports the count.
	r = runRoot(t, "--journal", jp, "jobs", "--clear")
	require.Equal(t, ExitOK, r.code, "errOut=%s", r.errOut)
	require.Contains(t, r.out, "Удалено записей: 4")

	st, err := journal.Open(jp)
	require.NoError(t, err)
	recs, err := st.ByStates(nil, nil)
	require.NoError(t, err)
	require.Empty(t, recs, "the journal must be empty after --clear")
	require.NoError(t, st.Close())

	// Clearing an empty journal is a clean no-op.
	r = runRoot(t, "--journal", jp, "jobs", "--clear")
	require.Equal(t, ExitOK, r.code)
	require.Contains(t, r.out, "Удалено записей: 0")
}
