package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ChunkID is the stable identity of a chunk within a corpus.
//
// Keyed on (docID, ordinal). Ordinal is a monotonic counter within the
// document, so two chunks of one document cannot collide — the property is
// structural rather than something the chunker has to remember to preserve.
//
// # Why not the byte offset
//
// The docs ingester this replaces keyed on (path, byte offset), which reads
// as reasonable and is not. Its oversize-section branch assigned the *same*
// end-of-section offset to every sub-chunk after the first, so any section
// that split into three or more produced identical ids. Qdrant upserts by id,
// so all but the last were silently overwritten: sections vanished from the
// index, no error was raised anywhere, and the only symptom was the assistant
// being unable to answer about the middle of long pages.
//
// A byte offset also makes the id depend on the bytes *before* the chunk, so
// inserting a paragraph at the top of a document renumbers every chunk below
// it and re-embeds the whole file. An ordinal only changes when the chunk
// structure genuinely changes.
//
// The corpus name is not in the hash. Ids are scoped by the index they live
// in, and mixing the name in would mean a corpus rename re-embeds everything
// for no semantic change.
func ChunkID(docID string, ordinal int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s#%d", docID, ordinal)
	return hex.EncodeToString(h.Sum(nil))[:32]
}
