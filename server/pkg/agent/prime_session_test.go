package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These tests cover the Prime Agent session dialect. Two kinds appear here and
// they prove different things:
//
//   - Contract tests drive a fake executable that implements the parts of
//     Prime v0.10.0's command-line and stdout contract this adapter depends
//     on, read from crates/pa-cli/src/args.rs and crates/pa-cli/src/
//     print_runtime.rs at that tag. They prove Multica's half of the contract
//     deterministically and offline. They are NOT evidence that the real
//     binary behaves this way.
//   - The real binary's behaviour is proven only by the one thing recorded
//     from it: `prime-agent -p --mode json --session <file> --offline` exits 1
//     with `Error: Unknown option: --session`. That exact failure is
//     reproduced in TestPrimeExecuteRegressionUnknownSessionOption.
//
// Nothing here runs inference, reaches a provider, or asserts anything about
// token accounting or output quality from the real engine.

// ── Fake Prime executable ──

// primeFake describes a fake `prime-agent` for one test.
type primeFake struct {
	// argvLog receives one argv token per line.
	argvLog string
	// stdinLog receives the prompt the backend wrote on stdin.
	stdinLog string
	// ranMarker is created the moment the process starts, so a test can prove
	// the backend did NOT launch it.
	ranMarker string
	// sessionID is announced in a `{"type":"session","id":...}` stdout header
	// line, as Prime does in json mode. Empty omits the header.
	sessionID string
	// transcripts are written into the directory named by --session-dir, each
	// carrying a session header with the given id. A fresh Prime run writes
	// exactly one.
	transcripts []primeFakeTranscript
	// events are emitted on stdout after the header.
	events []string
	// stderrLines are written to stderr before exiting.
	stderrLines []string
	// exitCode is the process exit status.
	exitCode int
	// sleep holds the process open for this many seconds before emitting
	// anything, for cancellation tests.
	sleep int
}

type primeFakeTranscript struct {
	name string
	id   string
}

// script renders the fake as a POSIX shell script. Values must not contain
// single quotes (same constraint as piEventStreamScript).
func (f primeFake) script() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if f.ranMarker != "" {
		fmt.Fprintf(&b, ": > '%s'\n", f.ranMarker)
	}
	if f.argvLog != "" {
		fmt.Fprintf(&b, ": > '%s'\n", f.argvLog)
		fmt.Fprintf(&b, "for a in \"$@\"; do printf '%%s\\n' \"$a\" >> '%s'; done\n", f.argvLog)
	}
	// Prime v0.10.0's parser has no --session flag, so it falls through to the
	// unknown-option arm: `Error: Unknown option: --session`, exit 1, nothing
	// on stdout. That is the confirmed real failure this adapter exists to
	// remove, so the fake reproduces it rather than tolerating the flag.
	b.WriteString("session_dir=''\n")
	b.WriteString("resume=''\n")
	b.WriteString("prev=''\n")
	b.WriteString("for a in \"$@\"; do\n")
	b.WriteString("  case \"$prev\" in\n")
	b.WriteString("    --session-dir) session_dir=\"$a\" ;;\n")
	b.WriteString("    --resume) resume=\"$a\" ;;\n")
	b.WriteString("  esac\n")
	b.WriteString("  case \"$a\" in\n")
	b.WriteString("    --session)\n")
	b.WriteString("      printf 'Error: Unknown option: --session\\n' >&2\n")
	b.WriteString("      exit 1 ;;\n")
	b.WriteString("  esac\n")
	b.WriteString("  prev=\"$a\"\n")
	b.WriteString("done\n")
	// Real Prime reads the piped prompt to EOF before it runs a turn. Draining
	// here keeps the backend's concurrent prompt write from hitting EPIPE.
	if f.stdinLog != "" {
		fmt.Fprintf(&b, "cat > '%s'\n", f.stdinLog)
	} else {
		b.WriteString("cat > /dev/null\n")
	}
	if f.sleep > 0 {
		fmt.Fprintf(&b, "sleep %d\n", f.sleep)
	}
	// A fresh run materialises its transcript inside --session-dir; a resumed
	// run appends to the one it was handed and creates nothing.
	for _, t := range f.transcripts {
		b.WriteString("if [ -n \"$session_dir\" ] && [ -z \"$resume\" ]; then\n")
		fmt.Fprintf(&b, "  printf '%%s\\n' '{\"type\":\"session\",\"id\":\"%s\",\"cwd\":\"/tmp\"}' > \"$session_dir/%s\"\n", t.id, t.name)
		b.WriteString("fi\n")
	}
	if f.sessionID != "" {
		fmt.Fprintf(&b, "printf '%%s\\n' '{\"type\":\"session\",\"version\":1,\"id\":\"%s\",\"cwd\":\"/tmp\"}'\n", f.sessionID)
	}
	for _, e := range f.events {
		fmt.Fprintf(&b, "printf '%%s\\n' '%s'\n", e)
	}
	for _, line := range f.stderrLines {
		fmt.Fprintf(&b, "printf '%%s\\n' '%s' >&2\n", line)
	}
	if f.exitCode != 0 {
		fmt.Fprintf(&b, "exit %d\n", f.exitCode)
	}
	return b.String()
}

