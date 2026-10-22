package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Session dialect identifiers for BuiltinRuntime.SessionDialect.
//
// A dialect answers one question: how does this CLI name the conversation it
// should append to? It is deliberately separate from the protocol family,
// which answers how the CLI streams events. Prime Agent shares pi's event
// protocol and shares none of its session flags.
const (
	// SessionDialectPi is the pi/omp contract: the caller owns the transcript
	// path and passes it as `--session <file>`. It is also the zero value's
	// meaning, so every runtime identity that predates dialects keeps it.
	SessionDialectPi = "pi"

	// SessionDialectPrime is the Prime Agent contract: the CLI owns the
	// filename inside a `--session-dir`, and an existing conversation is
	// continued with `--resume <selector>`. Verified against
	// PrimeIntellect-ai/prime-agent v0.10.0, crates/pa-cli/src/args.rs
	// (`--resume`, `--session-dir`, `--no-session`, `--fork`; no `--session`).
	SessionDialectPrime = "prime"
)

// errPiResumeTranscriptUnusable reports that a dialect was asked to continue a
// conversation whose transcript cannot be handed to the CLI as-is — it is
// missing, or it exists but holds no turns. Execute turns it into a
// ResumeRejected result so the daemon's fresh-session fallback runs, instead of
// either failing the whole task or inventing a transcript the CLI never wrote.
var errPiResumeTranscriptUnusable = errors.New("session transcript is missing or empty")

// piSessionLaunch is the resolved session selection for one run: what the
// backend will tell the CLI, and what it can lock and publish afterwards.
type piSessionLaunch struct {
	// resuming reports that this launch continues an existing transcript.
	// It is the fresh-vs-resumed distinction the rest of the backend reads;
	// a non-empty ResumeSessionID alone is not that distinction, because a
	// dialect may decline to resume (see errPiResumeTranscriptUnusable).
	resuming bool

	// file is the transcript path when the dialect addresses a session by
	// path. It is "" on a fresh launch under a dialect where the CLI chooses
	// the filename itself — which is also the signal that there is nothing to
	// lock and nothing to publish until the CLI reports its own identity.
	file string

	// dir is the directory the CLI keeps the transcript in. Empty when the
	// dialect does not express one.
	dir string
}

// piObservedSession is what the event stream told us about the session this
// run actually ran in. It is the CLI's own identity claim, not a guess made
// from argv.
type piObservedSession struct {
	// headerID is the id from the stream's session header, when the dialect
	// reads one.
	headerID string
}

// piSessionDialect is the session command-line contract of one
// pi-protocol-family CLI.
//
// Every method is about session lifecycle only. Event decoding, prompt
// delivery, cancellation, usage accounting and stderr retention stay in the
// shared backend, because those are the parts the family genuinely agrees on.
type piSessionDialect interface {
	// name identifies the dialect in errors and logs.
	name() string

	// prepare resolves resumeID into a launch, creating only what the CLI
	// requires to exist before it starts. It never manufactures a transcript
	// the CLI has not written: a resume it cannot honour returns
	// errPiResumeTranscriptUnusable rather than an empty file.
	prepare(resumeID string) (piSessionLaunch, error)

	// args returns the session-selection flags for a launch.
	args(l piSessionLaunch) []string

	// sessionID is the identity to publish for the next turn. Returning ""
	// means "no session to continue", which costs history but never resumes
	// the wrong conversation.
	sessionID(l piSessionLaunch, observed piObservedSession) string

	// readsSessionHeader reports whether the stdout stream carries a session
	// header event whose id names the transcript this run ran in.
	readsSessionHeader() bool

	// resumeRefusalMarkers are the stderr phrases that mean the CLI declined
	// to start from the session it was handed. They gate Result.ResumeRejected,
	// so each one must be a refusal and not merely a warning.
	resumeRefusalMarkers() []string

	// customArgBlocklist is the arg-filter policy for custom_args under this
	// dialect: piBlockedArgs plus the session flags that would fight the
	// daemon for control of the transcript.
	customArgBlocklist() map[string]blockedArgMode
}

// piSessionDialectByName resolves a BuiltinRuntime.SessionDialect value. The
// empty string is the pi dialect so an existing descriptor keeps its
// behaviour; anything unrecognised is an error, because launching a runtime
// under the wrong session flags is the failure the field exists to prevent.
func piSessionDialectByName(name string) (piSessionDialect, error) {
	switch name {
	case "", SessionDialectPi:
		return piDialect{}, nil
	case SessionDialectPrime:
		return primeDialect{}, nil
	default:
		return nil, fmt.Errorf("unknown session dialect %q (supported: %s, %s)", name, SessionDialectPi, SessionDialectPrime)
	}
}

// ── pi/omp dialect ──

