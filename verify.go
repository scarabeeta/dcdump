package main

// Post-dump steps, in order: hash → settle the DAT entry by hash (rename) → verify
// → pack submission logs → CRLF cue → GDI → summary. Every mismatch is a failure:
// dcdump's job is a dump whose hashes match redump.

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const sectorRaw = 2352
const hdStartLBA = 45000 // GD-ROM format: first sector of the HD data track

type CueTrack struct {
	Num     int
	File    string
	Audio   bool
	HD      bool // file belongs to the high-density area
	SHA1    string
	Sectors int64
}

var (
	reCueFile  = regexp.MustCompile(`^FILE "(.+)" BINARY`)
	reCueTrack = regexp.MustCompile(`^TRACK (\d+) (AUDIO|MODE(\d)/2352)`)
	reTrackNo  = regexp.MustCompile(`(?i)\(track 0*(\d+)\)`)
)

func ParseCue(text string) []CueTrack {
	var out []CueTrack
	file, hd := "", false
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "REM HIGH-DENSITY AREA") {
			hd = true
		}
		if m := reCueFile.FindStringSubmatch(l); m != nil {
			file = m[1]
			continue
		}
		if m := reCueTrack.FindStringSubmatch(l); m != nil && file != "" {
			n, _ := strconv.Atoi(m[1])
			out = append(out, CueTrack{Num: n, File: file, Audio: m[2] == "AUDIO", HD: hd})
		}
	}
	return out
}

func sha1File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha1.New()
	n, err := io.CopyBuffer(h, f, make([]byte, 1<<20))
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func normTrackName(s string) string {
	return reTrackNo.ReplaceAllStringFunc(strings.ToLower(s), func(m string) string {
		return "(track " + reTrackNo.FindStringSubmatch(m)[1] + ")"
	})
}

func binSHA1s(e *DatEntry) []string {
	var s []string
	for _, t := range e.Tracks {
		if !strings.HasSuffix(strings.ToLower(t.Name), ".cue") {
			s = append(s, t.SHA1)
		}
	}
	sort.Strings(s)
	return s
}

// MatchByHash: the entry whose .bin SHA-1s are exactly the dumped ones; failing
// that, the single entry containing the data track SHA-1 (LD-only dumps).
func MatchByHash(tracks []CueTrack, entries []*DatEntry) *DatEntry {
	var want []string
	for _, t := range tracks {
		want = append(want, t.SHA1)
	}
	sort.Strings(want)
	for _, e := range entries {
		if strings.Join(binSHA1s(e), ",") == strings.Join(want, ",") {
			return e
		}
	}
	var data string
	for _, t := range tracks {
		if !t.Audio {
			data = t.SHA1
			break
		}
	}
	var hits []*DatEntry
	for _, e := range entries {
		for _, s := range binSHA1s(e) {
			if data != "" && s == data {
				hits = append(hits, e)
				break
			}
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return nil
}

// RenameDump renames every <old>* file (patching the name inside .cue/.log) and the
// directory itself when it is named after the dump. Returns the new directory.
func RenameDump(dir, old, new string) string {
	newDir := dir
	if filepath.Base(dir) == old {
		newDir = filepath.Join(filepath.Dir(dir), new)
		if _, err := os.Stat(newDir); err == nil {
			fatal(fmt.Sprintf("Cannot rename %s → %s: target already exists.", dir, newDir))
		}
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), old) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if ext := strings.ToLower(filepath.Ext(p)); ext == ".cue" || ext == ".log" {
			b, _ := os.ReadFile(p)
			os.WriteFile(p, []byte(strings.ReplaceAll(string(b), old, new)), 0o644)
		}
		target := filepath.Join(dir, new+e.Name()[len(old):])
		os.Rename(p, target)
		fmt.Printf("  %s → %s\n", e.Name(), filepath.Base(target))
	}
	if newDir != dir {
		os.Rename(dir, newDir)
		fmt.Printf("  %s → %s\n", dir, newDir)
	}
	return newDir
}