func (f primeFake) install(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "prime-agent")
	writeTestExecutable(t, path, []byte(f.script()))
	return path
}

func primeOKEvents(text string) []string {
	return []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"` + text + `"}}`,
		`{"type":"turn_end","message":{"role":"assistant","model":"prime-test","usage":{"input":11,"output":7,"cacheRead":3,"cacheWrite":1}}}`,
	}
}

// primeBackend builds the Prime runtime the way the daemon does, through the
// descriptor registry rather than by constructing piBackend by hand — so these
// tests also cover the descriptor→dialect wiring.
func primeBackend(t *testing.T, execPath string) Backend {
	t.Helper()
	backend, err := ResolveBackend("prime", Config{ExecutablePath: execPath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("resolve prime backend: %v", err)
	}
	return backend
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read argv log %s: %v", path, err)
	}
	var argv []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			argv = append(argv, line)
		}
	}
	return argv
}

func awaitResult(t *testing.T, session *Session) Result {
	t.Helper()
	go func() {
		for range session.Messages {
		}
	}()
	select {
	case result, ok := <-session.Result:
		if !ok {
			t.Fatal("result channel closed without a value")
		}
		return result
	case <-time.After(20 * time.Second):
		t.Fatal("timeout waiting for result")
		return Result{}
	}
}

func skipIfNoShellFixtures(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
}

// isolateHome points the dialect's session root at a scratch directory so a
// fresh-conversation test cannot read or write the developer's real
// ~/.multica. Tests that call it must not run in parallel.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// ── Invocation contract ──

// TestPrimeExecuteRegressionUnknownSessionOption is the AEGI-639 regression.
//
// Before the dialect existed, the Prime runtime shared pi's session flags, so
// every launch carried `--session <path>`. Prime v0.10.0 has no such flag:
// args.rs routes it to the unknown-option arm, which prints
// `Error: Unknown option: --session` and exits 1 with an empty stdout — the
// exact failure recorded from the real binary on Aegis, before any model
// request. The fake reproduces that rule, so this test fails on the old
// behaviour and passes only when the flag is gone.
func TestPrimeExecuteRegressionUnknownSessionOption(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	fake := primeFake{
		argvLog:     filepath.Join(dir, "argv"),
		sessionID:   "sess-regression",
		transcripts: []primeFakeTranscript{{name: "conversation.jsonl", id: "sess-regression"}},
		events:      primeOKEvents("hello from prime"),
	}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "the task prompt", ExecOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.Status != "completed" {
		t.Fatalf("status = %q, error = %q; want completed (the unsupported flag must be gone)", result.Status, result.Error)
	}
	if result.Output != "hello from prime" {
		t.Fatalf("Output = %q, want the streamed text", result.Output)
	}
	argv := readArgv(t, fake.argvLog)
	for _, arg := range argv {
		if arg == "--session" {
			t.Fatalf("--session reached Prime: %v", argv)
		}
	}
}

func TestPrimeFreshLaunchArgsNameOnlyTheSessionDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	args := buildPiArgsForLaunch(primeDialect{}, piSessionLaunch{dir: dir}, ExecOptions{}, slog.Default())

	if got := strings.Join(args[:3], " "); got != "-p --mode json" {
		t.Fatalf("args start with %q, want the protocol flags pinned ahead of anything variable", got)
	}
	assertArgPair(t, args, "--session-dir", dir)
	for _, arg := range args {
		switch arg {
		case "--session":
			t.Fatalf("fresh launch forwarded the unsupported flag: %v", args)
		case "--resume", "-r", "--continue", "-c", "--fork":
			t.Fatalf("fresh launch asked Prime to continue a conversation: %v", args)
		}
	}
}

func TestPrimeResumeArgsNameTheTranscript(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	transcript := filepath.Join(dir, "conversation.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"session","id":"sess-1"}`+"\n"), 0o644); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}

	launch, err := primeDialect{}.prepare(transcript)
	if err != nil {
		t.Fatalf("prepare resume: %v", err)
	}
	if !launch.resuming {
		t.Fatal("a transcript with turns in it must resolve to a resuming launch")
	}

	args := buildPiArgsForLaunch(primeDialect{}, launch, ExecOptions{}, slog.Default())
	assertArgPair(t, args, "--resume", transcript)
	assertArgPair(t, args, "--session-dir", dir)
	for _, arg := range args {
		if arg == "--session" {
			t.Fatalf("resume forwarded the unsupported flag: %v", args)
		}
	}
}

