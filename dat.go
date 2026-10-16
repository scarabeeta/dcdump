package main

// redump.info Dreamcast datfile (serial,version edition): fetched with a conditional
// GET on every run (ETag / Last-Modified), local copy kept as fallback.

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const datURL = "https://redump.info/datfile/dc/serial,version"

type DatTrack struct {
	Name string
	SHA1 string
	Size int64
}

type DatEntry struct {
	Name    string
	ID      string
	Serials []string
	Version string
	Tracks  []DatTrack
}

type DAT struct {
	BySerial map[string][]*DatEntry
	All      []*DatEntry
}

func datPaths() (string, string) {
	h, _ := os.UserHomeDir()
	dir := filepath.Join(h, ".local", "share", "dcdump")
	return filepath.Join(dir, "dc-xml.dat"), filepath.Join(dir, "dc-xml.dat.meta")
}

type datMeta struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Version      string `json:"version,omitempty"` // <header><version> of the datfile
}

var reDatVersion = regexp.MustCompile(`<header>(?s:.*?)<version>([^<]+)</version>`)

func datVersion(b []byte) string {
	if m := reDatVersion.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

func ensureDAT() bool {
	datPath, metaPath := datPaths()
	os.MkdirAll(filepath.Dir(datPath), 0o755)
	var meta datMeta
	if b, err := os.ReadFile(metaPath); err == nil {
		json.Unmarshal(b, &meta)
	}
	_, statErr := os.Stat(datPath)
	haveLocal := statErr == nil
	haveMeta := meta.ETag != "" || meta.LastModified != "" || meta.Version != ""

	action := "Downloading DAT"
	if haveLocal && haveMeta {
		action = "Checking DAT for updates"
	}
	fmt.Printf("%s... ", action)

	req, _ := http.NewRequest("GET", datURL, nil)
	req.Header.Set("User-Agent", "dcdump/1.0")
	if haveLocal && meta.ETag != "" {
		req.Header.Set("If-None-Match", meta.ETag)
	} else if haveLocal && meta.LastModified != "" {
		req.Header.Set("If-Modified-Since", meta.LastModified)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Printf("failed (%v)\n", err)
		return haveLocal
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		fmt.Println("up to date")
		return true
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("failed (HTTP %d)\n", resp.StatusCode)
		return haveLocal
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("failed (%v)\n", err)
		return haveLocal
	}
	dat, err := extractDatFromZip(raw)
	if err != nil {
		dat = raw // tolerate a switch to plain XML
	}
	if err := os.WriteFile(datPath, dat, 0o644); err != nil {
		fmt.Printf("failed (%v)\n", err)
		return haveLocal
	}
	// redump.info sends no ETag/Last-Modified, so the datfile's own version
	// (its date) tells whether anything changed.
	nm := datMeta{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"), Version: datVersion(dat)}
	b, _ := json.Marshal(nm)
	os.WriteFile(metaPath, b, 0o644)
	switch {
	case haveLocal && haveMeta && nm.Version != "" && nm.Version == meta.Version:
		fmt.Printf("up to date (%s)\n", nm.Version)
	case haveLocal && haveMeta:
		fmt.Printf("updated (%s)\n", nm.Version)
	default:
		fmt.Printf("done (%s)\n", nm.Version)
	}
	return true
}

func extractDatFromZip(raw []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	var pick *zip.File
	for _, f := range zr.File {
		if strings.HasSuffix(strings.ToLower(f.Name), ".dat") {
			pick = f
			break
		}
	}
	if pick == nil && len(zr.File) > 0 {
		pick = zr.File[0]
	}
	if pick == nil {
		return nil, errors.New("empty zip")
	}
	rc, err := pick.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

var (
	reGame    = regexp.MustCompile(`(?s)<game\s+name="([^"]+)"([^>]*)>(.*?)</game>`)
	reGameID  = regexp.MustCompile(`\bid="([^"]+)"`)
	reSerial  = regexp.MustCompile(`<serial>([^<]+)</serial>`)
	reVersion = regexp.MustCompile(`<version>([^<]+)</version>`)
	reRom     = regexp.MustCompile(`<rom\s+([^>]+?)/?>`)
	reAttr    = regexp.MustCompile(`\b(name|size|sha1)="([^"]*)"`)
	reParen   = regexp.MustCompile(`\([^)]*\)`)
	reSerSep  = regexp.MustCompile(`[\s\-_]`)
)

// NormalizeSerial: "T-20106M" / "T20106M" / "SCLX-2004 (8848)" → "T20106M" / "SCLX2004".
func NormalizeSerial(s string) string {
	s = reParen.ReplaceAllString(s, "")
	return strings.ToUpper(reSerSep.ReplaceAllString(s, ""))
}

func ParseDAT(text string) *DAT {
	db := &DAT{BySerial: map[string][]*DatEntry{}}
	for _, g := range reGame.FindAllStringSubmatch(text, -1) {
		e := &DatEntry{Name: html.UnescapeString(g[1])}
		if m := reGameID.FindStringSubmatch(g[2]); m != nil {
			e.ID = m[1]
		}
		body := g[3]
		if m := reVersion.FindStringSubmatch(body); m != nil {
			e.Version = strings.TrimSpace(m[1])
		}
		for _, r := range reRom.FindAllStringSubmatch(body, -1) {
			var t DatTrack
			for _, a := range reAttr.FindAllStringSubmatch(r[1], -1) {
				switch a[1] {
				case "name":
					t.Name = html.UnescapeString(a[2])
				case "sha1":
					t.SHA1 = strings.ToLower(a[2])
				case "size":
					fmt.Sscan(a[2], &t.Size)
				}
			}
			if t.Name != "" && len(t.SHA1) == 40 {
				e.Tracks = append(e.Tracks, t)
			}
		}
		if len(e.Tracks) == 0 {
			continue
		}
		db.All = append(db.All, e)
		if m := reSerial.FindStringSubmatch(body); m != nil {
			for _, s := range strings.Split(html.UnescapeString(m[1]), ",") {
				if n := NormalizeSerial(s); n != "" {
					e.Serials = append(e.Serials, strings.TrimSpace(s))
					db.BySerial[n] = append(db.BySerial[n], e)
				}
			}
		}
	}
	return db
}

func LoadDAT() *DAT {
	db := &DAT{BySerial: map[string][]*DatEntry{}}
	if ensureDAT() {
		datPath, _ := datPaths()
		if b, err := os.ReadFile(datPath); err == nil {
			db = ParseDAT(string(b))
		}
		fmt.Printf("  DAT loaded: %d serials (%d games)\n", len(db.BySerial), len(db.All))
	} else {
		fmt.Println("  DAT unavailable, running without serial lookup or SHA-1 verification.")
	}
	fmt.Println()
	return db
}

func (db *DAT) Lookup(serial string) []*DatEntry {
	return db.BySerial[NormalizeSerial(serial)]
}