// SettleEntry picks the alternative whose hashes match and renames the dump to it.
func SettleEntry(tracks []CueTrack, dir, fullName string, entry *DatEntry, alts []*DatEntry) (string, string, *DatEntry) {
	if len(alts) < 2 {
		return dir, fullName, entry
	}
	fmt.Println("Resolving DAT entry by track SHA-1...")
	m := MatchByHash(tracks, alts)
	if m == nil {
		fmt.Println("  No entry matches every track — keeping provisional name.")
		fmt.Println()
		return dir, fullName, entry
	}
	fmt.Printf("  Matched: %s\n", m.Name)
	if m.Name != fullName {
		dir = RenameDump(dir, fullName, m.Name)
		for i := range tracks {
			if strings.HasPrefix(tracks[i].File, fullName) {
				tracks[i].File = m.Name + tracks[i].File[len(fullName):]
			}
		}
		fullName = m.Name
	}
	datNames := map[string]string{}
	for _, t := range m.Tracks {
		datNames[normTrackName(t.Name)] = t.Name
	}
	cue := filepath.Join(dir, fullName+".cue")
	for i, t := range tracks {
		if want, ok := datNames[normTrackName(t.File)]; ok && want != t.File {
			os.Rename(filepath.Join(dir, t.File), filepath.Join(dir, want))
			fmt.Printf("  %s → %s\n", t.File, want)
			if b, err := os.ReadFile(cue); err == nil {
				os.WriteFile(cue, []byte(strings.ReplaceAll(string(b), `"`+t.File+`"`, `"`+want+`"`)), 0o644)
			}
			tracks[i].File = want
		}
	}
	fmt.Println()
	return dir, fullName, m
}

// VerifyTracks compares every dumped track and lists DAT tracks that were not
// produced. Returns failure descriptions (empty = all good).
func VerifyTracks(tracks []CueTrack, entry *DatEntry, ldOnly bool) []string {
	if entry == nil {
		return []string{"disc not found in the DAT (no reference hashes)"}
	}
	fmt.Println("Verifying against redump DAT...")
	var fails []string
	dat := map[string]string{}
	for _, t := range entry.Tracks {
		dat[normTrackName(t.Name)] = t.SHA1
	}
	done := map[string]bool{}
	for _, t := range tracks {
		key := normTrackName(t.File)
		done[key] = true
		exp, ok := dat[key]
		switch {
		case !ok:
			fmt.Printf("  Track %02d: no DAT entry for %s\n", t.Num, t.File)
			fails = append(fails, fmt.Sprintf("track %d: not in the DAT entry", t.Num))
		case exp == t.SHA1:
			fmt.Printf("  Track %02d: ✓  %s\n", t.Num, t.SHA1)
		default:
			fmt.Printf("  Track %02d: ✗  %s\n", t.Num, t.SHA1)
			fmt.Printf("           (MISMATCH, expected %s)\n", exp)
			fails = append(fails, fmt.Sprintf("track %d: SHA-1 mismatch", t.Num))
		}
	}
	for _, t := range entry.Tracks {
		key := normTrackName(t.Name)
		if strings.HasSuffix(key, ".cue") || done[key] {
			continue
		}
		if ldOnly {
			fmt.Printf("  %s: not dumped (LD only)\n", t.Name)
			continue
		}
		fmt.Printf("  %s: ✗  missing from the dump\n", t.Name)
		fails = append(fails, t.Name+": missing")
	}
	if len(fails) == 0 {
		fmt.Println("  All tracks OK.")
	}
	fmt.Println()
	return fails
}

var submissionExclude = map[string]bool{".bin": true, ".iso": true, ".scram": true, ".raw": true, ".scm": true, ".sbram": true, ".sdram": true}

// DropStaleTOCFiles removes .atip/.pma/.cdtext files that are byte copies of the .fulltoc or
// .toc file. On Linux, libata returns some ILLEGAL REQUEST errors (e.g. READ TOC format 3-5 on
// drives without PMA/ATIP/CD-TEXT) with SCSI status GOOD plus sense data; redumper checks only
// the status byte, takes the command as successful and stores the stale kernel buffer, i.e. the
// previous FULL TOC response. Returns the names removed.
func DropStaleTOCFiles(dir, fullName string) []string {
	var refs [][]byte
	for _, ext := range []string{".fulltoc", ".toc"} {
		if b, err := os.ReadFile(filepath.Join(dir, fullName+ext)); err == nil && len(b) > 4 {
			refs = append(refs, b)
		}
	}
	var dropped []string
	for _, ext := range []string{".atip", ".pma", ".cdtext"} {
		p := filepath.Join(dir, fullName+ext)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, r := range refs {
			if bytes.Equal(b, r) {
				if os.Remove(p) == nil {
					dropped = append(dropped, filepath.Base(p))
				}
				break
			}
		}
	}
	return dropped
}

