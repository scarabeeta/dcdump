package main

// HD area: dreamdump (https://codeberg.org/MoriGM/dreamdump), the tool redump's GD-ROM
// dumping guide names for the HD area. The LD area is dumped by redumper as a plain CD
// (redumper.go); this file runs dreamdump into its own subdirectory (it writes
// <name>.cue/.log/.scram/.state/.subq too, which would collide with redumper's), moves the
// HD track files next to the LD ones and merges the two cues into redump's GD-ROM cue.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const dreamdumpSubdir = "dreamdump"

type DreamPlan struct {
	Binary string
	Dir    string
	Args   []string
}

// BuildDreamRun passes the drive parameters explicitly: dreamdump's own drive list only
// knows released firmware revisions, and for those the values are the same.
func BuildDreamRun(d *Drive, o Options, workDir, fullName string) DreamPlan {
	dd := d.Profile.Dreamdump
	dir := filepath.Join(workDir, dreamdumpSubdir)
	args := []string{"disc",
		"--drive=" + d.SG,
		"--image-path=" + dir,
		"--image-name=" + fullName,
		"--sector-order=" + dd.SectorOrder,
		fmt.Sprintf("--read-offset=%d", dd.ReadOffset),
		fmt.Sprintf("--read-at-once=%d", dd.ReadAtOnce),
		"--retries=" + o.Retries,
	}
	return DreamPlan{Binary: "dreamdump", Dir: dir, Args: args}
}

var reDreamBuild = regexp.MustCompile(`dreamdump \(build: ([^)]+)\)`)

// Run executes dreamdump with its output passed through. dreamdump exits 0 on some
// failures (exit_codes.UNKNOWN_DRIVE is 0), so success is judged by CollectHD, not here.
// Returns the build string from its log ("?" if unknown) and an error description.
func (dp *DreamPlan) Run(fullName string) (string, string) {
	path, err := findBinary(dp.Binary)
	if err != nil {
		fatal("dreamdump not found on PATH (needed for the HD area).")
	}
	if err := os.MkdirAll(dp.Dir, 0o755); err != nil {
		fatal(err.Error())
	}
	fmt.Println("Running dreamdump (HD area)...")
	fmt.Printf("  %s %s\n", dp.Binary, strings.Join(dp.Args, " "))
	fmt.Println()
	cmd := exec.Command(path, dp.Args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := cmd.Run()
	fmt.Println()
	build := "?"
	if b, err := os.ReadFile(filepath.Join(dp.Dir, fullName+".log")); err == nil {
		if m := reDreamBuild.FindStringSubmatch(string(b)); m != nil {
			build = m[1]
		}
	}
	if runErr != nil {
		return build, "dreamdump failed: " + runErr.Error()
	}
	return build, ""
}

// CollectHD checks dreamdump's output, moves its HD track files into dir and copies its
// log as <name>.dreamdump.log (packed with the submission logs). Returns the HD part of the
// cue (from "REM HIGH-DENSITY AREA") or an error description.
func CollectHD(dir, fullName string) (string, string) {
	sub := filepath.Join(dir, dreamdumpSubdir)
	cueB, err := os.ReadFile(filepath.Join(sub, fullName+".cue"))
	if err != nil {
		return "", "dreamdump wrote no cue (see its output above)"
	}
	cue := strings.ReplaceAll(string(cueB), "\r\n", "\n")
	i := strings.Index(cue, "REM HIGH-DENSITY AREA")
	if i < 0 {
		return "", "dreamdump cue has no HIGH-DENSITY AREA section"
	}
	hd := cue[i:]
	tracks := ParseCue(hd)
	if len(tracks) == 0 {
		return "", "dreamdump cue lists no tracks"
	}
	for _, t := range tracks {
		src, dst := filepath.Join(sub, t.File), filepath.Join(dir, t.File)
		if _, err := os.Stat(src); err != nil {
			return "", "dreamdump track file missing: " + t.File
		}
		if err := os.Rename(src, dst); err != nil {
			return "", err.Error()
		}
	}
	if b, err := os.ReadFile(filepath.Join(sub, fullName+".log")); err == nil {
		os.WriteFile(filepath.Join(dir, fullName+".dreamdump.log"), b, 0o644)
	}
	return hd, ""
}

// MergeCue builds redump's GD-ROM cue: "REM SINGLE-DENSITY AREA", the LD tracks of
// redumper's cue (from its first FILE line), then dreamdump's HD part. Line endings are
// LF here; FinalizeCue converts to CRLF.
func MergeCue(ldCue, hdCue string) string {
	var b strings.Builder
	b.WriteString("REM SINGLE-DENSITY AREA\n")
	started := false
	sc := bufio.NewScanner(strings.NewReader(strings.ReplaceAll(ldCue, "\r\n", "\n")))
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "FILE ") {
			started = true
		}
		if started && strings.TrimSpace(l) != "" {
			b.WriteString(l + "\n")
		}
	}
	for _, l := range strings.Split(strings.TrimRight(hdCue, "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			b.WriteString(strings.TrimRight(l, " ") + "\n")
		}
	}
	return b.String()
}

// CleanDreamdump removes dreamdump's working directory after a fully verified dump. Its log
// is already kept as <name>.dreamdump.log; the rest (HD .scram, .state, .subq, section
// hashes) is only useful when analysing a failed dump, which keeps the directory.
func CleanDreamdump(dir string) {
	sub := filepath.Join(dir, dreamdumpSubdir)
	if _, err := os.Stat(sub); err != nil {
		return
	}
	if err := os.RemoveAll(sub); err != nil {
		fmt.Printf("  [!] Could not remove %s: %v\n", sub, err)
		return
	}
	fmt.Printf("Removed dreamdump working files (%s/); its log is kept as the .dreamdump.log.\n", dreamdumpSubdir)
}
