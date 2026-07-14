package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"siyuan-worktree/internal/buildinfo"
	"siyuan-worktree/internal/config"
	"siyuan-worktree/internal/siyuan"
	"siyuan-worktree/internal/worktree"
)

var newSiYuanAPI = func(endpoint, token string) siyuan.API {
	return siyuan.NewClient(endpoint, token)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "siyuan-worktree: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printHelp()
		return nil
	}
	switch args[0] {
	case "clone":
		return runClone(args[1:])
	case "init":
		return runInit(args[1:])
	case "pull", "status", "diff", "add", "commit", "reset", "restore", "log", "push":
		return runCommand(args[0], args[1:])
	case "version":
		fmt.Println(buildinfo.Version)
		return nil
	case "help", "-h", "--help":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runCommand(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	root := flags.String("root", ".", "mapping workspace root")
	var all bool
	var staged bool
	var message string
	var force bool
	var ours bool
	var theirs bool
	var continuePush bool
	switch command {
	case "diff":
		flags.BoolVar(&staged, "staged", false, "show staged changes")
	case "add":
		flags.BoolVar(&all, "A", false, "stage all tracked document changes")
	case "commit":
		flags.StringVar(&message, "m", "", "commit message")
	case "reset":
		flags.BoolVar(&force, "force", false, "force reset of an uncertain in-flight commit")
	case "restore":
		flags.BoolVar(&ours, "ours", false, "keep the local side of a conflict")
		flags.BoolVar(&theirs, "theirs", false, "restore the SiYuan side of a conflict")
	case "push":
		flags.BoolVar(&continuePush, "continue", false, "continue an active push operation")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if command == "restore" {
		if ours == theirs {
			return errors.New("restore requires exactly one of --ours or --theirs")
		}
		if flags.NArg() != 1 {
			return errors.New("restore requires exactly one tracked document path")
		}
	}

	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	cfg, err := config.Load(absoluteRoot)
	if err != nil {
		return err
	}
	lock, err := worktree.AcquireWorkspaceLock(absoluteRoot)
	if err != nil {
		return err
	}
	defer lock.Release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	api := newSiYuanAPI(cfg.Endpoint, os.Getenv(cfg.TokenEnv))
	syncer := worktree.NewSyncer(absoluteRoot, cfg, api)

	var result any
	switch command {
	case "pull":
		result, err = syncer.Pull(ctx)
	case "status":
		result, err = syncer.RepositoryStatus(ctx)
	case "diff":
		if staged {
			result, err = syncer.StagedDiffs()
		} else {
			result, err = syncer.Diffs()
		}
	case "add":
		result, err = syncer.Add(ctx, worktree.AddOptions{All: all, Paths: flags.Args()})
	case "commit":
		var commit worktree.Commit
		commit, err = syncer.Commit(message)
		result = worktree.SummarizeCommit(commit)
	case "reset":
		result, err = syncer.Reset(force)
	case "restore":
		strategy := "ours"
		if theirs {
			strategy = "theirs"
		}
		result, err = syncer.Restore(ctx, flags.Arg(0), strategy)
	case "log":
		result, err = syncer.Log()
	case "push":
		if continuePush {
			result, err = syncer.ContinuePush(ctx)
		} else {
			result, err = syncer.Push(ctx)
		}
	}
	if err != nil {
		return err
	}
	return printJSON(result)
}

func runClone(args []string) error {
	cfg := config.Default()
	flags := flag.NewFlagSet("clone", flag.ContinueOnError)
	flags.StringVar(&cfg.OutputDir, "output", cfg.OutputDir, "relative Markdown output directory")
	flags.StringVar(&cfg.TokenEnv, "token-env", cfg.TokenEnv, "environment variable containing the SiYuan API token")
	var notebooks stringList
	flags.Var(&notebooks, "notebook", "notebook ID to map; may be repeated")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() < 1 || flags.NArg() > 2 {
		return errors.New("usage: siyuan-worktree clone [options] URL [DIRECTORY]")
	}

	cfg.Endpoint = strings.TrimRight(flags.Arg(0), "/")
	cfg.NotebookIDs = notebooks
	destination := ""
	if flags.NArg() == 2 {
		destination = flags.Arg(1)
	} else {
		var err error
		destination, err = defaultCloneDirectory(cfg.Endpoint)
		if err != nil {
			return err
		}
	}
	absoluteRoot, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if err := ensureCloneDestination(absoluteRoot); err != nil {
		return err
	}
	if err := config.Init(absoluteRoot, cfg); err != nil {
		return err
	}

	lock, err := worktree.AcquireWorkspaceLock(absoluteRoot)
	if err != nil {
		return err
	}
	defer lock.Release()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	api := newSiYuanAPI(cfg.Endpoint, os.Getenv(cfg.TokenEnv))
	syncer := worktree.NewSyncer(absoluteRoot, cfg, api)
	result, err := syncer.Pull(ctx)
	if err != nil {
		return fmt.Errorf("clone initialized %s but the initial pull failed: %w", absoluteRoot, err)
	}
	fmt.Printf("Cloned %s into %s\n", cfg.Endpoint, absoluteRoot)
	return printJSON(result)
}

func runInit(args []string) error {
	cfg := config.Default()
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	root := flags.String("root", ".", "mapping workspace root")
	flags.StringVar(&cfg.Endpoint, "endpoint", cfg.Endpoint, "SiYuan Kernel endpoint")
	flags.StringVar(&cfg.OutputDir, "output", cfg.OutputDir, "relative Markdown output directory")
	flags.StringVar(&cfg.TokenEnv, "token-env", cfg.TokenEnv, "environment variable containing the SiYuan API token")
	var notebooks stringList
	flags.Var(&notebooks, "notebook", "notebook ID to map; may be repeated")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("init accepts at most one directory")
	}
	if flags.NArg() == 1 {
		if *root != "." {
			return errors.New("specify the init directory either positionally or with -root, not both")
		}
		*root = flags.Arg(0)
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	cfg.NotebookIDs = notebooks
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	if err := config.Init(absoluteRoot, cfg); err != nil {
		return err
	}
	fmt.Printf("Initialized %s\n", absoluteRoot)
	fmt.Printf("Configuration: %s\n", config.Path(absoluteRoot))
	fmt.Printf("Markdown output: %s\n", config.OutputPath(absoluteRoot, cfg))
	return nil
}

func ensureCloneDestination(destination string) error {
	info, err := os.Stat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect clone destination: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("clone destination is not a directory: %s", destination)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		return fmt.Errorf("read clone destination: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("clone destination is not empty: %s", destination)
	}
	return nil
}

func defaultCloneDirectory(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid SiYuan URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("SiYuan URL must be an absolute http or https URL")
	}
	name := path.Base(strings.TrimRight(parsed.Path, "/"))
	if name == "." || name == "/" || name == "" {
		name = parsed.Host
	}
	name = strings.Map(func(character rune) rune {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_' || character == '.' {
			return character
		}
		return '-'
	}, name)
	name = strings.Trim(name, ".-")
	if name == "" {
		name = "siyuan-worktree"
	}
	return name, nil
}