// An empty file is not a conversation. Treating one as resumable is the
// specific shortcut that turns "resume" into "replay nothing and call it
// history", so the dialect refuses instead of handing it to --resume.
func TestPrimePrepareRefusesEmptyAndMissingTranscripts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("seed empty transcript: %v", err)
	}

	for name, path := range map[string]string{
		"empty":   empty,
		"missing": filepath.Join(dir, "absent.jsonl"),
		"dir":     dir,
	} {
		if _, err := (primeDialect{}).prepare(path); err == nil {
			t.Fatalf("%s: prepare accepted %q as resumable", name, path)
		} else if !strings.Contains(err.Error(), "missing or empty") {
			t.Fatalf("%s: prepare error = %v, want the unusable-transcript sentinel", name, err)
		}
	}
}

func TestPrimeCustomArgsCannotHijackTheSession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	args := buildPiArgsForLaunch(primeDialect{}, piSessionLaunch{dir: dir}, ExecOptions{
		CustomArgs: []string{
			"--session", "/tmp/hijack.jsonl",
			"--session-dir", "/tmp/hijack-dir",
			"--resume", "/tmp/other.jsonl",
			"--continue",
			"--fork", "/tmp/fork.jsonl",
			"--no-session",
			"--verbose",
			"--models", "a,b",
		},
	}, slog.Default())

	joined := strings.Join(args, "\x00")
	for _, leaked := range []string{"/tmp/hijack.jsonl", "/tmp/hijack-dir", "/tmp/other.jsonl", "/tmp/fork.jsonl"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("custom_args pointed the run at %q: %v", leaked, args)
		}
	}
	for _, blocked := range []string{"--continue", "--no-session", "--fork"} {
		for _, arg := range args {
			if arg == blocked {
				t.Errorf("custom_args kept session flag %q: %v", blocked, args)
			}
		}
	}
	// Non-session options are still the user's to set.
	assertArgPair(t, args, "--models", "a,b")
	if !containsArg(args, "--verbose") {
		t.Errorf("--verbose was dropped: %v", args)
	}
	// The daemon's own session flags survive the filter intact.
	assertArgPair(t, args, "--session-dir", dir)
}

func TestPrimeDialectKeepsModelAndThinkingPassthrough(t *testing.T) {
	t.Parallel()

	args := buildPiArgsForLaunch(primeDialect{}, piSessionLaunch{dir: t.TempDir()}, ExecOptions{
		Model:         "openai/gpt-5-codex",
		ThinkingLevel: "medium",
	}, slog.Default())

	assertArgPair(t, args, "--model", "openai/gpt-5-codex")
	assertArgPair(t, args, "--thinking", "medium")
}

// ── Dialect isolation: pi and omp must not move ──

func TestPiAndOmpKeepTheSessionFileDialect(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		provider string
		resolve  func() (Backend, error)
	}{
		{"pi", func() (Backend, error) { return New("pi", Config{Logger: slog.Default()}) }},
		{"omp", func() (Backend, error) { return ResolveBackend("omp", Config{Logger: slog.Default()}) }},
	} {
		backend, err := tc.resolve()
		if err != nil {
			t.Fatalf("%s: resolve: %v", tc.provider, err)
		}
		pb, ok := backend.(*piBackend)
		if !ok {
			t.Fatalf("%s: backend is %T, want *piBackend", tc.provider, backend)
		}
		if name := pb.dialect().name(); name != SessionDialectPi {
			t.Fatalf("%s: dialect = %q, want %q", tc.provider, name, SessionDialectPi)
		}

		args := buildPiArgsForLaunch(pb.dialect(), piSessionLaunch{file: "/tmp/s.jsonl"}, ExecOptions{}, slog.Default())
		assertArgPair(t, args, "--session", "/tmp/s.jsonl")
		for _, arg := range args {
			if arg == "--session-dir" || arg == "--resume" {
				t.Fatalf("%s: Prime-only session flag leaked into the pi dialect: %v", tc.provider, args)
			}
		}
	}
}

