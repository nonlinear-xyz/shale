// Package watch sweeps transcripts into the local store.
//
// The sweep is deliberately dumb and re-runnable: discover files, skip the ones
// still being written, skip the ones already below the watermark, scrub, hash,
// store. Correctness comes from content-hash dedupe in the store, never from the
// cursor — the cursor is an optimization that avoids re-reading settled files.
package watch

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nonlinear-xyz/shale/internal/discover"
	"github.com/nonlinear-xyz/shale/internal/scrub"
	"github.com/nonlinear-xyz/shale/internal/sessions"
	"github.com/nonlinear-xyz/shale/internal/store"
)

// SettleWindow is how long a transcript must be untouched before it is swept.
//
// It is a quiet-file heuristic, not an end-of-session signal: a session left open
// but idle for 30+ minutes can be swept before it ends, and its later activity is
// re-offered as a grown session with a different content hash. A SessionEnd hook
// is the only true end signal.
const SettleWindow = 30 * time.Minute

// Options control one sweep.
type Options struct {
	Sources []sessions.Source
	DryRun  bool
	// Force ignores both the settle window and the cursor. Used by an explicit
	// single-file capture, where the caller already knows the session ended.
	Force bool
	// Rescan ignores the cursor but keeps the settle window. Needed whenever the
	// scrub rules change: every stored transcript was redacted under the OLD rules
	// and nothing else would ever re-offer it, so a corpus captured under a bug
	// stays wrong forever.
	Rescan  bool
	Machine string
	Now     time.Time
}

// Result reports what one sweep did. Every skip carries a reason: a silent skip
// is indistinguishable from a bug.
type Result struct {
	Scanned  int
	Captured int
	// Backfilled counts sessions already stored whose chunk index was repaired.
	Backfilled int
	Skipped    []Skip
	Errors     []error
}

// Skip is one file the sweep declined, with why.
type Skip struct {
	Path   string
	Reason string
}

// Sweep processes every source and returns what happened.
func Sweep(ctx context.Context, db *store.DB, sc *scrub.Scrubber, opts Options) (Result, error) {
	var res Result
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	srcs := opts.Sources
	if len(srcs) == 0 {
		srcs = sessions.AllSources
	}

	for _, src := range srcs {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		paths, err := sessions.Discover(src)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("%s: discover: %w", src, err))
			continue
		}
		if len(paths) == 0 {
			continue
		}
		// Sort so a run is deterministic and easy to reason about in --dry-run.
		// It does NOT make the watermark safe on its own — see the clamp below.
		sort.Strings(paths)

		cursor, err := db.Cursor(ctx, string(src))
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("%s: cursor: %w", src, err))
			continue
		}

		maxSeen := cursor
		minFailed := int64(-1)

		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			res.Scanned++

			info, err := os.Stat(path)
			if err != nil {
				res.Skipped = append(res.Skipped, Skip{path, "unreadable"})
				continue
			}
			mtimeMS := info.ModTime().UnixMilli()

			if !opts.Force {
				// The settle check comes BEFORE the cursor check, and a not-settled
				// file must never advance maxSeen — otherwise a session still being
				// written would be passed over permanently once it went quiet.
				if now.Sub(info.ModTime()) < SettleWindow {
					res.Skipped = append(res.Skipped, Skip{path, "not settled (written <30m ago)"})
					continue
				}
				if !opts.Rescan && mtimeMS <= cursor {
					continue // already offered on an earlier sweep
				}
			}

			outcome, err := captureOne(ctx, db, sc, path, src, opts)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("%s: %w", filepath.Base(path), err))
				if minFailed < 0 || mtimeMS < minFailed {
					minFailed = mtimeMS
				}
				continue
			}
			switch outcome {
			case outcomeCaptured:
				res.Captured++
			case outcomeBackfilled:
				res.Backfilled++
			default:
				res.Skipped = append(res.Skipped, Skip{path, outcome.reason()})
			}
			if mtimeMS > maxSeen {
				maxSeen = mtimeMS
			}
		}

		if opts.DryRun {
			continue // a preview must never move the watermark
		}
		if err := db.SetCursor(ctx, string(src), Clamp(cursor, maxSeen, minFailed)); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("%s: set cursor: %w", src, err))
		}
	}
	return res, nil
}

// Clamp computes the watermark to persist after a sweep.
//
// Skipping a failed file is not enough. Directory order is not mtime order, so a
// file that fails is routinely followed by a NEWER file that succeeds and drags
// the watermark past it. The failure would then sit below the cursor and be
// skipped on every future sweep — silently lost, with no error anywhere.
//
// So the watermark is clamped to just under the earliest failure. The outer max
// against the starting cursor prevents regressing below where this run began,
// which would re-offer everything and turn one bad file into an endless loop.
func Clamp(startCursor, maxSeen, minFailed int64) int64 {
	if minFailed >= 0 {
		if capped := minFailed - 1; capped < maxSeen {
			maxSeen = capped
		}
	}
	if maxSeen < startCursor {
		return startCursor
	}
	return maxSeen
}

// outcome distinguishes the ways a transcript can fail to produce a new event.
// Collapsing them into a bool made the sweep report "already captured" for
// sessions that had never been seen, which is the kind of wrong status message
// that costs an hour of debugging the wrong thing.
type outcome int

const (
	outcomeCaptured outcome = iota
	outcomeNoUserMessages
	outcomeDuplicate
	// outcomeBackfilled marks a session that was already stored but had no chunks —
	// the upgrade case, repaired in place.
	outcomeBackfilled
)

