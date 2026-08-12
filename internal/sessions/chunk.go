package sessions

import "strings"

const (
	// ChunkTargetChars is roughly 500 tokens. Small enough that several chunks fit
	// an agent's context budget, large enough that a chunk still reads as a
	// coherent passage rather than a fragment.
	ChunkTargetChars = 2000

	// SegmentMaxChars bounds one segment before it enters a chunk. A single tool
	// result can be a whole file — hundreds of kilobytes — and letting one dominate
	// the index would both bloat it and swamp bm25 scoring for every other chunk in
	// the session.
	SegmentMaxChars = 4000
)

// ChunkKind mirrors the two kinds of evidence a packet distinguishes. Errors are
// pulled out separately because "what went wrong last time" is the single most
// useful thing to hand an agent starting similar work.
type ChunkKind string

const (
	ChunkTranscript ChunkKind = "transcript"
	ChunkError      ChunkKind = "error"
)

// Chunk is a searchable window over a transcript.
//
// LineStart and LineEnd are 1-based and inclusive, and refer to the SCRUBBED
// transcript in the blob — so a search hit points at exact bytes on disk and a
// citation is checkable rather than a claim.
type Chunk struct {
	Index     int
	LineStart int
	LineEnd   int
	Kind      ChunkKind
	Text      string
}

// Chunks groups segments into indexable windows.
//
// Chunks are built by accumulating whole segments up to a target size rather than
// by cutting at a fixed character count. Splitting mid-sentence produces chunks
// that retrieve badly — half a thought scores poorly and reads worse — so a
// segment that would overflow starts the next chunk instead.
func Chunks(segs []Segment) []Chunk {
	var out []Chunk
	var cur []Segment
	var size int

	flush := func() {
		if len(cur) == 0 {
			return
		}
		var b strings.Builder
		kind := ChunkTranscript
		for i, s := range cur {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(string(s.Kind))
			b.WriteString(": ")
			b.WriteString(s.Text)
			if s.Kind == SegToolError {
				// One error anywhere in the window marks the whole chunk. Erring toward
				// marking is right: a missed error chunk never reaches the corrections
				// section, while an over-marked one is merely ranked alongside them.
				kind = ChunkError
			}
		}
		out = append(out, Chunk{
			Index:     len(out),
			LineStart: cur[0].LineNo,
			LineEnd:   cur[len(cur)-1].LineNo,
			Kind:      kind,
			Text:      b.String(),
		})
		cur, size = nil, 0
	}

	for _, s := range segs {
		s.Text = clipSegment(s.Text)
		if s.Text == "" {
			continue
		}
		if size > 0 && size+len(s.Text) > ChunkTargetChars {
			flush()
		}
		cur = append(cur, s)
		size += len(s.Text)
	}
	flush()
	return out
}

func clipSegment(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= SegmentMaxChars {
		return s
	}
	// Cut on a line boundary when one is near the limit, so a truncated segment
	// still ends at something readable.
	cut := s[:SegmentMaxChars]
	if nl := strings.LastIndexByte(cut, '\n'); nl > SegmentMaxChars/2 {
		cut = cut[:nl]
	}
	return cut + "\n…[truncated]"
}