func printJSON(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

type stringList []string

func (s *stringList) String() string {
	return strings.Join(*s, ",")
}

func (s *stringList) Set(value string) error {
	if value == "" {
		return errors.New("notebook ID must not be empty")
	}
	*s = append(*s, value)
	return nil
}

func printHelp() {
	fmt.Print(`siyuan-worktree - Git-style local worktree for SiYuan Kernel

Usage:
  siyuan-worktree clone [options] URL [DIRECTORY]
  siyuan-worktree init [options] [DIRECTORY]
  siyuan-worktree pull [-root DIR]
  siyuan-worktree status [-root DIR]
  siyuan-worktree diff [-root DIR] [--staged]
  siyuan-worktree add [-root DIR] (-A | PATH...)
  siyuan-worktree commit [-root DIR] -m MESSAGE
  siyuan-worktree reset [-root DIR] [--force]
  siyuan-worktree restore [-root DIR] (--ours | --theirs) PATH
  siyuan-worktree log [-root DIR]
  siyuan-worktree push [-root DIR] [--continue]
  siyuan-worktree version

clone initializes a worktree and pulls directly from the SiYuan Kernel URL.
add validates and stages changes, commit freezes them locally, and push writes
only committed changes to SiYuan. status includes recorded conflicts.
`)
}
