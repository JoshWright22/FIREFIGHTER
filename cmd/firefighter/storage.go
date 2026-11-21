package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/storage"
)

const storageUsage = `usage:
  firefighter storage [--json]                          the allotment, every space and what it holds
  firefighter storage claim --gb N [--why text] [--project P]  claim, grow or shrink your project's space; prints its folder
  firefighter storage path [--project P]                your project's folder
  firefighter storage label <path> --rank keep|useful|scratch|none --for "what uses it, until when"
                                                        say what a file or folder in your space is for and how much it matters
  firefighter storage items [--json] [--project P]      your space's contents, least important first
  firefighter storage free --gb N [--upto scratch|unranked|useful] [--dry-run]
                                                        delete the least important items until N GB is free (keep is never deleted)
  firefighter storage release [--delete] [--project P]  give the space back (--delete removes its files)
Changing another project's space (claim, label, free, release) takes --force.
The human sets the allotment: firefighter config set storage.gb <GB> (folder: storage.root,
per-project most: storage.projects.<name>).`

// storageCmd: agents manage the disk space the human allots them.
func storageCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, home := storage.FromConfig(cfg), config.Home()
	sub := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("storage "+sub, flag.ContinueOnError)
	project := fs.String("project", "", "project (default: the git repo or folder name)")
	gb := fs.Float64("gb", 0, "size of the space, GB")
	why := fs.String("why", "", "what it holds")
	del := fs.Bool("delete", false, "release: delete the space's files")
	rank := fs.String("rank", "", "label: keep | useful | scratch | none")
	purpose := fs.String("for", "", "label: what uses it, and until when")
	upto := fs.String("upto", "scratch", "free: delete up to this rank (scratch, unranked, useful)")
	dry := fs.Bool("dry-run", false, "free: only say what would be deleted")
	force := fs.Bool("force", false, "change another project's space")
	asJSON := fs.Bool("json", false, "show: JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := ""
	if fs.NArg() > 0 && sub == "label" { // `storage label data/raw --rank keep`: the path
		target = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	} else if fs.NArg() > 0 { // `storage claim myproj --gb 5` names the project too
		*project = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if *project == "" {
		*project = callerProject()
	}
	*project = storage.Name(*project) // "My Project" -> "My-Project": the same name its jobs are matched by
	// Another project's space is that project's: claiming, ranking, freeing or releasing it takes
	// --force (as cancelling another project's job does), so a mistyped --project can't delete
	// someone else's work.
	if own := storage.Name(callerProject()); !strings.EqualFold(*project, own) && !*force {
		switch sub {
		case "claim", "label", "free", "release":
			return fmt.Errorf("%s is another project's space (you are in %s): pass --force if you mean it", *project, own)
		}
	}
	switch sub {
	case "show", "status", "ls":
		r, err := pool.Look(home)
		if err != nil {
			return err
		}
		if *asJSON {
			b, _ := json.MarshalIndent(r, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		printStorage(r)
	case "claim":
		if *gb <= 0 {
			return errors.New("usage: firefighter storage claim --gb N [--why text] [--project P]")
		}
		dir, err := pool.Claim(home, *project, *gb, *why)
		if err != nil {
			return err
		}
		saved := *gb
		if g, err := storage.Grants(home); err == nil {
			saved = g[filepath.Base(dir)].GB // what was stored (rounded down to 0.1 GB)
		}
		fmt.Printf("%s: %.1f GB space at %s\n", *project, saved, dir)
		fmt.Println("do: keep the project's big files (data, checkpoints, models, caches) there, delete what you no longer need, `firefighter storage release` when done")
	case "path":
		dir, err := pool.Dir(*project)
		if err != nil {
			return err
		}
		g, err := storage.Grants(home)
		if err != nil {
			return err
		}
		for name := range g {
			if strings.EqualFold(name, *project) {
				fmt.Println(filepath.Join(filepath.Dir(dir), name))
				return nil
			}
		}
		return fmt.Errorf("%s has no space yet: firefighter storage claim --gb N --project %s", *project, *project)
	case "label":
		if target == "" || *rank == "" {
			return errors.New(`usage: firefighter storage label <path> --rank keep|useful|scratch|none --for "what uses it, until when"`)
		}
		rel, err := pool.SetLabel(home, *project, target, *rank, *purpose)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s is %s (%s)\n", *project, rel, *rank, *purpose)
	case "items":
		its, err := pool.Items(home, *project)
		if err != nil {
			return err
		}
		if *asJSON {
			b, _ := json.MarshalIndent(its, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		if len(its) == 0 {
			fmt.Printf("%s's space is empty (or has none)\n", *project)
		}
		for _, it := range its {
			why := it.For
			if it.Rank == storage.Unranked {
				why = "nobody said: firefighter storage label " + it.Path + " --rank ... --for ..."
			}
			fmt.Printf("  %-8s %8.2f GB %6d files  %s  %-30s %s\n", it.Rank, it.GB, it.Files, it.Modified.Format("2006-01-02"), it.Path, why)
		}
	case "free":
		if *gb <= 0 {
			return errors.New("usage: firefighter storage free --gb N [--upto scratch|unranked|useful] [--dry-run]")
		}
		chosen, freed, err := pool.Free(home, *project, *gb, *upto, *dry)
		if err != nil {
			return err
		}
		verb := "deleted"
		if *dry {
			verb = "would delete"
		}
		for _, it := range chosen {
			if it.Left > 0 {
				fmt.Printf("  %d file(s) of %q are in use and were kept", it.Left, it.Path)
				fmt.Println()
			}
			fmt.Printf("  %s %q (%s, %.2f GB)\n", verb, it.Path, it.Rank, it.GB)
		}
		fmt.Printf("%s: %s %.2f GB of the %.1f GB asked", *project, verb, freed, *gb)
		if freed < *gb {
			fmt.Printf("; nothing else ranked %s or lower: delete by hand, rank more as scratch, or pass --upto unranked/useful", *upto)
		}
		fmt.Println()
	case "release":
		freed, err := pool.Release(home, *project, *del)
		if err != nil {
			return err
		}
		fmt.Printf("%s: space released (%.1f GB of files deleted)\n", *project, freed)
	default:
		fmt.Fprintln(os.Stderr, storageUsage)
		return fmt.Errorf("unknown storage action %q", sub)
	}
	return nil
}

func printStorage(r storage.Report) {
	if r.GB <= 0 {
		fmt.Println("no storage allotted to agents (the human: firefighter config set storage.gb <GB>)")
	} else {
		fmt.Printf("agent storage: %.1f GB allotted at %s; %.1f GB claimed, %.1f GB unclaimed; %.1f GB in use\n",
			r.GB, r.Root, r.GrantedGB, max(0, r.GB-r.GrantedGB), r.UsedGB)
	}
	if r.FloorGB > 0 || r.DriveFree > 0 {
		fmt.Printf("  drive: %.1f GB free, %.0f GB of it kept for the human\n", r.DriveFree, r.FloorGB)
	}
	for _, s := range r.Spaces {
		state := ""
		switch {
		case s.GB == 0:
			state = "  UNCLAIMED: claim it or delete it"
		case s.Over:
			state = fmt.Sprintf("  OVER by %.1f GB: delete files or claim more", s.UsedGB-s.GB)
		}
		why := ""
		if s.Why != "" {
			why = "  (" + s.Why + ")"
		}
		fmt.Printf("  %-20s %7.1f / %6.1f GB  %6d files  %s%s%s\n", s.Project, s.UsedGB, s.GB, s.Files, s.Path, why, state)
		if s.UsedGB > 0 {
			fmt.Printf("  %-20s keep %.1f, useful %.1f, unranked %.1f, scratch %.1f GB\n", "", s.RankGB[storage.Keep], s.RankGB[storage.Useful], s.RankGB[storage.Unranked], s.RankGB[storage.Scratch])
		}
	}
}