func (o outcome) reason() string {
	switch o {
	case outcomeNoUserMessages:
		return "nothing to index (no user messages)"
	case outcomeDuplicate:
		return "unchanged (already captured)"
	case outcomeBackfilled:
		return "backfilled chunk index"
	default:
		return "captured"
	}
}

// captureOne reads, digests, scrubs, hashes and stores a single transcript.
func captureOne(ctx context.Context, db *store.DB, sc *scrub.Scrubber, path string, src sessions.Source, opts Options) (outcome, error) {
	sess, lines, err := sessions.Read(path, src)
	if err != nil {
		return outcomeNoUserMessages, fmt.Errorf("read: %w", err)
	}

	digest, ok := sessions.BuildDigest(sess, lines)
	if !ok {
		// A session nobody spoke in has nothing worth indexing, and storing it
		// would dilute every search.
		return outcomeNoUserMessages, nil
	}
	sess.FirstUserMessage = digest.FirstUserMessage
	sess.Repo = repoFor(sess.CWD)

	// Scrub every line, THEN hash the scrubbed text. A hub recomputes the digest
	// over what it receives, so scrubbing precedes hashing by construction.
	scrubbed := make([]string, len(lines))
	for i, l := range lines {
		scrubbed[i] = sc.Line(l)
	}
	joined := strings.Join(scrubbed, "\n")
	sum := sha256.Sum256([]byte(joined))
	contentHash := hex.EncodeToString(sum[:])

	// The digest is derived from the raw transcript, so scrub it too — a secret in
	// a user message would otherwise reach the search index unredacted.
	digestText := sc.String(digest.Text)

	rec := store.SessionRecord{
		Source:     string(src),
		SourceKey:  sess.SourceKey,
		Title:      sc.String(sess.Title()),
		Digest:     digestText,
		Repo:       sess.Repo,
		Branch:     sess.Branch,
		Project:    sess.Project,
		CWD:        sess.CWD,
		Machine:    opts.Machine,
		Turns:      sess.Turns,
		Usage:      usageMap(sess.UsageByModel),
		Redactions: sc.Counts(),
		StartedAt:  sess.StartedAt,
		EndedAt:    sess.EndedAt,
		LineCount:  countNonBlank(scrubbed),
		SizeBytes:  len(joined),
	}

	chunks := chunkRows(src, scrubbed)

	if opts.DryRun {
		// Report what WOULD be captured without touching the store, so a cautious
		// first run can be inspected before anything is written. The duplicate check
		// still runs, so a dry run after a real one reports honestly.
		if seen, err := db.HasContent(ctx, contentHash); err == nil && seen {
			return outcomeDuplicate, nil
		}
		return outcomeCaptured, nil
	}

	// The blob is written BEFORE the event. Blobs are content-addressed, so an
	// orphaned one is harmless and costs only disk; an event pointing at a missing
	// blob is a dangling reference the corpus cannot repair itself out of. Order
	// the two so the failure mode is the recoverable one.
	if err := writeBlob(db.BlobPath(contentHash), joined); err != nil {
		return outcomeCaptured, fmt.Errorf("write blob: %w", err)
	}

	seq, inserted, err := db.PutSession(ctx, contentHash, rec, chunks)
	if err != nil {
		return outcomeCaptured, err
	}
	if inserted {
		return outcomeCaptured, nil
	}

	// Already stored. Repair rather than skip: a session captured by a build that
	// predates chunking has an event and no chunks, and nothing else will ever
	// offer this file again because dedupe is keyed on content hash. Without this,
	// upgrading leaves the entire existing corpus invisible to the search it was
	// installed for.
	n, err := db.ChunkCount(ctx, seq)
	if err != nil {
		return outcomeDuplicate, fmt.Errorf("chunk count: %w", err)
	}
	if n > 0 || len(chunks) == 0 {
		return outcomeDuplicate, nil
	}

	scope := rec.Repo
	if scope == "" {
		scope = rec.CWD
	}
	occurred := rec.EndedAt
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	if err := db.PutChunks(ctx, seq, string(src), scope, occurred.UTC().Format(time.RFC3339), chunks); err != nil {
		return outcomeDuplicate, fmt.Errorf("backfill chunks: %w", err)
	}
	return outcomeBackfilled, nil
}

// chunkRows extracts readable text from a transcript and groups it into
// indexable windows.
func chunkRows(src sessions.Source, scrubbed []string) []store.ChunkRow {
	chunks := sessions.Chunks(sessions.Segments(src, scrubbed))
	out := make([]store.ChunkRow, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, store.ChunkRow{
			Index:     c.Index,
			LineStart: c.LineStart,
			LineEnd:   c.LineEnd,
			Kind:      string(c.Kind),
			Text:      c.Text,
		})
	}
	return out
}

// writeBlob stores the scrubbed transcript gzipped and content-addressed, written
// atomically so a crash cannot leave a truncated blob that an event points at.
func writeBlob(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path + ".gz"); err == nil {
		return nil // content-addressed: identical bytes, already stored
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if _, err := io.WriteString(zw, body); err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path+".gz")
}

// repoFor resolves a working directory to its normalized owner/name. Reuses the
// discovery package so a repo has exactly one identity everywhere in the binary —
// two normalizers would eventually disagree and split one repo into two.
func repoFor(cwd string) string {
	if cwd == "" {
		return ""
	}
	if _, err := os.Stat(cwd); err != nil {
		return "" // deleted worktree — path matchers still apply downstream
	}
	return discover.NormalizeRemote(discover.Git(cwd, "config", "--get", "remote.origin.url"))
}

func usageMap(in map[string]sessions.Usage) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func countNonBlank(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}
