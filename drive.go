package main

// Drive enumeration (/proc/scsi/scsi plus sysfs),
// the drive list (which drives can read the GD-ROM HD area, and how), and selection.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed drives.json
var builtinDrives []byte

type Profile struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	Vendor      string            `json:"vendor"`
	Model       string            `json:"model"`
	Rev         string            `json:"rev"`
	GD          bool              `json:"gd"`
	ReadOffset  *int              `json:"read_offset"`
	Speed       int               `json:"speed"`
	SectorOrder map[string]string `json:"sector_order"`
	HDFirstLBA  int               `json:"hd_first_lba"`
	Dreamdump   *DreamParams      `json:"dreamdump"`

	reVendor, reModel, reRev *regexp.Regexp
}

// DreamParams are the dreamdump options for the HD area (passed explicitly).
type DreamParams struct {
	SectorOrder string `json:"sector_order"`
	ReadOffset  int    `json:"read_offset"`
	ReadAtOnce  int    `json:"read_at_once"`
}

func (p *Profile) compile() error {
	var err error
	for _, x := range []struct {
		src string
		dst **regexp.Regexp
	}{{p.Vendor, &p.reVendor}, {p.Model, &p.reModel}, {p.Rev, &p.reRev}} {
		if *x.dst, err = regexp.Compile("(?i)" + x.src); err != nil {
			return fmt.Errorf("drive list entry %q: %v", p.ID, err)
		}
	}
	if p.GD && (p.HDFirstLBA == 0 || p.Dreamdump == nil || p.Dreamdump.SectorOrder == "" || p.Dreamdump.ReadAtOnce == 0) {
		return fmt.Errorf("drive list entry %q: gd entries need hd_first_lba and dreamdump {sector_order, read_offset, read_at_once}", p.ID)
	}
	return nil
}

func (p *Profile) Matches(d *Drive) bool {
	return p.reVendor.MatchString(d.Vendor) && p.reModel.MatchString(d.Model) && p.reRev.MatchString(d.Rev)
}

// LoadProfiles returns the user's list (~/.config/dcdump/drives.json) followed by the
// built-in one; the first matching entry wins, so user entries override.
func LoadProfiles() ([]*Profile, error) {
	var all []*Profile
	userPath := filepath.Join(userConfigDir(), "dcdump", "drives.json")
	if b, err := os.ReadFile(userPath); err == nil {
		var u []*Profile
		if err := json.Unmarshal(b, &u); err != nil {
			return nil, fmt.Errorf("%s: %v", userPath, err)
		}
		all = append(all, u...)
	}
	var b []*Profile
	if err := json.Unmarshal(builtinDrives, &b); err != nil {
		return nil, err
	}
	all = append(all, b...)
	for _, p := range all {
		if err := p.compile(); err != nil {
			return nil, err
		}
	}
	return all, nil
}

func userConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config")
}

type Drive struct {
	Vendor, Model, Rev string
	Host               string // SCSI host number
	SG, SR             string
	Transport          string // "USB", "SATA" or "other"
	Profile            *Profile
}

func (d *Drive) Label() string {
	s := strings.TrimSpace(d.Vendor + " " + d.Model)
	if d.Rev != "" {
		s += " rev:" + d.Rev
	}
	s += "  →  " + d.SG
	if d.SR != "" {
		s += "  (" + d.SR + ")"
	}
	return s
}

func (d *Drive) Capability() string {
	switch {
	case d.Profile == nil:
		return "not in drive list"
	case d.Profile.GD:
		return "GD"
	default:
		return "LD only"
	}
}

var (
	reHostBlock = regexp.MustCompile(`(?m)^Host: scsi(\d+)`)
	reVendor    = regexp.MustCompile(`Vendor:\s+(\S+)`)
	reModel     = regexp.MustCompile(`Model:\s+(.+?)\s+Rev:`)
	reRev       = regexp.MustCompile(`Rev:\s*(\S+)`)
)

