package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"nicholas.sh/cmd/review/internal/daemon"
	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/engine"
	"nicholas.sh/cmd/review/internal/skill"
	"nicholas.sh/cmd/review/internal/ui"
	"nicholas.sh/cmd/review/internal/vcs"
)

// Env holds runtime execution dependencies without relying on package globals.
type Env struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	WorkDir string
}

// Main is the standard entry point for the review CLI tool.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "error getting working directory: %v\n", err)
		return 1
	}
	env := &Env{
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
		WorkDir: wd,
	}
	if err := env.Run(args); err != nil {
		fmt.Fprintf(stderr, "review: %v\n", err)
		return 1
	}
	return 0
}

// Run parses arguments and executes the appropriate review subcommand.
func (e *Env) Run(args []string) error {
	if len(args) == 0 {
		return e.cmdDefault(nil)
	}

	sub := args[0]
	subArgs := args[1:]

	switch sub {
	case "stop":
		return e.cmdStop(subArgs)
	case "restart":
		return e.cmdRestart(subArgs)
	case "snapshot":
		return e.cmdSnapshot(subArgs)
	case "snapshots":
		return e.cmdSnapshots(subArgs)
	case "comments":
		return e.cmdComments(subArgs)
	case "add":
		return e.cmdAdd(subArgs)
	case "reply":
		return e.cmdReply(subArgs)
	case "edit":
		return e.cmdEdit(subArgs)
	case "resolve":
		return e.cmdResolve(subArgs, true)
	case "unresolve":
		return e.cmdResolve(subArgs, false)
	case "publish":
		return e.cmdPublish(subArgs)
	case "search":
		return e.cmdSearch(subArgs)
	case "forget":
		return e.cmdForget(subArgs)
	case "gc":
		return e.cmdGC(subArgs)
	case "skill":
		return skill.Run(subArgs, e.Stdout, e.WorkDir)
	case "help", "-h", "--help":
		e.printHelp()
		return nil
	default:
		if strings.HasPrefix(sub, "-") {
			return e.cmdDefault(args)
		}
		return fmt.Errorf("unknown subcommand %q; run 'review help' for usage", sub)
	}
}

func (e *Env) printHelp() {
	fmt.Fprint(e.Stdout, `review - local code review tool for Go code in Jujutsu and Git

Usage:
  review [-a addr] [-n] [-db file]      Launch review server and open browser
  review stop [-db file]                Stop the review server
  review restart [-a addr] [-n] [-db f] Restart the review server
  review snapshot [-nopin] [change...]  Record a review snapshot
  review snapshots [change]             List snapshots for a change
  review comments [flags] [change]      List review comments (-json, -all, -drafts, -s, -c)
  review add [flags] <change> <loc> [t] Add a comment (-from, -side, -s, -draft)
  review reply [flags] <id> [text]      Reply to a thread (-from, -draft, -resolve)
  review edit <msg-id> [text]           Edit a comment message
  review resolve <thread-id>...         Mark threads as resolved
  review unresolve <thread-id>...       Reopen threads
  review publish [change]               Publish draft comments
  review search [-i] <query> [change]   Search inside changed files
  review forget [repo...]               Stop tracking a repository
  review gc [-dry-run] [-keep-missing]  Clean up stale review data and pins
  review skill [-print] [-install]      Print or install agent skill instructions
`)
}

func (e *Env) openDB(dbPath string) (*db.DB, error) {
	p := dbPath
	if p == "" {
		p = db.DefaultPath()
	}
	return db.Open(p)
}

func (e *Env) openRepo(d *db.DB) (vcs.Repo, *db.RepoRecord, func(), error) {
	cache := vcs.NewCache()
	repo, err := vcs.Open(e.WorkDir, cache)
	if err != nil {
		cache.Close()
		return nil, nil, nil, err
	}
	rec, err := d.EnsureRepo(repo.Root())
	if err != nil {
		cache.Close()
		return nil, nil, nil, err
	}
	return repo, rec, cache.Close, nil
}

func resolveChange(repo vcs.Repo, arg string) (*vcs.Change, error) {
	if changes, err := repo.Changes(); err == nil {
		if ch := vcs.ResolveChange(changes, arg); ch != nil {
			return ch, nil
		}
	}
	return repo.Commit(arg)
}

func (e *Env) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) (help bool, err error) {
	err = fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return true, nil
	}
	return false, err
}