// piDialect is the historical behaviour, unchanged: the daemon owns the
// transcript path, creates the file when it is missing, and passes it as
// `--session <file>`. pi refuses to start when `--session` points at a path
// that does not exist, which is why the file is created up front.
type piDialect struct{}

func (piDialect) name() string { return SessionDialectPi }

func (piDialect) prepare(resumeID string) (piSessionLaunch, error) {
	path := resumeID
	if path == "" {
		p, err := newPiSessionPath()
		if err != nil {
			return piSessionLaunch{}, fmt.Errorf("session path: %w", err)
		}
		path = p
	}
	if err := ensurePiSessionFile(path); err != nil {
		return piSessionLaunch{}, fmt.Errorf("session file: %w", err)
	}
	return piSessionLaunch{resuming: resumeID != "", file: path}, nil
}

func (piDialect) args(l piSessionLaunch) []string {
	if l.file == "" {
		return nil
	}
	return []string{"--session", l.file}
}

func (piDialect) sessionID(l piSessionLaunch, _ piObservedSession) string {
	return l.file
}

func (piDialect) readsSessionHeader() bool { return false }

func (piDialect) resumeRefusalMarkers() []string {
	return []string{piResumeRefusedMarker}
}

func (piDialect) customArgBlocklist() map[string]blockedArgMode {
	return piBlockedArgs
}

// ── Prime Agent dialect ──

// primeDialect speaks Prime Agent's session contract. The CLI has no flag that
// takes a transcript path to create: `--session-dir` names the directory it
// keeps transcripts in and it picks the filename, `--resume <selector>` opens
// an existing one. So a fresh run cannot be given a path, and a resumed run
// must be given a transcript Prime actually wrote.
//
// The identity published back to Multica is still an absolute transcript path,
// the same shape the pi dialect publishes. That is not cosmetic: the daemon's
// pi-family resume gate stats the session id before it offers it to a new run,
// so an id in any other shape would be dropped as unreachable on every
// follow-up turn.
type primeDialect struct{}

func (primeDialect) name() string { return SessionDialectPrime }

func (primeDialect) prepare(resumeID string) (piSessionLaunch, error) {
	if resumeID == "" {
		dir, err := newPrimeSessionDir()
		if err != nil {
			return piSessionLaunch{}, fmt.Errorf("session dir: %w", err)
		}
		return piSessionLaunch{dir: dir}, nil
	}
	// A resume is only honourable against a transcript with turns in it. An
	// absent or zero-length file is not a Prime conversation, and handing it to
	// `--resume` would either make Prime refuse the selector or — worse, if the
	// shape ever became acceptable — present an empty transcript as continued
	// history.
	info, err := os.Stat(resumeID)
	switch {
	case err != nil, !info.Mode().IsRegular(), info.Size() == 0:
		return piSessionLaunch{}, errPiResumeTranscriptUnusable
	}
	abs, err := filepath.Abs(resumeID)
	if err != nil {
		return piSessionLaunch{}, fmt.Errorf("session path: %w", err)
	}
	return piSessionLaunch{resuming: true, file: abs, dir: filepath.Dir(abs)}, nil
}

func (primeDialect) args(l piSessionLaunch) []string {
	var args []string
	if l.dir != "" {
		args = append(args, "--session-dir", l.dir)
	}
	if l.resuming && l.file != "" {
		args = append(args, "--resume", l.file)
	}
	return args
}

func (primeDialect) sessionID(l piSessionLaunch, observed piObservedSession) string {
	if l.resuming {
		return l.file
	}
	if l.dir == "" {
		return ""
	}
	return findPrimeSessionTranscript(l.dir, observed.headerID)
}

func (primeDialect) readsSessionHeader() bool { return true }

// primeResumeRefusalMarkers are Prime v0.10.0's selector refusals, taken from
// crates/pa-cli/src/print_runtime.rs.
//
// The browse line is appended by render_selector_error to every selector
// rejection, so it covers the whole class — an unknown id, an unreadable
// transcript — without guessing at wordings not read from the source. The
// project line is the one specific refusal with its own remedy (`--fork`),
// kept separately because it survives even if the generic suffix is reworded.
//
// Both describe a session this run cannot start from, which is exactly
// Result.ResumeRejected's contract: a fresh session is the only cure.
var primeResumeRefusalMarkers = []string{
	"press left-arrow to browse sessions",
	"belongs to a different project",
}

func (primeDialect) resumeRefusalMarkers() []string {
	return primeResumeRefusalMarkers
}

