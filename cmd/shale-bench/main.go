// shale-bench prepares and runs private, explicitly invoked Jev experiments.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nonlinear-xyz/shale/internal/artifacts"
	"github.com/nonlinear-xyz/shale/internal/benchmark"
	"github.com/nonlinear-xyz/shale/internal/jev"
	"github.com/nonlinear-xyz/shale/internal/pack"
	"github.com/nonlinear-xyz/shale/internal/store"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: shale-bench prepare|inspect|run|batch [flags]")
	}
	switch os.Args[1] {
	case "inspect":
		return inspect(os.Args[2:])
	case "prepare":
		return prepare(os.Args[2:])
	case "run":
		return evaluate(os.Args[2:])
	case "batch":
		return batching(os.Args[2:])
	default:
		return errors.New("unknown benchmark command")
	}
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = f.Write(append(b, '\n'))
	return e
}
func hashFile(path string) (string, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func commit() string {
	b, e := exec.Command("git", "rev-parse", "HEAD").Output()
	if e != nil {
		return "unknown"
	}
	return string(b)
}

func prepare(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	dir := fs.String("dir", "", "private snapshot directory")
	budget := fs.Int("budget", 2000, "packet token budget for all cases")
	mode := fs.String("mode", "historical_transcript_only", "historical_transcript_only or present_day (snapshot existing harness memories)")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *dir == "" {
		return errors.New("--dir required")
	}
	if *mode != "historical_transcript_only" && *mode != "present_day" {
		return errors.New("unsupported preparation mode")
	}
	if *budget < pack.MinBudget || *budget > pack.MaxBudget {
		return errors.New("budget outside packet limits")
	}
	var tasks struct {
		CorpusFingerprint string `json:"corpusFingerprint"`
		Mode              string `json:"mode"`
		Cases             []struct {
			ID            string `json:"id"`
			Task          string `json:"task"`
			Repo          string `json:"repo"`
			SourceKey     string `json:"sourceKey"`
			At            string `json:"at"`
			Split         string `json:"split"`
			TaskSourceRef string `json:"taskSourceRef"`
			TaskReviewed  bool   `json:"taskReviewed"`
		} `json:"cases"`
	}
	b, e := os.ReadFile(filepath.Join(*dir, "tasks.json"))
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &tasks); e != nil {
		return e
	}
	if _, e = os.Stat(filepath.Join(*dir, "suite.json")); e == nil {
		return errors.New("suite.json exists; preserve labels by using a fresh snapshot directory")
	}
	root := filepath.Join(*dir, "store")
	// Only the explicitly prepared private snapshot is migrated/edited.
	db, e := store.Open(root)
	if e != nil {
		return e
	}
	defer db.Close()
	if *mode == "present_day" {
		result := artifacts.Refresh(context.Background(), db, artifacts.Options{})
		if e = writeJSON(filepath.Join(*dir, "memory-refresh.json"), map[string]any{"scanned": result.Scanned, "indexed": result.Indexed, "unchanged": result.Unchanged, "skipped": len(result.Skipped), "errors": len(result.Errors)}); e != nil {
			return e
		}
		if len(result.Errors) > 0 {
			return fmt.Errorf("memory snapshot refresh had %d errors; see the isolated snapshot", len(result.Errors))
		}
	}
	q, e := sql.Open("sqlite", "file:"+filepath.Join(root, "shale.db")+"?_pragma=busy_timeout(5000)")
	if e != nil {
		return e
	}
	defer q.Close()
	if _, e = q.Exec(`CREATE TABLE benchmark_chunks AS SELECT * FROM chunks_fts`); e != nil {
		return fmt.Errorf("snapshot already prepared or invalid: %w", e)
	}
	if *mode == "historical_transcript_only" {
		if _, e = q.Exec(`DELETE FROM artifacts_fts`); e != nil {
			return e
		}
	}
	suite := benchmark.Suite{Version: 1, CorpusFingerprint: tasks.CorpusFingerprint, Cases: []benchmark.Case{}}
	skipped := []map[string]string{}
	for _, t := range tasks.Cases {
		at, e := time.Parse(time.RFC3339Nano, t.At)
		if e != nil {
			return e
		}
		if *mode == "present_day" {
			at = time.Now().UTC()
		}
		// FTS contains only completed sessions in the historical 30-day window.
		// Exclude all versions of the target source session, including earlier captures.
		tx, e := q.Begin()
		if e != nil {
			return e
		}
		if _, e = tx.Exec(`DELETE FROM chunks_fts`); e != nil {
			tx.Rollback()
			return e
		}
		_, e = tx.Exec(`INSERT INTO chunks_fts SELECT * FROM benchmark_chunks WHERE julianday(occurred_at) < julianday(?) AND julianday(occurred_at) >= julianday(?) AND CAST(event_seq AS INTEGER) NOT IN (SELECT seq FROM events WHERE json_extract(payload,'$.sourceKey') = ?)`, at.Format(time.RFC3339Nano), at.AddDate(0, 0, -30).Format(time.RFC3339Nano), t.SourceKey)
		if e != nil {
			tx.Rollback()
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		c, e := pack.Retrieve(context.Background(), db, pack.Input{Task: t.Task, Repo: t.Repo, SinceDays: 36500, TokenBudget: *budget, Now: at})
		if e != nil {
			return e
		}
		c.Packet.SinceDays = 30
		if len(pack.RankingCandidates(c)) < 2 {
			skipped = append(skipped, map[string]string{"id": t.ID, "reason": "fewer_than_two_candidates"})
			continue
		}
		suite.Cases = append(suite.Cases, benchmark.Case{ID: t.ID, Split: t.Split, Mode: *mode, TaskSourceRef: t.TaskSourceRef, TaskReviewed: t.TaskReviewed, LabelNotes: "Review the task excerpt and full candidate text. Label required/helpful/irrelevant refs before any Jev run; mark critical refs separately. Add required refs outside this candidate pool when known.", Required: []string{}, Critical: []string{}, Helpful: []string{}, Irrelevant: []string{}, Candidates: *c})
	}
	if e = writeJSON(filepath.Join(*dir, "suite.json"), suite); e != nil {
		return e
	}
	if e = writeJSON(filepath.Join(*dir, "skipped.json"), skipped); e != nil {
		return e
	}
	fmt.Printf("Prepared %d cases; %d skipped. Labels remain unreviewed. No provider calls made.\n", len(suite.Cases), len(skipped))
	return nil
}

func inspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	path := fs.String("suite", "", "private suite path")
	if e := fs.Parse(args); e != nil {
		return e
	}
	suite, e := benchmark.ReadSuite(*path)
	if e != nil {
		return e
	}
	rows := []map[string]any{}
	for _, c := range suite.Cases {
		candidates := pack.RankingCandidates(&c.Candidates)
		p, err := jev.Prepare(c.Candidates.Packet.Task, candidates, jev.DefaultModel)
		row := map[string]any{"caseId": c.ID, "split": c.Split, "reviewed": c.Reviewed, "candidates": len(candidates)}
		if err != nil {
			row["preparationError"] = err.Error()
		} else {
			state, request := p.Sizes()
			row["stateBytes"] = state
			row["requestBytes"] = request
		}
		rows = append(rows, row)
	}
	b, _ := json.MarshalIndent(rows, "", "  ")
	fmt.Println(string(b))
	return nil
}

// sourceFingerprint includes uncommitted implementation files as well as HEAD.
func sourceFingerprint() string {
	h := sha256.New()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !(strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".py") || path == "go.mod" || path == "go.sum") {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		fmt.Fprintf(h, "%s\x00%d\x00", path, len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(h.Sum(nil))
}
