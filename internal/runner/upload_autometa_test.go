// Per-target item metadata (--auto-meta, Task 14): Title/Description
// override the batch HeaderOpts on that file's PUT; Extra values append
// after the batch values for the same names.
package runner

import (
	"context"
	"path/filepath"
	"testing"

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