func GetCDDrives(profiles []*Profile) []*Drive {
	text, err := os.ReadFile("/proc/scsi/scsi")
	if err != nil {
		return nil
	}
	idx := reHostBlock.FindAllStringSubmatchIndex(string(text), -1)
	var drives []*Drive
	for i, m := range idx {
		end := len(text)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		block := string(text[m[0]:end])
		if !strings.Contains(block, "CD-ROM") {
			continue
		}
		d := &Drive{Host: string(text[m[2]:m[3]]), Vendor: "?", Model: "?"}
		if v := reVendor.FindStringSubmatch(block); v != nil {
			d.Vendor = v[1]
		}
		if v := reModel.FindStringSubmatch(block); v != nil {
			d.Model = strings.TrimSpace(v[1])
		}
		if v := reRev.FindStringSubmatch(block); v != nil {
			d.Rev = strings.TrimSpace(v[1])
		}
		base := "/sys/class/scsi_device/" + d.Host + ":0:0:0/device/"
		d.SG = firstDev(base+"scsi_generic/", "sg")
		d.SR = firstDev(base+"block/", "sr")
		if d.SG == "" {
			continue
		}
		d.Transport = transportOf(base)
		for _, p := range profiles {
			if p.Matches(d) {
				d.Profile = p
				break
			}
		}
		drives = append(drives, d)
	}
	return drives
}

func firstDev(dir, prefix string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return "/dev/" + names[0]
}

func transportOf(sysDevice string) string {
	p, err := filepath.EvalSymlinks(sysDevice)
	if err != nil {
		return "other"
	}
	switch {
	case strings.Contains(p, "/usb"):
		return "USB"
	case strings.Contains(p, "/ata"):
		return "SATA"
	}
	return "other"
}

// SelectDrive resolves --drive (index or vendor/model substring)
// resolves directly; one usable drive is taken without a prompt; several prompt.
// Only GD-capable drives are usable unless ldOnly; forceID assigns a drive-list entry
// to a drive that does not match it.
func SelectDrive(auto bool, driveArg string, ldOnly bool, forceID string, profiles []*Profile) *Drive {
	drives := GetCDDrives(profiles)
	if len(drives) == 0 {
		fatal("No CD drives found.")
	}
	var forced *Profile
	if forceID != "" {
		for _, p := range profiles {
			if p.ID == forceID {
				forced = p
				break
			}
		}
		if forced == nil {
			fatal(fmt.Sprintf("--force-drive %q is not an entry of the drive list.", forceID))
		}
	}
	usable := func(d *Drive) bool {
		return ldOnly || forced != nil || (d.Profile != nil && d.Profile.GD)
	}

	fmt.Println("Detected drives:")
	labels := make([]string, len(drives))
	for i, d := range drives {
		labels[i] = fmt.Sprintf("%s  [%s, %s]", d.Label(), d.Transport, d.Capability())
	}

	idx := -1
	switch {
	case driveArg != "":
		if n, err := strconv.Atoi(driveArg); err == nil {
			if n < 1 || n > len(drives) {
				fatal(fmt.Sprintf("--drive index %s out of range (1-%d).", driveArg, len(drives)))
			}
			idx = n - 1
		} else {
			for i, d := range drives {
				if strings.Contains(strings.ToUpper(d.Vendor+" "+d.Model), strings.ToUpper(driveArg)) {
					idx = i
					break
				}
			}
			if idx < 0 {
				fatal(fmt.Sprintf("--drive %q matched no detected drive.", driveArg))
			}
		}
		printList(labels, idx)
	default:
		var cand []int
		for i, d := range drives {
			if usable(d) {
				cand = append(cand, i)
			}
		}
		switch {
		case len(cand) == 0:
			printList(labels, -1)
			fatal("No GD-capable drive found (see the drive list). Use --ld-only to dump the LD area only.")
		case len(cand) == 1: // the only usable drive: no real choice
			idx = cand[0]
			printList(labels, idx)
		default:
			idx = choose("Select drive", labels, cand[0])
		}
	}
	d := drives[idx]
	if !usable(d) {
		fatal(fmt.Sprintf("%s is not a GD-capable drive (%s). Use --ld-only to dump the LD area only, or --force-drive ID to test it with a drive-list entry.",
			strings.TrimSpace(d.Vendor+" "+d.Model+" "+d.Rev), d.Capability()))
	}
	if forced != nil {
		d.Profile = forced
		fmt.Println()
		box([]string{
			fmt.Sprintf("--force-drive: using drive-list entry %q for this drive.", forced.ID),
			"The drive does not match that entry; results are untested.",
		})
	}
	fmt.Printf("  Using: %s\n", d.SG)
	return d
}

func printList(labels []string, sel int) {
	for i, l := range labels {
		m := ""
		if i == sel {
			m = " ←"
		}
		fmt.Printf("  [%d] %s%s\n", i+1, l, m)
	}
}