// The pi dialect still publishes the path the daemon chose, and still creates
// the file up front — pi refuses to start when --session points at a path that
// does not exist, so that creation is load-bearing for pi and must not have
// been moved by the refactor.
func TestPiDialectPreparesAndPublishesTheDaemonPath(t *testing.T) {
	isolateHome(t)

	chosen := filepath.Join(t.TempDir(), "resumed.jsonl")
	launch, err := piDialect{}.prepare(chosen)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if launch.file != chosen || !launch.resuming {
		t.Fatalf("launch = %+v, want the given path as a resuming launch", launch)
	}
	if _, err := os.Stat(chosen); err != nil {
		t.Fatalf("pi dialect must create the session file up front: %v", err)
	}
	if got := (piDialect{}).sessionID(launch, piObservedSession{headerID: "ignored"}); got != chosen {
		t.Fatalf("sessionID = %q, want the daemon's path regardless of any header", got)
	}

	fresh, err := piDialect{}.prepare("")
	if err != nil {
		t.Fatalf("prepare fresh: %v", err)
	}
	if fresh.resuming {
		t.Fatal("a launch with no prior id must not be marked resuming")
	}
	if !strings.HasSuffix(fresh.file, ".jsonl") {
		t.Fatalf("fresh pi session path = %q, want a .jsonl under the pi session dir", fresh.file)
	}
}

func TestUnknownSessionDialectFailsClosed(t *testing.T) {
	t.Parallel()

	if _, err := piSessionDialectByName("not-a-dialect"); err == nil {
		t.Fatal("an unrecognised dialect must not resolve to the family default")
	}
	var b piBackend
	if err := b.applyBuiltinRuntimeOverrides(BuiltinRuntime{ID: "x", SessionDialect: "not-a-dialect"}); err == nil {
		t.Fatal("applying an unknown dialect must fail rather than keep the family's flags")
	}
	if b.defaultExecutable != "" {
		t.Fatal("a failed override must not partially mutate the backend")
	}
}

func TestPrimeDescriptorIsRegisteredAsAPiFamilyRuntime(t *testing.T) {
	t.Parallel()

	desc, ok := BuiltinRuntimeByID("prime")
	if !ok {
		t.Fatal("prime is not a registered built-in runtime identity")
	}
	if desc.ProtocolFamily != "pi" {
		t.Fatalf("ProtocolFamily = %q, want pi", desc.ProtocolFamily)
	}
	if desc.SessionDialect != SessionDialectPrime {
		t.Fatalf("SessionDialect = %q, want %q", desc.SessionDialect, SessionDialectPrime)
	}
	if desc.DefaultCommand != "prime-agent" || desc.DefaultExecutable != "prime-agent" {
		t.Fatalf("descriptor command = %q/%q, want prime-agent", desc.DefaultCommand, desc.DefaultExecutable)
	}
	// prime must not be a protocol family: a custom runtime profile basing
	// itself on "prime" would bypass the descriptor and its dialect.
	if IsSupportedType("prime") {
		t.Fatal("prime must stay a runtime identity, not a protocol_family")
	}
	// Model discovery is deliberately absent: v0.10.0 removed --list-models.
	if desc.ModelDiscovery != nil {
		t.Fatal("prime must not inherit a discovery command that is known to fail")
	}
	cat, err := ListModels(context.Background(), "prime", Command{})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(cat.Models) != 0 {
		t.Fatalf("ListModels returned %d models, want an empty catalog (manual entry)", len(cat.Models))
	}
}

// ── Session identity ──

// A fresh Prime run cannot be told where to write, so the identity Multica
// records has to come from Prime: the id on its stdout session header names
// the transcript it created inside --session-dir.
func TestPrimeExecutePublishesTheTranscriptPrimeWrote(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	fake := primeFake{
		argvLog:   filepath.Join(dir, "argv"),
		sessionID: "sess-abc123",
		transcripts: []primeFakeTranscript{
			{name: "20261022-000001.jsonl", id: "sess-abc123"},
		},
		events: primeOKEvents("first answer"),
	}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "first prompt", ExecOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.Status != "completed" {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if result.SessionID == "" {
		t.Fatal("no session identity published; the next turn would start over")
	}
	if !filepath.IsAbs(result.SessionID) {
		t.Fatalf("SessionID = %q, want an absolute path (the daemon stats it before reuse)", result.SessionID)
	}
	if filepath.Base(result.SessionID) != "20261022-000001.jsonl" {
		t.Fatalf("SessionID = %q, want the transcript Prime announced", result.SessionID)
	}
	// The published id has to satisfy the daemon's own pi-family resume gate,
	// which stats it and requires a regular, non-empty file.
	info, err := os.Stat(result.SessionID)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("published id is not a resumable transcript: stat=%v err=%v", info, err)
	}
	// --session-dir must be under the isolated session root, not the workdir.
	argv := readArgv(t, fake.argvLog)
	sessionDir := argValue(argv, "--session-dir")
	if sessionDir == "" {
		t.Fatalf("no --session-dir in argv: %v", argv)
	}
	if filepath.Dir(result.SessionID) != sessionDir {
		t.Fatalf("transcript %q is not inside --session-dir %q", result.SessionID, sessionDir)
	}
}