// primeBlockedArgs is piBlockedArgs plus the session flags Prime exposes. The
// daemon owns which conversation a task continues; a custom_args
// `--resume`/`--continue`/`--fork`/`--no-session`/`--session-dir` would
// silently point the run at a different transcript than the one whose id
// Multica recorded, and the next turn would then resume the wrong history.
//
// `--resume` takes an OPTIONAL value in Prime (bare means "most recent"), so
// it is blockedOptionalValue: blocking it with a required value would swallow
// a following unrelated flag's value.
var primeBlockedArgs = mergeBlockedArgs(piBlockedArgs, map[string]blockedArgMode{
	"--session-dir": blockedWithValue,
	"--resume":      blockedOptionalValue,
	"-r":            blockedOptionalValue,
	"--continue":    blockedStandalone,
	"-c":            blockedStandalone,
	"--fork":        blockedWithValue,
	"--no-session":  blockedStandalone,
})

func (primeDialect) customArgBlocklist() map[string]blockedArgMode {
	return primeBlockedArgs
}

func mergeBlockedArgs(base, extra map[string]blockedArgMode) map[string]blockedArgMode {
	merged := make(map[string]blockedArgMode, len(base)+len(extra))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return merged
}

// ── Prime session discovery ──

// primeSessionDir is the parent of the per-conversation directories handed to
// Prime as `--session-dir`. It is deliberately not piSessionDir(): a pi
// transcript and a Prime transcript are different formats, and the pi usage
// scanner reads everything under its own directory.
func primeSessionDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".multica", "prime-sessions"), nil
}

// newPrimeSessionDir mints an empty directory for one new conversation.
//
// One directory per conversation is what makes the transcript Prime creates
// identifiable afterwards: Prime names the file, so the only way to know which
// file is this run's is for the directory to have contained nothing else.
func newPrimeSessionDir() (string, error) {
	parent, err := primeSessionDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, time.Now().UTC().Format("20060102T150405.000000000")+"-")
	if err != nil {
		return "", err
	}
	return dir, nil
}

// primeTranscriptWalkDepth bounds the search below a session directory. Prime
// resolves transcripts relative to the session dir; one level of project
// nesting is tolerated so a layout change does not silently lose every
// session, while an unbounded walk of a user's home directory cannot happen.
const primeTranscriptWalkDepth = 3

// primeTranscriptCandidateLimit bounds how many transcripts are inspected
// before the directory is treated as ambiguous. A fresh conversation directory
// holds one.
const primeTranscriptCandidateLimit = 32

// findPrimeSessionTranscript returns the absolute path of the transcript Prime
// wrote in dir, or "" when it cannot be named with certainty.
//
// headerID is the session id Prime reported on its own stdout. When it is
// known, the transcript is the file whose header carries that id — the CLI's
// identity claim decides, not the filesystem's modification times. With no
// header to go on, a directory holding exactly one transcript is unambiguous
// and anything else is not.
//
// Returning "" is a deliberate outcome, not a swallowed error: the next turn
// then starts a fresh conversation, which loses history. Guessing would resume
// someone else's.
func findPrimeSessionTranscript(dir, headerID string) string {
	candidates := make([]string, 0, 1)
	root := filepath.Clean(dir)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is skipped, not fatal: the transcript may
			// still be found elsewhere under the directory.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if depthBelow(root, path) >= primeTranscriptWalkDepth {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		candidates = append(candidates, path)
		if len(candidates) > primeTranscriptCandidateLimit {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil || len(candidates) == 0 || len(candidates) > primeTranscriptCandidateLimit {
		return ""
	}
	if headerID != "" {
		for _, path := range candidates {
			if primeTranscriptHeaderID(path) == headerID {
				return path
			}
		}
		// Prime named a session and none of these transcripts is it. Publishing
		// one of them anyway would hand the next turn a transcript Prime did
		// not say it was writing.
		return ""
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

func depthBelow(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	depth := 1
	for _, r := range rel {
		if r == filepath.Separator {
			depth++
		}
	}
	return depth
}

// primeTranscriptHeaderMaxBytes bounds the header read. Prime writes the
// session header as the first line of the transcript; a file whose first line
// is longer than this is not one.
const primeTranscriptHeaderMaxBytes = 64 << 10

// primeTranscriptHeaderID reads a transcript's session id from its header
// line. It returns "" for anything that is not a Prime session header, so a
// stray .jsonl in the directory can never be mistaken for a transcript.
func primeTranscriptHeaderID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReaderSize(f, 4<<10)
	line, err := readBoundedLine(reader, primeTranscriptHeaderMaxBytes)
	if err != nil || len(line) == 0 {
		return ""
	}
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(line, &header); err != nil {
		return ""
	}
	if header.Type != piSessionHeaderEvent {
		return ""
	}
	return header.ID
}

func readBoundedLine(reader *bufio.Reader, limit int) ([]byte, error) {
	line := make([]byte, 0, 512)
	for {
		chunk, more, err := reader.ReadLine()
		line = append(line, chunk...)
		if err != nil {
			return line, err
		}
		if len(line) > limit {
			return nil, fmt.Errorf("session header exceeds %d bytes", limit)
		}
		if !more {
			return line, nil
		}
	}
}
