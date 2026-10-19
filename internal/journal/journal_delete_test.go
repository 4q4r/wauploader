package journal

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeleteRemovesRecord covers the `jobs --clear` surface (Task 12):
// Delete removes exactly the targeted record, reports whether a record
// existed, and a second delete of the same key reports false.
func TestDeleteRemovesRecord(t *testing.T) {
	st := openTestStore(t)
	rec := &Record{Kind: KindSave, Target: "https://example.com/clear-me", State: StateFailed}
	require.NoError(t, st.Put(rec))

	existed, err := st.Delete(KindSave, "https://example.com/clear-me")
	require.NoError(t, err)
	require.True(t, existed, "Delete must report an existing record")

	_, ok, err := st.Get(KindSave, "https://example.com/clear-me")
	require.NoError(t, err)
	require.False(t, ok, "record must be gone after Delete")

	existed, err = st.Delete(KindSave, "https://example.com/clear-me")
	require.NoError(t, err)
	require.False(t, existed, "second Delete must report no record")
}

// TestDeleteUnknownKindRejects pins the kind validation: an unknown kind
// fails loudly instead of touching a nonexistent bucket.
func TestDeleteUnknownKindRejects(t *testing.T) {
	st := openTestStore(t)
	existed, err := st.Delete(Kind("nope"), "x")
	require.Error(t, err)
	require.False(t, existed)
}