func PackSubmissionLogs(dir, fullName string) {
	zipPath := filepath.Join(dir, fullName+"_logs.zip")
	ents, _ := os.ReadDir(dir)
	var files []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, fullName) || submissionExclude[strings.ToLower(filepath.Ext(n))] || filepath.Join(dir, n) == zipPath {
			continue
		}
		files = append(files, n)
	}
	sort.Strings(files)
	if len(files) == 0 {
		fmt.Printf("[WARN] No redumper log files found to pack in %s\n", dir)
		return
	}
	fmt.Println("Packing submission logs...")
	zf, err := os.Create(zipPath)
	if err != nil {
		fatal(err.Error())
	}
	zw := zip.NewWriter(zf)
	for _, n := range files {
		src, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		st, _ := src.Stat()
		hdr, _ := zip.FileInfoHeader(st)
		hdr.Name, hdr.Method = n, zip.Deflate
		w, _ := zw.CreateHeader(hdr)
		io.Copy(w, src)
		src.Close()
		fmt.Printf("  + %s  (%s B)\n", n, commas(st.Size()))
	}
	zw.Close()
	zf.Close()
	st, _ := os.Stat(zipPath)
	fmt.Printf("  → %s  (%s B)\n", filepath.Base(zipPath), commas(st.Size()))
	fmt.Println()
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// FinalizeCue rewrites redumper's LF cue as CRLF (redump's DAT cues are CRLF) and
// checks it against the DAT. Returns a failure description or "".
func FinalizeCue(dir, fullName string, entry *DatEntry, ldOnly bool) string {
	p := filepath.Join(dir, fullName+".cue")
	b, err := os.ReadFile(p)
	if err != nil {
		return "cue missing"
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	os.WriteFile(p, []byte(strings.Join(lines, "\r\n")+"\r\n"), 0o644)
	sum, _, _ := sha1File(p)
	exp := ""
	if entry != nil {
		for _, t := range entry.Tracks {
			if strings.HasSuffix(strings.ToLower(t.Name), ".cue") {
				exp = t.SHA1
			}
		}
	}
	fmt.Println("Checking cue (CRLF) against redump DAT...")
	defer fmt.Println()
	switch {
	case exp == "":
		fmt.Printf("  %s: no DAT entry  %s\n", filepath.Base(p), sum)
	case sum == exp:
		fmt.Printf("  %s: ✓  %s\n", filepath.Base(p), sum)
	case ldOnly:
		fmt.Printf("  %s: (LD only, not comparable)  %s\n", filepath.Base(p), sum)
	default:
		fmt.Printf("  %s: ✗  %s\n", filepath.Base(p), sum)
		fmt.Printf("           (mismatch, expected %s)\n", exp)
		return "cue SHA-1 mismatch"
	}
	return ""
}

// GDIText builds a .gdi from the cue tracks: every track starts at the LBA of the
// first sector of its .bin (the LD audio bin includes its pregap; the HD area
// starts at 45000), so the redump track files are used unchanged.
func GDIText(tracks []CueTrack) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", len(tracks))
	lba, hdSeen := int64(0), false
	for _, t := range tracks {
		if t.HD && !hdSeen {
			lba, hdSeen = hdStartLBA, true
		}
		typ := 4
		if t.Audio {
			typ = 0
		}
		name := t.File
		if strings.ContainsAny(name, " ") {
			name = `"` + name + `"`
		}
		fmt.Fprintf(&b, "%d %d %d 2352 %s 0\n", t.Num, lba, typ, name)
		lba += t.Sectors
	}
	return b.String()
}

func PrintSummary(dir string, tracks []CueTrack, notes []string) {
	fmt.Println(strings.Repeat("━", 48))
	fmt.Printf("Done!  Output: %s\n", dir)
	fmt.Println()
	fmt.Println("SHA-1 summary:")
	for _, t := range tracks {
		fmt.Printf("  Track %02d: %s  %s\n", t.Num, t.SHA1, t.File)
	}
	fmt.Println()
	for _, n := range notes {
		fmt.Println(n)
	}
}