// When Prime names a session and no transcript in the directory carries that
// id, there is nothing we can honestly publish. Publishing the newest file
// anyway would hand the next turn a conversation Prime never said it wrote.
func TestPrimeExecutePublishesNothingWhenTheTranscriptCannotBeNamed(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	fake := primeFake{
		sessionID: "sess-announced",
		transcripts: []primeFakeTranscript{
			{name: "one.jsonl", id: "sess-other-1"},
			{name: "two.jsonl", id: "sess-other-2"},
		},
		events: primeOKEvents("answer"),
	}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.Status != "completed" {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if result.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty rather than a guessed transcript", result.SessionID)
	}
}

func TestPrimeExecuteResumeKeepsTheSessionIdentity(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "conversation")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	transcript := filepath.Join(sessionDir, "live.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"session","id":"sess-live","cwd":"/tmp"}`+"\n"), 0o644); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}

	fake := primeFake{
		argvLog:   filepath.Join(dir, "argv"),
		sessionID: "sess-live",
		events:    primeOKEvents("second answer"),
	}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "second prompt", ExecOptions{
		Timeout:         15 * time.Second,
		ResumeSessionID: transcript,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.Status != "completed" {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if result.SessionID != transcript {
		t.Fatalf("SessionID = %q, want the resumed transcript %q", result.SessionID, transcript)
	}
	argv := readArgv(t, fake.argvLog)
	if got := argValue(argv, "--resume"); got != transcript {
		t.Fatalf("--resume = %q, want %q (argv: %v)", got, transcript, argv)
	}
	if got := argValue(argv, "--session-dir"); got != sessionDir {
		t.Fatalf("--session-dir = %q, want the transcript's directory %q", got, sessionDir)
	}
}

// The transcript this turn was told to continue is gone. The adapter must say
// "that resume is refused" so the daemon's fresh-session fallback runs — and
// must not launch the CLI at all, because there is nothing to hand it.
func TestPrimeExecuteRejectsAVanishedTranscriptWithoutLaunching(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	fake := primeFake{ranMarker: filepath.Join(dir, "ran")}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{
		Timeout:         15 * time.Second,
		ResumeSessionID: filepath.Join(dir, "gone.jsonl"),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.Status != "failed" || !result.ResumeRejected {
		t.Fatalf("result = %+v, want failed with ResumeRejected", result)
	}
	if result.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty so the dead pointer is not served again", result.SessionID)
	}
	if _, err := os.Stat(fake.ranMarker); err == nil {
		t.Fatal("the CLI was launched for a resume that had no transcript")
	}
}

// Prime refuses a selector it cannot open, and render_selector_error appends
// the same browse line to every such refusal. That phrase is the family's
// evidence that only a fresh session can cure the run.
func TestPrimeExecuteReportsARefusedSelectorAsResumeRejected(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	transcript := filepath.Join(dir, "stale.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"session","id":"sess-stale"}`+"\n"), 0o644); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
	fake := primeFake{
		stderrLines: []string{
			"session sess-stale belongs to a different project (/gone). Pass --fork sess-stale to use it here, or run from that project directory.",
			"Open prime-agent and press left-arrow to browse sessions.",
		},
		exitCode: 1,
	}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{
		Timeout:         15 * time.Second,
		ResumeSessionID: transcript,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !result.ResumeRejected {
		t.Fatalf("ResumeRejected = false for a refused selector; error = %q", result.Error)
	}
	if !strings.Contains(result.Error, "different project") {
		t.Fatalf("Error = %q, want the refusal text preserved", result.Error)
	}
}

// A cold run cannot have had a resume refused, whatever the CLI prints.
func TestPrimeExecuteNeverReportsResumeRejectedOnAFreshRun(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	fake := primeFake{
		stderrLines: []string{"Open prime-agent and press left-arrow to browse sessions."},
		exitCode:    1,
	}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.ResumeRejected {
		t.Fatal("a run that asked for no resume cannot have had one refused")
	}
}

// ── Startup failure reporting ──

// A process that dies before its first event has only stderr to explain
// itself. Without this the run record read "exited with error: exit status 1"
// and the actual cause — a rejected command line — lived only in the daemon's
// debug log, which is how AEGI-614 spent a day looking like an auth problem.
func TestPrimeExecuteKeepsStartupStderrInTheResult(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	fake := primeFake{
		stderrLines: []string{"Error: Unknown option: --not-a-flag"},
		exitCode:    1,
	}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Error, "Unknown option: --not-a-flag") {
		t.Fatalf("Error = %q, want the startup stderr carried into the result", result.Error)
	}
	if !strings.Contains(result.Error, "exited with error") {
		t.Fatalf("Error = %q, want the process exit still visible", result.Error)
	}
}

// Once the protocol has started, the stream carries the structured error and
// stderr must stay out of the result — otherwise every provider message is
// reported twice. This is the pi-side regression for the change above.
func TestPiStartupDiagnosticLeavesEventBearingFailuresAlone(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	events := []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"turn_end","message":{"role":"assistant","model":"test","usage":{"input":0,"output":0},"stopReason":"error","errorMessage":"Connection error."}}`,
	}
	script := piEventStreamScriptWithExit(events, 1) + "printf 'noisy startup warning\\n' >&2\n"
	fakePath := filepath.Join(t.TempDir(), "pi")
	writeTestExecutable(t, fakePath, []byte(script))

	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Error, "Connection error.") {
		t.Fatalf("Error = %q, want the provider message", result.Error)
	}
	if strings.Contains(result.Error, "noisy startup warning") {
		t.Fatalf("Error = %q, want stderr left out once the stream explained the failure", result.Error)
	}
}

func TestPiStartupDiagnosticIsBoundedAndTrailing(t *testing.T) {
	t.Parallel()

	w := newPiStderrWatcher(discardWriter{})
	for i := 0; i < 10; i++ {
		if _, err := w.Write([]byte(fmt.Sprintf("line %d\n", i))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	got := w.startupDiagnostic()
	if got != "line 7; line 8; line 9" {
		t.Fatalf("startupDiagnostic = %q, want the last three lines", got)
	}

	long := newPiStderrWatcher(discardWriter{})
	if _, err := long.Write([]byte(strings.Repeat("x", 4096))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if n := len(long.startupDiagnostic()); n > piStartupDiagnosticLimit+4 {
		t.Fatalf("startupDiagnostic length = %d, want it bounded near %d", n, piStartupDiagnosticLimit)
	}
	if empty := newPiStderrWatcher(discardWriter{}).startupDiagnostic(); empty != "" {
		t.Fatalf("startupDiagnostic = %q for silent stderr, want empty", empty)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// ── Prompt delivery, decoding, cancellation ──

// The prompt rides piped stdin exactly once and never appears on argv. Prime
// reads a non-TTY stdin as its initial message (piped_stdin.rs) and `-p` also
// accepts a positional message, so a prompt on argv would be delivered twice.
func TestPrimeExecuteDeliversThePromptOnceOnStdin(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	const prompt = "do the thing, carefully"
	dir := t.TempDir()
	fake := primeFake{
		argvLog:     filepath.Join(dir, "argv"),
		stdinLog:    filepath.Join(dir, "stdin"),
		sessionID:   "sess-stdin",
		transcripts: []primeFakeTranscript{{name: "s.jsonl", id: "sess-stdin"}},
		events:      primeOKEvents("done"),
	}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, prompt, ExecOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result := awaitResult(t, session); result.Status != "completed" {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}

	raw, err := os.ReadFile(fake.stdinLog)
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	if got := string(raw); got != prompt {
		t.Fatalf("stdin = %q, want the prompt exactly once and nothing else", got)
	}
	for _, arg := range readArgv(t, fake.argvLog) {
		if strings.Contains(arg, "do the thing") {
			t.Fatalf("prompt leaked into argv: %v", readArgv(t, fake.argvLog))
		}
	}
}

// An empty prompt is rejected before the process starts: Prime trims piped
// stdin, so whitespace-only input would become a run with no turn, no output
// and a transcript nobody can use.
func TestPrimeExecuteRejectsAnEmptyPrompt(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	fake := primeFake{ranMarker: filepath.Join(dir, "ran")}
	backend := primeBackend(t, fake.install(t, dir))

	if _, err := backend.Execute(context.Background(), "   \n\t ", ExecOptions{Timeout: 5 * time.Second}); err == nil {
		t.Fatal("an empty prompt must not start a run")
	} else if !strings.Contains(err.Error(), "prime-agent prompt must not be empty") {
		t.Fatalf("error = %v, want the provider-labelled empty-prompt rejection", err)
	}
	if _, err := os.Stat(fake.ranMarker); err == nil {
		t.Fatal("the CLI was launched for an empty prompt")
	}
}

// This asserts MULTICA's decoder against the pi event shapes, under the Prime
// runtime's label and dialect. It is NOT evidence that Prime v0.10.0 emits
// these shapes: agent_event_json was not read, and the one Prime emitter that
// WAS read (print_boundary events.rs) uses message_start/message_end wrappers
// this decoder ignores. Proving the real stream is the native canary's job.
func TestPrimeExecuteDecodesFinalOutputAndUsageFromPiShapedEvents(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	fake := primeFake{
		sessionID:   "sess-usage",
		transcripts: []primeFakeTranscript{{name: "s.jsonl", id: "sess-usage"}},
		events: []string{
			`{"type":"agent_start"}`,
			`{"type":"turn_start"}`,
			`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"partial"}}`,
			`{"type":"turn_end","message":{"role":"assistant","model":"prime/m1","usage":{"input":5,"output":2,"cacheRead":1,"cacheWrite":0}}}`,
			`{"type":"turn_start"}`,
			`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"final text"}}`,
			`{"type":"turn_end","message":{"role":"assistant","model":"prime/m1","usage":{"input":7,"output":3,"cacheRead":0,"cacheWrite":2}}}`,
		},
	}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.Status != "completed" {
		t.Fatalf("status = %q, error = %q", result.Status, result.Error)
	}
	if result.Output != "final text" {
		t.Fatalf("Output = %q, want only the last turn's text", result.Output)
	}
	got, ok := result.Usage["prime/m1"]
	if !ok {
		t.Fatalf("Usage = %+v, want an entry for the reported model", result.Usage)
	}
	want := TokenUsage{InputTokens: 12, OutputTokens: 5, CacheReadTokens: 1, CacheWriteTokens: 2}
	if got != want {
		t.Fatalf("Usage = %+v, want %+v (summed across turns)", got, want)
	}
}

// The session header is a Prime-only event. A pi-family CLI that never emits
// one must keep publishing the daemon's own path, so reading the header
// cannot have changed pi's recorded identity.
func TestSessionHeaderIsIgnoredByThePiDialect(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	events := append([]string{`{"type":"session","id":"sess-should-be-ignored"}`}, primeOKEvents("ok")...)
	fakePath := filepath.Join(t.TempDir(), "pi")
	writeTestExecutable(t, fakePath, []byte(piEventStreamScript(events)))

	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	chosen := filepath.Join(t.TempDir(), "session.jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{
		Timeout:         15 * time.Second,
		ResumeSessionID: chosen,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.SessionID != chosen {
		t.Fatalf("SessionID = %q, want the daemon's path %q", result.SessionID, chosen)
	}
}

func TestPrimeExecuteCancellationAborts(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	fake := primeFake{sleep: 30, events: primeOKEvents("never")}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithCancel(context.Background())
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: 25 * time.Second})
	if err != nil {
		cancel()
		t.Fatalf("execute: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	result := awaitResult(t, session)

	if result.Status != "aborted" {
		t.Fatalf("status = %q, error = %q; want aborted", result.Status, result.Error)
	}
	if result.ResumeRejected {
		t.Fatal("a cancelled run must not be reported as a refused resume")
	}
}

func TestPrimeExecuteTimeoutIsReportedAsTimeout(t *testing.T) {
	skipIfNoShellFixtures(t)
	isolateHome(t)

	dir := t.TempDir()
	fake := primeFake{sleep: 30}
	backend := primeBackend(t, fake.install(t, dir))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := awaitResult(t, session)

	if result.Status != "timeout" {
		t.Fatalf("status = %q, error = %q; want timeout", result.Status, result.Error)
	}
}

func TestPrimeExecuteMissingBinaryIsNamedInTheError(t *testing.T) {
	t.Parallel()

	backend, err := ResolveBackend("prime", Config{
		ExecutablePath: missingAgentExecutable(t, "prime-agent"),
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("resolve prime backend: %v", err)
	}
	_, err = backend.Execute(context.Background(), "prompt", ExecOptions{})
	if err == nil {
		t.Fatal("a missing binary must fail the execution")
	}
	if !strings.Contains(err.Error(), "prime-agent executable not found") {
		t.Fatalf("error = %v, want the prime-agent provider label", err)
	}
}

// primeObservedHeaderV0_10_0 is the session header line the real prime-agent
// 0.10.0 printed on stdout during the AEGI-639 offline parser probe, copied
// verbatim apart from the id/timestamp/cwd values. It is the one piece of the
// stdout contract this adapter depends on that was read from the binary rather
// than from its source, so the decoder is pinned to it.
const primeObservedHeaderV0_10_0 = `{"type":"session","version":3,"id":"01a16199-79d5-7036-b619-b52619e45e84","timestamp":"2026-10-22T03:13:30.581Z","cwd":"/private/tmp/probe/cwd","rlmDepth":0}`

func TestPrimeSessionHeaderDecodesTheShapeTheBinaryPrinted(t *testing.T) {
	t.Parallel()

	var evt piStreamEvent
	if err := json.Unmarshal([]byte(primeObservedHeaderV0_10_0), &evt); err != nil {
		t.Fatalf("decode observed header: %v", err)
	}
	if evt.Type != piSessionHeaderEvent {
		t.Fatalf("Type = %q, want %q", evt.Type, piSessionHeaderEvent)
	}
	if evt.ID != "01a16199-79d5-7036-b619-b52619e45e84" {
		t.Fatalf("ID = %q, want the id from the observed header", evt.ID)
	}

	// The same line is what a transcript's first line looks like, so the
	// discovery reader must recognise it too.
	path := filepath.Join(t.TempDir(), "observed.jsonl")
	if err := os.WriteFile(path, []byte(primeObservedHeaderV0_10_0+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if got := primeTranscriptHeaderID(path); got != evt.ID {
		t.Fatalf("primeTranscriptHeaderID = %q, want %q", got, evt.ID)
	}
}

// ── Transcript discovery unit coverage ──

func TestFindPrimeSessionTranscript(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, dir, name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return path
	}
	header := func(id string) string {
		raw, err := json.Marshal(map[string]any{"type": "session", "id": id, "cwd": "/tmp"})
		if err != nil {
			t.Fatalf("marshal header: %v", err)
		}
		return string(raw) + "\n"
	}

	t.Run("matches the announced id", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "a.jsonl", header("sess-a"))
		want := write(t, dir, "b.jsonl", header("sess-b"))
		if got := findPrimeSessionTranscript(dir, "sess-b"); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("finds a nested transcript", func(t *testing.T) {
		dir := t.TempDir()
		want := write(t, dir, filepath.Join("project", "c.jsonl"), header("sess-c"))
		if got := findPrimeSessionTranscript(dir, "sess-c"); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("single transcript needs no id", func(t *testing.T) {
		dir := t.TempDir()
		want := write(t, dir, "only.jsonl", header("sess-only"))
		if got := findPrimeSessionTranscript(dir, ""); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("ambiguous without an id resolves to nothing", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "a.jsonl", header("sess-a"))
		write(t, dir, "b.jsonl", header("sess-b"))
		if got := findPrimeSessionTranscript(dir, ""); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("announced id that matches nothing resolves to nothing", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "a.jsonl", header("sess-a"))
		if got := findPrimeSessionTranscript(dir, "sess-missing"); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("non-session jsonl is not a transcript", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "notes.jsonl", `{"type":"note","id":"sess-a"}`+"\n")
		if got := findPrimeSessionTranscript(dir, "sess-a"); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("empty and absent directories resolve to nothing", func(t *testing.T) {
		if got := findPrimeSessionTranscript(t.TempDir(), ""); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
		if got := findPrimeSessionTranscript(filepath.Join(t.TempDir(), "absent"), "sess-a"); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func TestNewPrimeSessionDirIsEmptyAndUnderTheSessionRoot(t *testing.T) {
	home := isolateHome(t)

	first, err := newPrimeSessionDir()
	if err != nil {
		t.Fatalf("newPrimeSessionDir: %v", err)
	}
	second, err := newPrimeSessionDir()
	if err != nil {
		t.Fatalf("newPrimeSessionDir: %v", err)
	}
	if first == second {
		t.Fatal("two conversations shared one session directory; the transcript could not then be identified")
	}
	root := filepath.Join(home, ".multica", "prime-sessions")
	for _, dir := range []string{first, second} {
		if filepath.Dir(dir) != root {
			t.Fatalf("session dir %q is not under %q", dir, root)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		if len(entries) != 0 {
			t.Fatalf("fresh session dir %q is not empty: %v", dir, entries)
		}
	}
}

// ── helpers ──

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func assertArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Fatalf("missing %s %s in %v", flag, value, args)
}