func isInteractiveStdin(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func (e *Env) readText(args []string) (string, error) {
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}
	data, err := io.ReadAll(e.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (e *Env) cmdDefault(args []string) error {
	fs := e.newFlagSet("review")

	addrFlag := fs.String("a", "", "Server listen address (e.g. localhost:2626)")
	noBrowser := fs.Bool("n", false, "Do not open browser")
	dbPath := fs.String("db", "", "Database path")
	foreground := fs.Bool("foreground", false, "Run in foreground (internal)")

	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	// Register current repo if inside one
	var repoSlug string
	cmdCache := vcs.NewCache()
	if repo, err := vcs.Open(e.WorkDir, cmdCache); err == nil {
		if rec, err := d.EnsureRepo(repo.Root()); err == nil && rec != nil {
			repoSlug = rec.Slug
		}
	}
	cmdCache.Close()

	configDir := daemon.ConfigDir(d.Path())
	info, _ := daemon.GetServerInfo(configDir)
	running := info != nil && daemon.IsAlive(info.PID)

	if !*foreground && running {
		if *addrFlag != "" && daemon.DialAddr(*addrFlag) != daemon.DialAddr(info.Addr) {
			fmt.Fprintf(e.Stderr, "review: server already running at %s; use 'review restart -a %s' to change address\n", info.Addr, *addrFlag)
		}
		displayHost := daemon.DialAddr(info.Addr)
		serverURL := fmt.Sprintf("http://%s", displayHost)
		if repoSlug != "" {
			serverURL = fmt.Sprintf("http://%s/r/%s", displayHost, url.PathEscape(repoSlug))
		}
		fmt.Fprintf(e.Stdout, "review server already running at %s\n", serverURL)
		if !*noBrowser {
			openBrowser(serverURL)
		}
		return nil
	}

	var configuredAddr string
	if *addrFlag != "" {
		_ = d.SetPref("listen_addr", *addrFlag)
		configuredAddr = *addrFlag
	} else if !running {
		configuredAddr = d.Pref("listen_addr", "")
	}

	listenAddr := configuredAddr
	if listenAddr == "" {
		listenAddr = "localhost:2626"
	}

	if *foreground {
		return e.runServer(d, listenAddr, configuredAddr)
	}

	// Self-start background daemon
	selfBin, err := os.Executable()
	if err != nil {
		// Fallback to running in foreground
		return e.runServer(d, listenAddr, configuredAddr)
	}

	info, err = daemon.StartBackground(selfBin, d.Path(), configuredAddr)
	if err != nil {
		return err
	}

	displayHost := daemon.DialAddr(info.Addr)
	serverURL := fmt.Sprintf("http://%s", displayHost)
	if repoSlug != "" {
		serverURL = fmt.Sprintf("http://%s/r/%s", displayHost, url.PathEscape(repoSlug))
	}
	fmt.Fprintf(e.Stdout, "review server started at %s (pid %d)\n", serverURL, info.PID)
	if !*noBrowser {
		openBrowser(serverURL)
	}
	return nil
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func (e *Env) runServer(d *db.DB, listenAddr, configuredAddr string) error {
	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", listenAddr, err)
	}
	boundAddr := l.Addr().String()
	_, boundPort, _ := net.SplitHostPort(boundAddr)

	uiAddr := ""
	if configuredAddr != "" {
		if reqHost, _, err := net.SplitHostPort(configuredAddr); err == nil && boundPort != "" {
			uiAddr = net.JoinHostPort(reqHost, boundPort)
		} else {
			uiAddr = configuredAddr
		}
	}

	infoAddr := daemon.DialAddr(listenAddr)
	if reqHost, _, err := net.SplitHostPort(infoAddr); err == nil && boundPort != "" {
		infoAddr = net.JoinHostPort(reqHost, boundPort)
	}

	configDir := daemon.ConfigDir(d.Path())
	_ = daemon.WriteServerInfo(configDir, infoAddr, os.Getpid())
	defer func() {
		_ = daemon.RemoveServerInfo(configDir)
	}()

	cache := vcs.NewCache()
	defer cache.Close()

	srv, err := ui.NewServer(d, cache, uiAddr)
	if err != nil {
		_ = l.Close()
		return err
	}

	displayAddr := infoAddr
	if uiAddr != "" {
		displayAddr = uiAddr
	}
	fmt.Fprintf(e.Stdout, "Serving review on http://%s\n", displayAddr)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveDone := make(chan struct{})
	shutDone := make(chan struct{})
	go func() {
		defer close(shutDone)
		select {
		case <-sigCtx.Done():
			shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutCtx)
		case <-serveDone:
		}
	}()

	err = srv.Serve(l)
	close(serveDone)
	<-shutDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (e *Env) cmdStop(args []string) error {
	fs := e.newFlagSet("stop")
	dbPath := fs.String("db", "", "Database path")
	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	configDir := daemon.ConfigDir(*dbPath)
	info, err := daemon.Stop(configDir)
	if err != nil {
		return err
	}
	if info == nil {
		fmt.Fprintln(e.Stdout, "No review server was running.")
	} else {
		fmt.Fprintf(e.Stdout, "Stopped review server (pid %d).\n", info.PID)
	}
	return nil
}

func (e *Env) cmdRestart(args []string) error {
	fs := e.newFlagSet("restart")
	addrFlag := fs.String("a", "", "Server listen address")
	noBrowser := fs.Bool("n", false, "Do not open browser")
	dbPath := fs.String("db", "", "Database path")
	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	configDir := daemon.ConfigDir(*dbPath)
	_, _ = daemon.Stop(configDir)

	var runArgs []string
	if *addrFlag != "" {
		runArgs = append(runArgs, "-a", *addrFlag)
	}
	if *noBrowser {
		runArgs = append(runArgs, "-n")
	}
	if *dbPath != "" {
		runArgs = append(runArgs, "-db", *dbPath)
	}
	return e.cmdDefault(runArgs)
}

func (e *Env) cmdSnapshot(args []string) error {
	fs := e.newFlagSet("snapshot")
	noPin := fs.Bool("nopin", false, "Do not pin revision in VCS")
	dbPath := fs.String("db", "", "Database path")
	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	repo, rec, closeRepo, err := e.openRepo(d)
	if err != nil {
		return err
	}
	defer closeRepo()

	changes, err := repo.Changes()
	if err != nil {
		return err
	}

	targetKeys := fs.Args()
	var targets []*vcs.Change
	if len(targetKeys) == 0 {
		for _, c := range changes {
			if !c.Empty && !c.Working {
				targets = append(targets, c)
			}
		}
		if len(targets) == 0 && len(changes) > 0 {
			for _, c := range changes {
				if !c.Working {
					targets = append(targets, c)
					break
				}
			}
			if len(targets) == 0 {
				targets = append(targets, changes[0])
			}
		}
	} else {
		for _, k := range targetKeys {
			c, err := resolveChange(repo, k)
			if err != nil {
				return fmt.Errorf("resolving change %s: %w", k, err)
			}
			if c == nil {
				return fmt.Errorf("resolving change %s: change not found", k)
			}
			targets = append(targets, c)
		}
	}

	if len(targets) == 0 {
		return fmt.Errorf("no changes found to snapshot")
	}

	for _, c := range targets {
		snap, created, err := engine.TakeSnapshot(d, repo, rec.ID, c, !*noPin)
		if err != nil {
			return err
		}
		if created {
			fmt.Fprintf(e.Stdout, "Recorded Snapshot %d for %s (%s)\n", snap.Num, c.Key, c.Subject)
		} else {
			fmt.Fprintf(e.Stdout, "Snapshot %d is already up to date for %s\n", snap.Num, c.Key)
		}
	}

	changeKey := ""
	if len(targets) == 1 {
		changeKey = targets[0].Key
	}
	daemon.NotifyEvent(d.Path(), "snapshot", rec.Slug, changeKey)
	return nil
}

func (e *Env) cmdSnapshots(args []string) error {
	fs := e.newFlagSet("snapshots")
	dbPath := fs.String("db", "", "Database path")
	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	repo, rec, closeRepo, err := e.openRepo(d)
	if err != nil {
		return err
	}
	defer closeRepo()

	var c *vcs.Change
	if fs.NArg() > 0 {
		cArg := fs.Arg(0)
		var err error
		c, err = resolveChange(repo, cArg)
		if err != nil {
			return fmt.Errorf("resolving change %s: %w", cArg, err)
		}
	} else {
		changes, err := repo.Changes()
		if err != nil {
			return err
		}
		for _, ch := range changes {
			if !(ch.Working && ch.Empty) {
				c = ch
				break
			}
		}
	}

	if c == nil {
		return fmt.Errorf("no change specified")
	}
	changeKey := c.Key

	snaps, err := d.Snapshots(rec.ID, changeKey)
	if err != nil {
		return err
	}
	snaps, _ = engine.SyncLiveRebasedSnapshot(d, repo, rec.ID, c, snaps)

	if len(snaps) == 0 {
		fmt.Fprintf(e.Stdout, "No snapshots recorded for change %s\n", changeKey)
		return nil
	}

	for _, s := range snaps {
		fmt.Fprintf(e.Stdout, "Snapshot %d: rev %s (parent %s) recorded %s\n",
			s.Num, s.Rev, s.ParentRev, s.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func (e *Env) cmdComments(args []string) error {
	fs := e.newFlagSet("comments")
	jsonOutput := fs.Bool("json", false, "Output JSON")
	showAll := fs.Bool("all", false, "Include resolved comments")
	includeDrafts := fs.Bool("drafts", false, "Include unpublished draft comments")
	ctxLines := fs.Int("c", 3, "Context lines around anchored range")
	snapNum := fs.Int("s", -1, "Filter by snapshot number")
	dbPath := fs.String("db", "", "Database path")

	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	repo, rec, closeRepo, err := e.openRepo(d)
	if err != nil {
		return err
	}
	defer closeRepo()

	var targetChanges []*vcs.Change
	if fs.NArg() > 0 {
		cArg := fs.Arg(0)
		c, err := resolveChange(repo, cArg)
		if err != nil {
			return fmt.Errorf("resolving change %s: %w", cArg, err)
		}
		targetChanges = []*vcs.Change{c}
	} else {
		changes, err := repo.Changes()
		if err != nil {
			return err
		}
		// Iterate over pending changes oldest-to-newest so ancestors come before descendants.
		for i := len(changes) - 1; i >= 0; i-- {
			c := changes[i]
			if c.Working && c.Empty {
				continue
			}
			targetChanges = append(targetChanges, c)
		}
	}

	type commentOutput struct {
		ChangeKey      string        `json:"change_key"`
		Commit         string        `json:"commit"`
		Subject        string        `json:"subject"`
		ThreadID       int64         `json:"thread_id"`
		File           string        `json:"file"`
		Side           string        `json:"side"`
		Line           int           `json:"line"`
		EndLine        int           `json:"end_line"`
		StartCol       int           `json:"start_col,omitempty"`
		EndCol         int           `json:"end_col,omitempty"`
		CurrentLine    int           `json:"current_line"`
		CurrentEndLine int           `json:"current_end_line"`
		Stale          bool          `json:"stale"`
		Resolved       bool          `json:"resolved"`
		Context        string        `json:"context,omitempty"`
		Messages       []*db.Message `json:"messages"`
	}

	changeKeys := make([]string, len(targetChanges))
	for i, c := range targetChanges {
		changeKeys[i] = c.Key
		if snaps, sErr := d.Snapshots(rec.ID, c.Key); sErr == nil && len(snaps) > 0 {
			_, _ = engine.SyncLiveRebasedSnapshot(d, repo, rec.ID, c, snaps)
		}
	}
	threadsByChange, err := d.ThreadsForChanges(rec.ID, changeKeys)
	if err != nil {
		return err
	}

	linesCache := make(map[string][]string)
	allOutputs := []commentOutput{}
	for _, c := range targetChanges {
		threads := threadsByChange[c.Key]
		if len(threads) == 0 {
			continue
		}

		var filteredThreads []*db.Thread
		filteredMsgs := make(map[int64][]*db.Message)
		for _, th := range threads {
			if !*showAll && th.Resolved {
				continue
			}
			if *snapNum != -1 && th.SnapshotNum != *snapNum {
				continue
			}

			// Filter draft messages
			var msgs []*db.Message
			for _, m := range th.Messages {
				if m.Draft && !*includeDrafts {
					continue
				}
				msgs = append(msgs, m)
			}
			if len(msgs) == 0 {
				continue
			}

			filteredMsgs[th.ID] = msgs
			filteredThreads = append(filteredThreads, th)
		}
		if len(filteredThreads) == 0 {
			continue
		}

		tvs := engine.ReanchorThreadsForChange(repo, c, nil, c.Rev, c.Parent, filteredThreads, linesCache)

		for _, tv := range tvs {
			th := tv.Thread
			msgs := filteredMsgs[th.ID]

			codeCtx := engine.GetThreadContextCached(repo, tv, *ctxLines, linesCache)

			allOutputs = append(allOutputs, commentOutput{
				ChangeKey:      c.Key,
				Commit:         c.Rev,
				Subject:        c.Subject,
				ThreadID:       th.ID,
				File:           th.Path,
				Side:           th.Side,
				Line:           th.StartLine,
				EndLine:        th.EndLine,
				StartCol:       th.StartCol,
				EndCol:         th.EndCol,
				CurrentLine:    tv.CurrentStart,
				CurrentEndLine: tv.CurrentEnd,
				Stale:          tv.Stale,
				Resolved:       th.Resolved,
				Context:        codeCtx,
				Messages:       msgs,
			})
		}
	}

	if *jsonOutput {
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(allOutputs)
	}

	if len(allOutputs) == 0 {
		fmt.Fprintln(e.Stdout, "No comments found.")
		return nil
	}

	currentChange := ""
	for _, out := range allOutputs {
		if out.ChangeKey != currentChange {
			currentChange = out.ChangeKey
			fmt.Fprintf(e.Stdout, "=== %s: %s ===\n", out.ChangeKey, out.Subject)
		}
		status := "open"
		if out.Resolved {
			status = "resolved"
		}
		staleBadge := ""
		if out.Stale {
			staleBadge = " [STALE]"
		}

		lineSpec := strconv.Itoa(out.CurrentLine)
		if out.CurrentLine == 0 {
			lineSpec = "file-level"
		} else if out.CurrentEndLine > out.CurrentLine {
			lineSpec = fmt.Sprintf("%d-%d", out.CurrentLine, out.CurrentEndLine)
		}

		origSpec := ""
		if out.CurrentLine != out.Line || out.CurrentEndLine != out.EndLine {
			origSpec = fmt.Sprintf(" (originally lines %d-%d on %s)", out.Line, out.EndLine, out.Side)
		} else if out.Side != "" {
			origSpec = fmt.Sprintf(" (%s)", out.Side)
		}

		fmt.Fprintf(e.Stdout, "[thread #%d] %s:%s%s%s [%s]:\n", out.ThreadID, out.File, lineSpec, origSpec, staleBadge, status)
		if out.Context != "" {
			for _, cl := range strings.Split(strings.TrimRight(out.Context, "\n"), "\n") {
				fmt.Fprintf(e.Stdout, "    %s\n", cl)
			}
		}
		for _, m := range out.Messages {
			draftBadge := ""
			if m.Draft {
				draftBadge = " [DRAFT]"
			}
			fmt.Fprintf(e.Stdout, "  [msg #%d] %s%s: %s\n", m.ID, m.Author, draftBadge, m.Body)
		}
		fmt.Fprintln(e.Stdout)
	}

	return nil
}

func (e *Env) cmdAdd(args []string) error {
	fs := e.newFlagSet("add")
	fromFlag := fs.String("from", "", "Comment author name (default reviewer)")
	sideFlag := fs.String("side", "R", "Diff side (R or L)")
	draftFlag := fs.Bool("draft", false, "Save comment as unpublished draft")
	dbPath := fs.String("db", "", "Database path")
	snapFlag := fs.Int("s", 0, "Snapshot number")

	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	if fs.NArg() < 2 {
		return fmt.Errorf("usage: review add [-from author] [-side R|L] [-s snap] [-draft] <change> <file[:line[-end]]> [text]")
	}

	changeArg := fs.Arg(0)
	targetSpec := fs.Arg(1)
	filePath, startLine, endLine, err := parseFileSpec(targetSpec)
	if err != nil {
		return err
	}

	side := strings.ToUpper(strings.TrimSpace(*sideFlag))
	if side != "L" && side != "R" {
		return fmt.Errorf("invalid -side %q: must be L or R", *sideFlag)
	}

	text, err := e.readText(fs.Args()[2:])
	if err != nil {
		return err
	}
	if text == "" {
		return fmt.Errorf("comment text cannot be empty")
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	repo, rec, closeRepo, err := e.openRepo(d)
	if err != nil {
		return err
	}
	defer closeRepo()

	c, err := resolveChange(repo, changeArg)
	if err != nil {
		return fmt.Errorf("resolving change %s: %w", changeArg, err)
	}

	author := *fromFlag
	if author == "" {
		author = repo.User()
	}
	if author == "" {
		author = "reviewer"
	}

	if vcs.IsCommitMsgPath(filePath) {
		filePath = vcs.CommitMsgPath
		side = "R"
	}

	rev := ""
	targetRev := ""
	if *snapFlag > 0 {
		if snaps, sErr := d.Snapshots(rec.ID, c.Key); sErr == nil && len(snaps) > 0 {
			_, _ = engine.SyncLiveRebasedSnapshot(d, repo, rec.ID, c, snaps)
		}
		snap, err := d.SnapshotByNum(rec.ID, c.Key, *snapFlag)
		if err != nil {
			return fmt.Errorf("finding snapshot %d: %w", *snapFlag, err)
		}
		if snap == nil {
			return fmt.Errorf("snapshot %d not found for change %s", *snapFlag, c.Key)
		}
		targetRev = snap.Rev
		if side == "L" {
			rev = snap.ParentRev
		} else {
			rev = snap.Rev
		}
	}

	th, msg, err := engine.AddCommentRangeWithTarget(d, repo, rec.ID, c, rev, targetRev, filePath, side, startLine, endLine, 0, 0, author, text, *draftFlag, false)
	if err != nil {
		return err
	}

	daemon.NotifyEvent(d.Path(), "comment_add", rec.Slug, c.Key)
	lineDesc := fmt.Sprintf("%d", startLine)
	if startLine == 0 {
		lineDesc = "file-level"
	} else if endLine > startLine {
		lineDesc = fmt.Sprintf("%d-%d", startLine, endLine)
	}
	draftDesc := ""
	if *draftFlag {
		draftDesc = " (draft)"
	}
	fmt.Fprintf(e.Stdout, "Added comment thread #%d [msg #%d] on %s:%s%s\n", th.ID, msg.ID, filePath, lineDesc, draftDesc)
	return nil
}

func parseFileSpec(spec string) (path string, startLine, endLine int, err error) {
	idx := strings.LastIndexByte(spec, ':')
	if idx == -1 {
		return spec, 0, 0, nil
	}
	linePart := spec[idx+1:]
	rangeParts := strings.Split(linePart, "-")
	n1, err1 := strconv.Atoi(rangeParts[0])
	if err1 != nil {
		// Treat non-numeric suffixes (e.g. colons in file names) as file-level specs
		// unless they look like malformed numeric ranges.
		if len(rangeParts) == 1 && (linePart == "" || (linePart[0] < '0' || linePart[0] > '9')) {
			return spec, 0, 0, nil
		}
		return "", 0, 0, fmt.Errorf("invalid line specification in %q", spec)
	}
	if n1 <= 0 {
		return "", 0, 0, fmt.Errorf("invalid line number %d in %q: must be positive", n1, spec)
	}
	if len(rangeParts) > 2 {
		return "", 0, 0, fmt.Errorf("invalid line range in %q", spec)
	}
	path = spec[:idx]
	startLine = n1
	endLine = n1
	if len(rangeParts) == 2 {
		n2, err2 := strconv.Atoi(rangeParts[1])
		if err2 != nil || n2 < startLine {
			return "", 0, 0, fmt.Errorf("invalid line range in %q", spec)
		}
		endLine = n2
	}
	return path, startLine, endLine, nil
}

func (e *Env) cmdReply(args []string) error {
	fs := e.newFlagSet("reply")
	fromFlag := fs.String("from", "agent", "Reply author name")
	draftFlag := fs.Bool("draft", false, "Save reply as draft")
	resolveFlag := fs.Bool("resolve", false, "Mark thread resolved")
	dbPath := fs.String("db", "", "Database path")

	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: review reply [-from author] [-draft] [-resolve] <thread-id> [text]")
	}

	threadID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid thread ID %q: %w", fs.Arg(0), err)
	}

	var text string
	textArgs := fs.Args()[1:]
	if len(textArgs) > 0 || !*resolveFlag || !isInteractiveStdin(e.Stdin) {
		text, err = e.readText(textArgs)
		if err != nil {
			return err
		}
	}
	if text == "" && !*resolveFlag {
		return fmt.Errorf("reply text required (or use -resolve)")
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	if text != "" {
		author := *fromFlag
		if author == "" {
			author = "agent"
		}

		msg, err := d.AddMessage(threadID, author, text, *draftFlag)
		if err != nil {
			return err
		}
		draftDesc := ""
		if *draftFlag {
			draftDesc = " (draft)"
		}
		fmt.Fprintf(e.Stdout, "Added reply [msg #%d] to thread #%d%s\n", msg.ID, threadID, draftDesc)
	}

	if *resolveFlag {
		if err := d.SetResolved(threadID, true); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "Marked thread #%d as resolved.\n", threadID)
	}

	reason := "comment_reply"
	if text == "" {
		reason = "resolve"
	}
	var repoSlug, changeKey string
	if th, err := d.ThreadByID(threadID); err == nil && th != nil {
		changeKey = th.ChangeKey
		if rec, err := d.RepoByID(th.RepoID); err == nil && rec != nil {
			repoSlug = rec.Slug
		}
	}
	daemon.NotifyEvent(d.Path(), reason, repoSlug, changeKey)
	return nil
}

func (e *Env) cmdEdit(args []string) error {
	fs := e.newFlagSet("edit")
	dbPath := fs.String("db", "", "Database path")

	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: review edit <message-id> [text]")
	}

	msgID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid message ID %q: %w", fs.Arg(0), err)
	}

	text, err := e.readText(fs.Args()[1:])
	if err != nil {
		return err
	}
	if text == "" {
		return fmt.Errorf("message text cannot be empty")
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	msg, th, err := d.EditMessage(msgID, text)
	if err != nil {
		return err
	}

	var repoSlug, changeKey string
	if th != nil {
		changeKey = th.ChangeKey
		if rec, err := d.RepoByID(th.RepoID); err == nil && rec != nil {
			repoSlug = rec.Slug
		}
	}
	daemon.NotifyEvent(d.Path(), "comment_edit", repoSlug, changeKey)
	fmt.Fprintf(e.Stdout, "Updated message #%d: %s\n", msg.ID, msg.Body)
	return nil
}

func (e *Env) cmdResolve(args []string, resolve bool) error {
	name := "resolve"
	if !resolve {
		name = "unresolve"
	}
	fs := e.newFlagSet(name)
	dbPath := fs.String("db", "", "Database path")

	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	if fs.NArg() == 0 {
		return fmt.Errorf("usage: review %s <thread-id>...", name)
	}

	ids := make([]int64, 0, fs.NArg())
	for _, arg := range fs.Args() {
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid thread ID %q: %w", arg, err)
		}
		ids = append(ids, id)
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	var notifyRepo, notifyChange string
	var multipleChanges bool
	for _, id := range ids {
		if err := d.SetResolved(id, resolve); err != nil {
			return err
		}
		if th, err := d.ThreadByID(id); err == nil && th != nil {
			if rec, err := d.RepoByID(th.RepoID); err == nil && rec != nil {
				if notifyRepo == "" {
					notifyRepo = rec.Slug
					notifyChange = th.ChangeKey
				} else if notifyRepo != rec.Slug || notifyChange != th.ChangeKey {
					multipleChanges = true
				}
			}
		}
		if resolve {
			fmt.Fprintf(e.Stdout, "Resolved thread #%d\n", id)
		} else {
			fmt.Fprintf(e.Stdout, "Reopened thread #%d\n", id)
		}
	}

	if multipleChanges {
		notifyRepo, notifyChange = "", ""
	}
	daemon.NotifyEvent(d.Path(), "resolve", notifyRepo, notifyChange)
	return nil
}

func (e *Env) cmdPublish(args []string) error {
	fs := e.newFlagSet("publish")
	dbPath := fs.String("db", "", "Database path")

	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	repo, rec, closeRepo, err := e.openRepo(d)
	if err != nil {
		return err
	}
	defer closeRepo()

	var res *engine.PublishResult
	publishChange := ""
	if fs.NArg() > 0 {
		changeArg := fs.Arg(0)
		key := changeArg
		c, err := resolveChange(repo, changeArg)
		if err == nil && c != nil {
			key = c.Key
		} else {
			c = nil
		}
		publishChange = key
		res, err = engine.PublishChange(d, repo, rec.ID, key, c, true)
		if err != nil {
			return err
		}
	} else {
		res, err = engine.PublishRepo(d, repo, rec.ID, true)
		if err != nil {
			return err
		}
	}

	daemon.NotifyEvent(d.Path(), "publish", rec.Slug, publishChange)
	fmt.Fprintf(e.Stdout, "Published %d draft comments.\n", res.Published)
	return nil
}

func (e *Env) cmdSearch(args []string) error {
	fs := e.newFlagSet("search")
	ciFlag := fs.Bool("i", false, "Case-insensitive search")
	_ = fs.String("db", "", "Database path (unused)")

	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: review search [-i] <query> [change]")
	}

	query := fs.Arg(0)
	cache := vcs.NewCache()
	defer cache.Close()
	repo, err := vcs.Open(e.WorkDir, cache)
	if err != nil {
		return err
	}

	var target *vcs.Change
	if fs.NArg() > 1 {
		cArg := fs.Arg(1)
		var err error
		target, err = resolveChange(repo, cArg)
		if err != nil {
			return fmt.Errorf("resolving change %s: %w", cArg, err)
		}
	} else {
		changes, err := repo.Changes()
		if err != nil {
			return err
		}
		for _, c := range changes {
			if !(c.Working && c.Empty) {
				target = c
				break
			}
		}
	}
	if target == nil {
		return fmt.Errorf("no matching change found")
	}

	results, err := engine.SearchChange(repo, target, target.Rev, target.Parent, query, *ciFlag)
	if err != nil {
		return err
	}

	if len(results) == 0 {
		fmt.Fprintln(e.Stdout, "No matches found.")
		return nil
	}

	for _, r := range results {
		fmt.Fprintf(e.Stdout, "%s:\n", r.Path)
		for _, m := range r.Matches {
			fmt.Fprintf(e.Stdout, "  line %d: %s\n", m.Line, m.Snippet)
		}
	}
	return nil
}

func (e *Env) cmdForget(args []string) error {
	fs := e.newFlagSet("forget")
	dbPath := fs.String("db", "", "Database path")

	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	var targets []string
	if fs.NArg() > 0 {
		targets = fs.Args()
	} else {
		target := e.WorkDir
		cache := vcs.NewCache()
		if repo, err := vcs.Open(e.WorkDir, cache); err == nil {
			target = repo.Root()
		}
		cache.Close()
		targets = append(targets, target)
	}

	forgotten := 0
	for _, t := range targets {
		rec, err := d.ForgetRepo(t)
		if err != nil {
			return err
		}
		if rec == nil && (t == "." || t == ".." || strings.ContainsAny(t, `/\`)) {
			candidate := t
			if !filepath.IsAbs(candidate) {
				candidate = filepath.Join(e.WorkDir, candidate)
			}
			candidate = filepath.Clean(candidate)
			cache := vcs.NewCache()
			if repo, err := vcs.Open(candidate, cache); err == nil {
				candidate = repo.Root()
			}
			cache.Close()
			if candidate != t {
				rec, err = d.ForgetRepo(candidate)
				if err != nil {
					return err
				}
			}
		}
		if rec != nil {
			forgotten++
			fmt.Fprintf(e.Stdout, "Forgot repository %s (%s)\n", rec.Slug, rec.RootPath)
		} else {
			fmt.Fprintf(e.Stdout, "Repository %s was not tracked.\n", t)
		}
	}

	if forgotten > 0 {
		daemon.NotifyEvent(d.Path(), "repo_forget", "", "")
	}
	return nil
}

func (e *Env) cmdGC(args []string) error {
	fs := e.newFlagSet("gc")
	dryRun := fs.Bool("dry-run", false, "Print what would be cleaned up without modifying anything")
	keepMissing := fs.Bool("keep-missing", false, "Do not forget repositories whose directory no longer exists")
	dbPath := fs.String("db", "", "Database path")

	if help, err := parseFlags(fs, args); err != nil || help {
		return err
	}

	d, err := e.openDB(*dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	repos, err := d.Repos()
	if err != nil {
		return err
	}

	report, err := engine.RunGC(d, repos, *dryRun, *keepMissing)
	if err != nil {
		return err
	}

	prefix := ""
	if *dryRun {
		prefix = "[dry-run] "
	}

	for _, r := range report.ForgottenRepos {
		fmt.Fprintf(e.Stdout, "%sForgot missing repo: %s\n", prefix, r)
	}
	for _, c := range report.PrunedChanges {
		fmt.Fprintf(e.Stdout, "%sPruned change data: %s\n", prefix, c)
	}
	for _, s := range report.PrunedSnapshots {
		fmt.Fprintf(e.Stdout, "%sPruned redundant snapshot: %s\n", prefix, s)
	}
	for _, p := range report.PrunedPins {
		fmt.Fprintf(e.Stdout, "%sPruned VCS pin: %s\n", prefix, p)
	}

	if len(report.ForgottenRepos) == 0 && len(report.PrunedChanges) == 0 && len(report.PrunedSnapshots) == 0 && len(report.PrunedPins) == 0 {
		fmt.Fprintf(e.Stdout, "%sNothing to clean up.\n", prefix)
	} else if !*dryRun {
		daemon.Notify(d.Path())
	}

	return nil
}
